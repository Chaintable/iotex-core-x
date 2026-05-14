package api

import (
	"fmt"
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
// a single replay-side log, keyed by EVM LogIndex (action.Log.Index).
type eventBinding struct {
	parentTraceID string
	position      int64
	id            string
}

// extractLogIndexToBinding builds the (LogIndex -> binding) map from replay-side
// events. callTracer.addTraceAndLog (pipeline/tracer/call_tracer.go:332) writes
// each EVM log to BlockFile.Events / ErrorEvents after filling ParentTraceID +
// Position + ID; we just copy those out keyed by LogIndex so canonical-event
// rebuild can re-bind via action.Log.Index.
func extractLogIndexToBinding(events, errEvents []ptypes.Event) map[uint32]eventBinding {
	m := make(map[uint32]eventBinding, len(events)+len(errEvents))
	for _, evs := range [][]ptypes.Event{events, errEvents} {
		for _, e := range evs {
			if e.LogIndex < 0 {
				continue
			}
			m[uint32(e.LogIndex)] = eventBinding{
				parentTraceID: e.ParentTraceID,
				position:      e.Position,
				id:            e.ID,
			}
		}
	}
	return m
}

// extractRootTraceByTx builds the (tx_id -> root trace id) map. Used as a
// fallback when an event's LogIndex isn't in logIndexToBinding (drift case or
// synthetic TransactionLog not emitted by EVM).
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
// LogIndex is monotonically increasing within the block (matches geth's
// eth_getBlockReceipts numbering).
//
// Trace-attribution fields (ParentTraceID / Position / ID) are filled with a
// two-tier strategy:
//  1. precise: action.Log.Index keys into logIndexToBinding -> copy callTracer's
//     pre-computed frame binding (same hash algo as call_tracer.go:340 so values
//     match replay exactly).
//  2. fallback: synthetic TransactionLog OR drift case (replay missed a log) ->
//     bind to the tx's root trace id via rootTraceByTx; position uses b.logIdx
//     (block-global counter) so colliding hashes within one tx are impossible.
//  3. no-binding: non-EVM action (GrantReward / staking) has no root trace
//     because callTracer never opened a frame for it; leave the three fields at
//     zero value + warn.
type canonicalEventBuilder struct {
	events            []ptypes.Event
	logIdx            int64
	logIndexToBinding map[uint32]eventBinding
	rootTraceByTx     map[string]string
	height            uint64
}

// buildCanonicalEvents constructs the full block-level events list directly from
// historical receipts, then re-binds each event to a trace frame (precise) or
// the tx root trace (fallback) using the maps caller built from replay output.
//
// txIDs is the parallel list of canonical tx hashes (eth-tx-style hex with 0x prefix);
// must match `receipts` 1:1 by index.
//
// Both maps are optional (nil OK): nil degrades to the pre-binding behavior
// where the three attribution fields are left empty.
func buildCanonicalEvents(
	receipts []*action.Receipt,
	txIDs []string,
	logIndexToBinding map[uint32]eventBinding,
	rootTraceByTx map[string]string,
	height uint64,
) []ptypes.Event {
	if len(receipts) != len(txIDs) {
		return []ptypes.Event{}
	}
	b := &canonicalEventBuilder{
		events:            make([]ptypes.Event, 0),
		logIndexToBinding: logIndexToBinding,
		rootTraceByTx:     rootTraceByTx,
		height:            height,
	}
	for i, r := range receipts {
		if r == nil {
			continue
		}
		b.appendFromReceipt(r, txIDs[i])
	}
	return b.events
}

// appendFromReceipt converts one receipt's EVM logs + synthetic TransactionLogs to events.
func (b *canonicalEventBuilder) appendFromReceipt(r *action.Receipt, txID string) {
	// EVM logs (from LOG opcodes inside contract execution).
	for _, l := range r.Logs() {
		ev := ptypes.Event{
			Data:     hexutil.Bytes(l.Data),
			LogIndex: b.logIdx,
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
		// Try precise binding via action.Log.Index (== callTracer's LogIndex).
		if binding, ok := b.logIndexToBinding[l.Index]; ok {
			ev.ParentTraceID = binding.parentTraceID
			ev.Position = binding.position
			ev.ID = binding.id
		} else {
			b.applyFallbackBinding(&ev, txID)
		}
		b.events = append(b.events, ev)
		b.logIdx++
	}

	// Synthetic logs from native iotex flows (GAS_FEE / GRANT_REWARD / CLAIM /
	// BUCKET_CREATE_AMOUNT / IN_CONTRACT_TRANSFER ...). Never in EVM execution,
	// so logIndexToBinding never has them — always go through fallback.
	if len(r.TransactionLogs()) == 0 {
		return
	}
	transferLogs, err := r.TransferLogs(account.ProtocolAddr().String(), 0)
	if err != nil {
		return
	}
	for _, l := range transferLogs {
		ev := ptypes.Event{
			Data:     hexutil.Bytes(l.Data),
			LogIndex: b.logIdx,
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
		b.applyFallbackBinding(&ev, txID)
		b.events = append(b.events, ev)
		b.logIdx++
	}
}

// applyFallbackBinding fills ParentTraceID / Position / ID using the tx's root
// trace. If the tx has no root trace (non-EVM action), the three fields are
// left at zero value + a warn is logged so downstream knows the event isn't
// frame-bound. Position uses b.logIdx (block-global) to guarantee uniqueness
// of the (rootTraceID, position) hash input across all events in the block.
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
