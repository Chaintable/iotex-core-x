package api

import (
	"math/big"

	ptypes "github.com/Chaintable/pipeline/types"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

// overrideTxsAndStripDivergedTraces overrides per-tx canonical fields on
// BlockFile.Txs (Status + GasUsed) from the historical receipt.
//
// HISTORICAL NOTE: an earlier version of this function additionally stripped
// the full call tree of any tx whose replay-side status diverged from the
// canonical receipt, then synthesized a single-frame "canonical-minimal"
// replacement trace. We removed the strip step because:
//
//  1. Replay/canonical status divergence is an iotex internal quirk (the
//     EVM re-runs the action under slightly different conditions than the
//     historical block); divergence does NOT mean the replay's CALL graph
//     is invented. The sub-frames, addresses, inputs, gas all describe
//     real EVM activity that did happen at trace time, even if the final
//     status bit ended up different.
//  2. Dropping the call tree caused downstream verification to lose
//     legitimate frame and event data (block 48182127 / 48193234 in the
//     production data: one reverted-but-status-mismatched tx had its
//     30-frame call tree replaced by a single placeholder, taking its
//     receipt's ccd3d8 synthetic transfer logs down with it because
//     ErrorEvents drop was indexed by dropped trace IDs).
//  3. The tx-level fields that actually matter (Status / GasUsed) are
//     already authoritative — they come straight from the canonical
//     receipt below. That's enough to make downstream "did this tx
//     succeed?" + "how much gas did this tx burn?" answers correct.
//
// So we now: (a) override Status + GasUsed on every tx with canonical
// values; (b) keep the replay's full trace tree and events untouched.
// Status divergence is still counted via the replayDivergedTxsTotal
// metric in coreservice.go for visibility; this function returns the
// count of diverged txs so the caller can keep logging it.
//
// Receipt lookup is by tx.ID (= "0x" + action_hash hex), NOT by index —
// out.BlockFile.Txs only contains eth-compatible actions while receipts
// covers ALL actions including non-EVM (GrantReward / staking / etc.).
// Indexing receipts[i] against Txs[i] would silently misalign on any
// block with native actions interleaved.
func overrideTxsAndStripDivergedTraces(
	out *ptypes.DebankOutPut,
	receiptByTxID map[string]*action.Receipt,
) int {
	if out == nil || out.BlockFile == nil {
		return 0
	}

	divergedCount := 0
	for i := range out.BlockFile.Txs {
		tx := &out.BlockFile.Txs[i]
		hr := receiptByTxID[tx.ID]
		if hr == nil {
			// No canonical receipt for this tx (shouldn't happen for valid
			// blocks; the dao receipts are exhaustive). Defensive skip.
			continue
		}
		canonicalSuccess := hr.Status == uint64(iotextypes.ReceiptStatus_Success)
		if tx.Status != canonicalSuccess {
			divergedCount++
		}
		// Always override — replay's gas can drift slightly even on
		// status-matched txs.
		tx.Status = canonicalSuccess
		tx.GasUsed = big.NewInt(int64(hr.GasConsumed))
	}

	return divergedCount
}
