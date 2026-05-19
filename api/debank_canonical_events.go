package api

import (
	"fmt"
	"sort"
	"strings"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/Chaintable/pipeline/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	iotexAddress "github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol/account"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"go.uber.org/zap"
)

// eventBinding carries the trace-attribution fields callTracer pre-computed for
// a single replay-side log.
type eventBinding struct {
	parentTraceID string
	position      int64
	id            string
}

// bindingKey uniquely identifies a log within a block by (tx_id, in-tx position).
//
// We deliberately do NOT use the global LogIndex as the binding key, because
// iotex's receipt-side `r.Logs()[i].Index` (assigned by updateReceiptIndex /
// handler emission order) and callTracer's flush-side counter can diverge:
// e.g. when a Claim/Reward action's receipt log is allocated an Index that
// falls between an earlier Execution action's EVM logs and that same action's
// synthetic transfer logs, the two sides cannot be reconciled by a single
// block-global counter.
//
// (tx_id, in_tx_log_idx) is stable across both sides:
//   - replay side: callTracer.OnLog stamps each Event with `InTxLogIdx`
//     (per-tx counter reset on OnTxStart).
//   - canonical side: each receipt's logs iterate in handler-emit order, so
//     the i-th log within a receipt corresponds to the i-th `OnLog` callTracer
//     observed for the same tx.
type bindingKey struct {
	txID    string
	inTxIdx int64
}

// extractBindingByTxPos builds the ((tx_id, in_tx_log_idx) -> binding) map
// from replay-side events. callTracer.addTraceAndLog (pipeline/tracer/
// call_tracer.go) writes each EVM log to BlockFile.Events / ErrorEvents after
// filling ParentTraceID + Position + ID + InTxLogIdx; we look up the enclosing
// tx via the trace tree (every Trace carries its TxID) and key by
// (TxID, InTxLogIdx).
func extractBindingByTxPos(
	events, errEvents []ptypes.Event,
	traces, errTraces []ptypes.Trace,
) map[bindingKey]eventBinding {
	// All traces from one callTracer share the same TxID (callTracer is per-tx;
	// see pipeline tracer ToTrace).
	traceIDToTxID := make(map[string]string, len(traces)+len(errTraces))
	for _, ts := range [][]ptypes.Trace{traces, errTraces} {
		for _, t := range ts {
			if t.ID != "" {
				traceIDToTxID[t.ID] = t.TxID
			}
		}
	}
	m := make(map[bindingKey]eventBinding, len(events)+len(errEvents))
	for _, evs := range [][]ptypes.Event{events, errEvents} {
		for _, e := range evs {
			txID := traceIDToTxID[e.ParentTraceID]
			if txID == "" {
				continue
			}
			m[bindingKey{txID: txID, inTxIdx: e.InTxLogIdx}] = eventBinding{
				parentTraceID: e.ParentTraceID,
				position:      e.Position,
				id:            e.ID,
			}
		}
	}
	return m
}

// extractRootTraceByTx builds the (tx_id -> root trace id) map. Used as a
// fallback when an event has no precise (txID, inTxIdx) binding in
// `bindingByTxPos` (synthetic TransactionLog without a paired callTracer log,
// or replay-side drift).
func extractRootTraceByTx(traces, errTraces []ptypes.Trace) map[string]string {
	m := make(map[string]string)
	for _, ts := range [][]ptypes.Trace{traces, errTraces} {
		for _, t := range ts {
			if len(t.TraceAddress) == 0 && t.TxID != "" && t.ID != "" {
				// keep the first root trace seen per tx
				if _, ok := m[t.TxID]; !ok {
					m[t.TxID] = t.ID
				}
			}
		}
	}
	return m
}

// canonicalEventBuilder accumulates events across all receipts in a block.
//
// Each event's LogIndex is the iotex-side `action.Log.Index` (matches what
// eth_getTransactionReceipt exposes downstream), NOT a builder-internal counter.
//
// Trace-attribution fields (ParentTraceID / Position / ID) are filled with a
// two-tier strategy:
//  1. precise: (txID, in-tx ordinal) keys into bindingByTxPos -> copy
//     callTracer's pre-computed frame binding.
//  2. fallback: synthetic TransactionLog OR drift case -> bind to the tx's
//     root trace id via rootTraceByTx; Position uses b.logIdx (block-global)
//     so colliding hashes within one tx are impossible.
//  3. no-binding: non-EVM action with no root trace -> leave three fields
//     at zero value + warn.
type canonicalEventBuilder struct {
	events         []ptypes.Event
	logIdx         int64
	bindingByTxPos map[bindingKey]eventBinding
	rootTraceByTx  map[string]string
	height         uint64
}

// buildCanonicalEvents constructs the full block-level events list directly from
// historical receipts, then re-binds each event to a trace frame (precise) or
// the tx root trace (fallback) using the maps caller built from replay output.
//
// txIDs is the parallel list of canonical tx hashes (eth-tx-style hex with 0x
// prefix); must match `receipts` 1:1 by index.
//
// Both maps are optional (nil OK): nil degrades to the pre-binding behavior
// where the three attribution fields are left empty.
//
// LogIndex allocation mirrors eth_getTransactionReceipt's algorithm exactly
// (see web3server.go:getTransactionReceipt): EVM logs use the Index already
// stamped by updateReceiptIndex; synthetic TransferLogs (converted from
// TransactionLogs) start at (block_total_EVM_logs + sum_of_earlier_tx_txLogs),
// so they slot in after all real EVM logs and after any earlier tx's
// synthetic logs.
func buildCanonicalEvents(
	receipts []*action.Receipt,
	txIDs []string,
	bindingByTxPos map[bindingKey]eventBinding,
	rootTraceByTx map[string]string,
	height uint64,
) []ptypes.Event {
	if len(receipts) != len(txIDs) {
		return []ptypes.Event{}
	}

	// Pre-compute total EVM logs in the block. Synthetic transferLogs of any tx
	// start their Index space at this offset (matches web3server algorithm).
	var totalEVMLogs uint32
	for _, r := range receipts {
		if r != nil {
			totalEVMLogs += uint32(len(r.Logs()))
		}
	}

	b := &canonicalEventBuilder{
		events:         make([]ptypes.Event, 0),
		bindingByTxPos: bindingByTxPos,
		rootTraceByTx:  rootTraceByTx,
		height:         height,
	}
	var priorTxLogs uint32
	for i, r := range receipts {
		if r == nil {
			continue
		}
		transferLogStartIdx := totalEVMLogs + priorTxLogs
		b.appendFromReceipt(r, txIDs[i], transferLogStartIdx)
		priorTxLogs += uint32(len(r.TransactionLogs()))
	}

	// Sort events by LogIndex so the array order matches eth_getLogs and any
	// downstream consumer that walks events in ascending logIndex (instead of
	// in tx-then-receipt-section insertion order, which is non-monotonic when
	// synthetic transferLogs of an earlier tx have a higher LogIndex than EVM
	// logs of a later tx — see block 48125704).
	sort.SliceStable(b.events, func(i, j int) bool {
		return b.events[i].LogIndex < b.events[j].LogIndex
	})
	return b.events
}

// appendFromReceipt converts one receipt's EVM logs + synthetic TransactionLogs
// to events. The per-receipt `inTxPos` counter mirrors callTracer's
// `inTxLogIdx`: i-th log within this receipt corresponds to the i-th
// callTracer.OnLog call for the same tx, so (txID, inTxPos) recovers the
// replay-side binding.
//
// transferLogStartIdx is the LogIndex to assign to this receipt's first
// synthetic transferLog (computed at block level by buildCanonicalEvents to
// match eth_getTransactionReceipt's allocation).
func (b *canonicalEventBuilder) appendFromReceipt(r *action.Receipt, txID string, transferLogStartIdx uint32) {
	var inTxPos int64

	// EVM logs (from LOG opcodes inside contract execution).
	for _, l := range r.Logs() {
		ev := ptypes.Event{
			Data:     hexutil.Bytes(l.Data),
			LogIndex: int64(l.Index),
		}
		if addr, err := iotexAddress.FromString(l.Address); err == nil {
			ev.Address = strings.ToLower(common.BytesToAddress(addr.Bytes()).Hex())
		}
		if len(l.Topics) > 0 {
			ev.Selector = strings.ToLower(common.BytesToHash(l.Topics[0][:]).Hex())
			if len(l.Topics) > 1 {
				ev.Topics = make([]string, 0, len(l.Topics)-1)
				for _, t := range l.Topics[1:] {
					ev.Topics = append(ev.Topics, strings.ToLower(common.BytesToHash(t[:]).Hex()))
				}
			}
		}
		if binding, ok := b.bindingByTxPos[bindingKey{txID: txID, inTxIdx: inTxPos}]; ok {
			ev.ParentTraceID = binding.parentTraceID
			ev.Position = binding.position
			ev.ID = binding.id
		} else {
			b.applyFallbackBinding(&ev, txID)
		}
		b.events = append(b.events, ev)
		b.logIdx++
		inTxPos++
	}

	// Synthetic logs from native iotex flows (GAS_FEE / GRANT_REWARD / CLAIM /
	// BUCKET_CREATE_AMOUNT / IN_CONTRACT_TRANSFER ...). For non-Execution
	// actions where the EVM never runs, these are also surfaced via
	// EmitTransferLogs -> callTracer.OnLog, so they may carry a precise
	// binding under (txID, inTxPos). Lookup first; fall back if missed.
	if len(r.TransactionLogs()) == 0 {
		return
	}
	// Pass the block-level starting LogIndex so r.TransferLogs assigns
	// Index = transferLogStartIdx, transferLogStartIdx+1, ... matching the
	// numbering returned by eth_getTransactionReceipt for this tx.
	transferLogs, err := r.TransferLogs(account.ProtocolAddr().String(), transferLogStartIdx)
	if err != nil {
		return
	}
	for _, l := range transferLogs {
		ev := ptypes.Event{
			Data:     hexutil.Bytes(l.Data),
			LogIndex: int64(l.Index),
		}
		if addr, err := iotexAddress.FromString(l.Address); err == nil {
			ev.Address = strings.ToLower(common.BytesToAddress(addr.Bytes()).Hex())
		}
		if len(l.Topics) > 0 {
			ev.Selector = strings.ToLower(common.BytesToHash(l.Topics[0][:]).Hex())
			if len(l.Topics) > 1 {
				ev.Topics = make([]string, 0, len(l.Topics)-1)
				for _, t := range l.Topics[1:] {
					ev.Topics = append(ev.Topics, strings.ToLower(common.BytesToHash(t[:]).Hex()))
				}
			}
		}
		if binding, ok := b.bindingByTxPos[bindingKey{txID: txID, inTxIdx: inTxPos}]; ok {
			ev.ParentTraceID = binding.parentTraceID
			ev.Position = binding.position
			ev.ID = binding.id
		} else {
			b.applyFallbackBinding(&ev, txID)
		}
		b.events = append(b.events, ev)
		b.logIdx++
		inTxPos++
	}
}

// applyFallbackBinding fills ParentTraceID / Position / ID using the tx's root
// trace. If the tx has no root trace (non-EVM action without callTracer
// coverage), the three fields are left at zero value + a warn is logged so
// downstream knows the event isn't frame-bound. Position uses b.logIdx
// (block-global) to guarantee uniqueness of the (rootTraceID, position) hash
// input across all events in the block.
func (b *canonicalEventBuilder) applyFallbackBinding(ev *ptypes.Event, txID string) {
	rootID := b.rootTraceByTx[txID]
	if rootID == "" {
		log.L().Warn("trace_debankBlock: event left unbound, no root trace",
			zap.Uint64("height", b.height), zap.String("tx", txID))
		return
	}
	ev.ParentTraceID = rootID
	ev.Position = b.logIdx
	ev.ID = util.ToHash([]string{rootID, fmt.Sprintf("%d", b.logIdx)})
}
