package api

import (
	"math/big"
	"strings"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/Chaintable/pipeline/util"
	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

// overrideTxsAndStripDivergedTraces enforces the rule that block_file fields shipped
// to S3 / Kafka must reflect canonical (main-chain) truth, not replay's potentially-
// diverged execution. Concretely:
//
//  1. tx.Status and tx.GasUsed: ALWAYS overridden from the historical canonical receipt
//     (replay's receipt may differ even when status agrees — small gas drift exists in
//     post-Sumatra blocks too, e.g., 47,487,940's 0xa576c141 case).
//
//  2. For txs where replay status DIVERGES from canonical:
//     a. Drop replay's traces / error_traces / error_events for that tx — they describe
//        a call tree that didn't actually run on chain.
//     b. Synthesize a single-frame "canonical-minimal" trace as replacement: tx-level
//        from/to/value/input/gas/status all canonical-correct, no internal call tree.
//        Routed into Traces or ErrorTraces depending on canonical status. This preserves
//        the "every tx has at least one trace" invariant downstream may rely on, without
//        shipping replay's wrong call structure.
//
// Status-matched txs keep their full replay trace tree as best-effort. Internal frame-
// level drift (gas attribution, sub-call status, non-LOG-emitting CALL value) is NOT
// detected here — it's a documented limitation of trace correctness without real
// canonical traces. State-correctness (state_diff/events) is unaffected by trace drift.
//
// Returns the count of diverged txs.
func overrideTxsAndStripDivergedTraces(
	out *ptypes.DebankOutPut,
	receipts []*action.Receipt,
) int {
	if out == nil || out.BlockFile == nil {
		return 0
	}

	// First pass: override tx.Status / tx.GasUsed always; collect (txID → receipt) for
	// diverged txs.
	type divergedEntry struct {
		txIdx   int
		receipt *action.Receipt
	}
	diverged := make(map[string]divergedEntry) // txID → entry

	for i, hr := range receipts {
		if i >= len(out.BlockFile.Txs) || hr == nil {
			continue
		}
		tx := &out.BlockFile.Txs[i]
		canonicalSuccess := hr.Status == uint64(iotextypes.ReceiptStatus_Success)
		if tx.Status != canonicalSuccess {
			diverged[tx.ID] = divergedEntry{txIdx: i, receipt: hr}
		}
		// Always override — replay's gas can drift slightly even on status-matched txs.
		tx.Status = canonicalSuccess
		tx.GasUsed = big.NewInt(int64(hr.GasConsumed))
	}

	if len(diverged) == 0 {
		return 0
	}

	// Drop diverged-tx traces from BOTH success/fail buckets, and collect the IDs of
	// the dropped traces so we can also strip orphan error_events.
	divergedTxIDs := make(map[string]struct{}, len(diverged))
	for id := range diverged {
		divergedTxIDs[id] = struct{}{}
	}
	droppedTraceIDs := make(map[string]struct{})
	out.BlockFile.Traces = dropTracesByTxIDs(out.BlockFile.Traces, divergedTxIDs, droppedTraceIDs)
	out.BlockFile.ErrorTraces = dropTracesByTxIDs(out.BlockFile.ErrorTraces, divergedTxIDs, droppedTraceIDs)
	out.BlockFile.ErrorEvents = dropEventsByParentTrace(out.BlockFile.ErrorEvents, droppedTraceIDs)

	// Second pass: synthesize a canonical-minimal trace per diverged tx and append to
	// the appropriate bucket. Routed by canonical status (now reflected in tx.Status).
	for _, e := range diverged {
		tx := out.BlockFile.Txs[e.txIdx]
		synth := synthesizeMinimalTraceFromCanonical(tx, e.receipt)
		if tx.Status {
			out.BlockFile.Traces = append(out.BlockFile.Traces, synth)
		} else {
			out.BlockFile.ErrorTraces = append(out.BlockFile.ErrorTraces, synth)
		}
	}

	return len(diverged)
}

// synthesizeMinimalTraceFromCanonical builds a single-frame trace from the canonical
// tx + receipt. No internal calls (Subtraces=0), no Output (iotex receipts don't
// canonically expose return data), but all top-level fields (from / to / value / input /
// gas / gasUsed / status / error) are canonically correct.
//
// Mirrors the genesis-path trace synthesis in api_debank.go (single-frame "call" trace
// with empty parent and PosInParentTrace=0).
func synthesizeMinimalTraceFromCanonical(tx ptypes.Transaction, receipt *action.Receipt) ptypes.Trace {
	callCreate := "call"
	callType := "call"
	to := tx.To
	if to == "" || strings.EqualFold(to, "0x") {
		callCreate = "create"
		callType = ""
	}

	var errMsg string
	if receipt.Status != uint64(iotextypes.ReceiptStatus_Success) {
		errMsg = "execution reverted"
		if revert := receipt.ExecutionRevertMsg(); revert != "" {
			errMsg = "execution reverted: " + revert
		}
	}

	gas := tx.Gas
	if gas == nil {
		gas = big.NewInt(0)
	}
	gasUsed := tx.GasUsed
	if gasUsed == nil {
		gasUsed = big.NewInt(0)
	}

	return ptypes.Trace{
		ID:                util.ToHash([]string{tx.ID, "", "0"}),
		From:              tx.From,
		To:                to,
		Gas:               gas,
		GasUsed:           gasUsed,
		Value:             tx.Value,
		Input:             tx.Input,
		Output:            hexutil.Bytes{},
		CallCreateType:    callCreate,
		CallType:          callType,
		TxID:              tx.ID,
		ParentTraceID:     "",
		PosInParentTrace:  0,
		SelfStorageChange: false,
		StorageChange:     false,
		Subtraces:         0,
		TraceAddress:      []int64{},
		Error:             errMsg,
	}
}

// dropTracesByTxIDs returns `traces` with entries whose TxID ∈ divergedTxIDs removed,
// and inserts the dropped traces' IDs into `outDroppedIDs` for caller use.
func dropTracesByTxIDs(traces []ptypes.Trace, divergedTxIDs map[string]struct{}, outDroppedIDs map[string]struct{}) []ptypes.Trace {
	if len(traces) == 0 || len(divergedTxIDs) == 0 {
		return traces
	}
	kept := traces[:0]
	for _, t := range traces {
		if _, diverged := divergedTxIDs[t.TxID]; diverged {
			outDroppedIDs[t.ID] = struct{}{}
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

// dropEventsByParentTrace returns `events` with entries whose ParentTraceID ∈
// droppedTraceIDs removed.
func dropEventsByParentTrace(events []ptypes.Event, droppedTraceIDs map[string]struct{}) []ptypes.Event {
	if len(events) == 0 || len(droppedTraceIDs) == 0 {
		return events
	}
	kept := events[:0]
	for _, e := range events {
		if _, dropped := droppedTraceIDs[e.ParentTraceID]; dropped {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}
