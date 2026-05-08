package api

import (
	"strings"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"

	iotexAddress "github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol/account"
)

// canonicalEventBuilder accumulates events across all receipts in a block.
//
// LogIndex is monotonically increasing within the block (matches geth's
// eth_getBlockReceipts numbering). The trace-attribution fields (ParentTraceID,
// Position, ID) are intentionally LEFT EMPTY here — leafage's RPC layer
// (crates/leafage-evm-rpc/src/api_impl/utils.rs:build_trace_node) re-derives them
// from its own freshly-replayed Reth CallTraceNode tree when serving
// `eth_getDebankBlock`, overwriting whatever the writer sent. So filling them on
// the writer side is wasted work + could mislead readers into thinking the
// bindings reflect actual call-frame attribution.
type canonicalEventBuilder struct {
	events []ptypes.Event
	logIdx int64
}

// buildCanonicalEvents constructs the full block-level events list directly from
// historical receipts. Address / Topics / Data / LogIndex are populated; trace
// attribution fields are left at Go zero values (downstream re-fills).
//
// txIDs is the parallel list of canonical tx hashes (eth-tx-style hex with 0x prefix);
// must match `receipts` 1:1 by index. Currently unused but kept for forward-compat
// (downstream may want a tx_id for non-frame-bound events in future).
func buildCanonicalEvents(receipts []*action.Receipt, txIDs []string) []ptypes.Event {
	if len(receipts) != len(txIDs) {
		return []ptypes.Event{}
	}
	b := &canonicalEventBuilder{events: make([]ptypes.Event, 0)}
	for _, r := range receipts {
		if r == nil {
			continue
		}
		b.appendFromReceipt(r)
	}
	return b.events
}

// appendFromReceipt converts one receipt's EVM logs + synthetic TransactionLogs to events.
//
// Both kinds are emitted unconditionally — canonical receipts already represent the
// "main-chain truth" so the old replay-side includeEVMLogs gate is gone.
func (b *canonicalEventBuilder) appendFromReceipt(r *action.Receipt) {
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
		b.events = append(b.events, ev)
		b.logIdx++
	}

	// Synthetic logs from native iotex flows (GAS_FEE / GRANT_REWARD / CLAIM /
	// BUCKET_CREATE_AMOUNT / IN_CONTRACT_TRANSFER ...).
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
		b.events = append(b.events, ev)
		b.logIdx++
	}
}
