package api

import (
	"encoding/hex"
	"math/big"
	"testing"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/action"
)

func mkPipelineTx(id string, status bool, gasUsed int64) ptypes.Transaction {
	return ptypes.Transaction{
		ID:      id,
		Status:  status,
		GasUsed: big.NewInt(gasUsed),
	}
}

// mkReceipt builds a receipt whose ActionHash matches the given txID (= "0xHEX").
// Returns the receipt; caller is responsible for inserting into receiptByTxID.
func mkReceipt(txID string, status uint64, gasUsed uint64) *action.Receipt {
	b, _ := hex.DecodeString(txID[2:])
	var h hash.Hash256
	copy(h[:], b)
	return &action.Receipt{
		Status:      status,
		GasConsumed: gasUsed,
		BlockHeight: 1,
		ActionHash:  h,
	}
}

func mkReceiptMap(pairs ...interface{}) map[string]*action.Receipt {
	m := make(map[string]*action.Receipt, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		txID := pairs[i].(string)
		r := pairs[i+1].(*action.Receipt)
		m[txID] = r
	}
	return m
}

// padTxID right-pads to 32 bytes hex so it can encode as a hash.Hash256.
func padTxID(short string) string {
	// "0x" + 64 hex chars
	hexStr := short[2:]
	for len(hexStr) < 64 {
		hexStr += "0"
	}
	return "0x" + hexStr
}

func TestOverrideTxs_AlwaysSyncsStatusAndGas(t *testing.T) {
	r := require.New(t)

	tx0ID := padTxID("0xa0")
	tx1ID := padTxID("0xa1")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx(tx0ID, false, 1234),
				mkPipelineTx(tx1ID, true, 5678),
			},
		},
	}
	receipts := mkReceiptMap(
		tx0ID, mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_Success), 9999),
		tx1ID, mkReceipt(tx1ID, uint64(iotextypes.ReceiptStatus_Success), 8888),
	)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged, "tx0 status diverged (replay false vs canonical success)")
	r.True(out.BlockFile.Txs[0].Status)
	r.EqualValues(9999, out.BlockFile.Txs[0].GasUsed.Int64())
	r.True(out.BlockFile.Txs[1].Status)
	r.EqualValues(8888, out.BlockFile.Txs[1].GasUsed.Int64(), "tx1 gas overridden even when status agrees")
}

// REGRESSION: receipts must be looked up by tx.ID, NOT by parallel index.
// This test simulates a block where a native action (e.g., GrantReward) precedes the
// only EVM tx — receipts has 2 entries (action-ordered), BlockFile.Txs has 1 entry
// (eth-compatible only). Old (buggy) code would compare receipts[0] (= GrantReward
// receipt) against BlockFile.Txs[0] (= the EVM tx), corrupting status / gas / divergence.
func TestOverrideTxs_NativeActionBeforeEVMTx_NoMisalignment(t *testing.T) {
	r := require.New(t)

	grantRewardTxID := padTxID("0xfeed01") // action[0] in block — NOT in BlockFile.Txs
	evmTxID := padTxID("0xbeef02")         // action[1] in block — IS in BlockFile.Txs[0]

	// EVM tx: replay says SUCCESS, gas 5000. Canonical receipt: SUCCESS, gas 5500.
	// GrantReward (native): canonical receipt SUCCESS, gas 0. NOT a tx in BlockFile.Txs.
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{
					ID: evmTxID, From: "0xsender", To: "0xtarget", Status: true,
					Gas: big.NewInt(10000), GasUsed: big.NewInt(5000),
					Value: (*hexutil.Big)(big.NewInt(0)),
				},
			},
		},
	}
	// Receipt map covers BOTH actions (action-ordered), keyed by ActionHash hex.
	receipts := mkReceiptMap(
		grantRewardTxID, mkReceipt(grantRewardTxID, uint64(iotextypes.ReceiptStatus_Success), 0),
		evmTxID, mkReceipt(evmTxID, uint64(iotextypes.ReceiptStatus_Success), 5500),
	)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(0, diverged, "EVM tx status matches canonical (both SUCCESS) — NOT diverged")
	r.True(out.BlockFile.Txs[0].Status)
	r.EqualValues(5500, out.BlockFile.Txs[0].GasUsed.Int64(),
		"EVM tx gas must come from EVM tx's receipt (5500), NOT GrantReward's receipt (0)")
}

// Stronger regression: multiple native actions interleaved between EVM txs.
// Block layout: [GrantReward, EVM tx A, putPollResult, EVM tx B]
// BlockFile.Txs:    [tx A, tx B]
// receipts:         [grantR, recA, putPoll, recB]
// Old buggy code would map receipts[0]→txA, receipts[1]→txB → both assignments wrong.
func TestOverrideTxs_InterleavedNativeActions(t *testing.T) {
	r := require.New(t)
	grantTx := padTxID("0xa1")
	evmA := padTxID("0xa2")
	pollTx := padTxID("0xa3")
	evmB := padTxID("0xa4")

	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: evmA, Status: false, GasUsed: big.NewInt(100)}, // replay: fail/100
				{ID: evmB, Status: true, GasUsed: big.NewInt(200)},  // replay: success/200
			},
		},
	}
	receipts := mkReceiptMap(
		grantTx, mkReceipt(grantTx, uint64(iotextypes.ReceiptStatus_Success), 0),
		evmA, mkReceipt(evmA, uint64(iotextypes.ReceiptStatus_Success), 7777),                // canonical: SUCCESS / 7777 → diverges from replay
		pollTx, mkReceipt(pollTx, uint64(iotextypes.ReceiptStatus_Success), 0),
		evmB, mkReceipt(evmB, uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 8888),   // canonical: REVERT / 8888 → diverges from replay
	)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(2, diverged, "both EVM txs diverge from canonical")

	// evmA: replay said FAIL → canonical SUCCESS, must override + use 7777 gas
	r.True(out.BlockFile.Txs[0].Status, "evmA overridden to canonical SUCCESS")
	r.EqualValues(7777, out.BlockFile.Txs[0].GasUsed.Int64())

	// evmB: replay said SUCCESS → canonical REVERT, must override + use 8888 gas
	r.False(out.BlockFile.Txs[1].Status, "evmB overridden to canonical REVERT")
	r.EqualValues(8888, out.BlockFile.Txs[1].GasUsed.Int64())
}

// Replay said FAIL but canonical says SUCCESS: only tx-level fields are
// overridden; the replay's full trace tree (including the failed root that
// represented the action being run at replay time) is preserved.
func TestOverrideTxs_DivergedReplayFail_PreservesTraces(t *testing.T) {
	r := require.New(t)
	tx0ID := padTxID("0xb0")
	tx1ID := padTxID("0xb1")
	failedTraceID := "trace-tx0-failed"
	otherTraceID := "trace-tx1-success"
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{
					ID: tx0ID, From: "0xsender0", To: "0xcontract", Status: false,
					GasUsed: big.NewInt(100), Gas: big.NewInt(50000),
					Value: (*hexutil.Big)(big.NewInt(123)),
					Input: hexutil.Bytes{0xab, 0xcd},
				},
				{ID: tx1ID, Status: true, GasUsed: big.NewInt(200)},
			},
			Traces: []ptypes.Trace{
				{ID: otherTraceID, TxID: tx1ID},
			},
			ErrorTraces: []ptypes.Trace{
				{ID: failedTraceID, TxID: tx0ID},
			},
			ErrorEvents: []ptypes.Event{
				{ParentTraceID: failedTraceID, Address: "0xsomecontract"},
				{ParentTraceID: "stale-other", Address: "0xunaffected"},
			},
		},
	}
	receipts := mkReceiptMap(
		tx0ID, mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_Success), 7777),
		tx1ID, mkReceipt(tx1ID, uint64(iotextypes.ReceiptStatus_Success), 200),
	)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged)
	// tx0 fields overridden to canonical
	r.True(out.BlockFile.Txs[0].Status)
	r.EqualValues(7777, out.BlockFile.Txs[0].GasUsed.Int64())
	// Trace tree untouched — both buckets preserved as-is
	r.Len(out.BlockFile.Traces, 1, "tx1's trace stays")
	r.Equal(otherTraceID, out.BlockFile.Traces[0].ID)
	r.Len(out.BlockFile.ErrorTraces, 1, "tx0's diverged trace preserved (was previously dropped)")
	r.Equal(failedTraceID, out.BlockFile.ErrorTraces[0].ID)
	// ErrorEvents untouched
	r.Len(out.BlockFile.ErrorEvents, 2)
}

// Replay said SUCCESS but canonical says REVERT: only tx-level fields are
// overridden; replay's trace tree (including any sub-frames the SUCCESS replay
// produced) is preserved for diagnostic value.
func TestOverrideTxs_DivergedReplaySuccess_PreservesTraces(t *testing.T) {
	r := require.New(t)
	tx0ID := padTxID("0xc0")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: tx0ID, From: "0xsender", To: "0xtarget", Status: true,
					Gas: big.NewInt(100000), GasUsed: big.NewInt(100),
					Value: (*hexutil.Big)(big.NewInt(0))},
			},
			Traces: []ptypes.Trace{
				{ID: "replay-frame-root", TxID: tx0ID, TraceAddress: []int64{}},
				{ID: "replay-frame-sub",  TxID: tx0ID, TraceAddress: []int64{0}},
			},
		},
	}
	rcpt := mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 9999)
	receipts := mkReceiptMap(tx0ID, rcpt)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged)
	// tx fields overridden
	r.False(out.BlockFile.Txs[0].Status)
	r.EqualValues(9999, out.BlockFile.Txs[0].GasUsed.Int64())
	// Trace tree preserved as-is in the original Traces bucket
	// (we don't move it to ErrorTraces — that would also lose downstream
	// data, and the per-frame `.error` field on each trace is the right
	// source of truth for "did THIS frame fail" anyway).
	r.Len(out.BlockFile.Traces, 2, "both replay frames kept")
	r.Empty(out.BlockFile.ErrorTraces)
}

func TestOverrideTxs_DivergedCreate_PreservesTraces(t *testing.T) {
	r := require.New(t)
	createTxID := padTxID("0xd0")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: createTxID, From: "0xdeployer", To: "", Status: false,
					Gas: big.NewInt(0), GasUsed: big.NewInt(0)},
			},
			Traces: []ptypes.Trace{
				{ID: "replay-create-root", TxID: createTxID, CallCreateType: "create"},
			},
		},
	}
	receipts := mkReceiptMap(
		createTxID, mkReceipt(createTxID, uint64(iotextypes.ReceiptStatus_Success), 50000),
	)
	overrideTxsAndStripDivergedTraces(out, receipts)
	// Trace preserved untouched
	r.Len(out.BlockFile.Traces, 1)
	r.Equal("replay-create-root", out.BlockFile.Traces[0].ID)
	r.Equal("create", out.BlockFile.Traces[0].CallCreateType)
}

func TestOverrideTxs_NoDivergence_KeepsTracesIntact(t *testing.T) {
	r := require.New(t)
	tx0ID := padTxID("0xe0")
	tx1ID := padTxID("0xe1")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx(tx0ID, true, 100),
				mkPipelineTx(tx1ID, false, 50),
			},
			Traces: []ptypes.Trace{
				{ID: "t0", TxID: tx0ID},
			},
			ErrorTraces: []ptypes.Trace{
				{ID: "t1", TxID: tx1ID},
			},
			ErrorEvents: []ptypes.Event{
				{ParentTraceID: "t1"},
			},
		},
	}
	receipts := mkReceiptMap(
		tx0ID, mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_Success), 0),
		tx1ID, mkReceipt(tx1ID, uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 0),
	)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(0, diverged)
	r.Len(out.BlockFile.Traces, 1)
	r.Len(out.BlockFile.ErrorTraces, 1)
	r.Len(out.BlockFile.ErrorEvents, 1)
}

func TestOverrideTxs_NilSafety(t *testing.T) {
	overrideTxsAndStripDivergedTraces(nil, nil)
	overrideTxsAndStripDivergedTraces(&ptypes.DebankOutPut{}, nil)
	overrideTxsAndStripDivergedTraces(&ptypes.DebankOutPut{BlockFile: &ptypes.BlockFile{}}, nil)
}

// If a tx has no matching canonical receipt (shouldn't happen for valid blocks but
// defensive), the override silently skips that tx rather than crashing.
func TestOverrideTxs_TxWithoutReceiptIsSkipped(t *testing.T) {
	r := require.New(t)
	tx0ID := padTxID("0xf0")
	tx1ID := padTxID("0xf1")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx(tx0ID, false, 100),
				mkPipelineTx(tx1ID, false, 200), // no receipt
			},
		},
	}
	// Only 1 receipt, for tx0
	receipts := mkReceiptMap(
		tx0ID, mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_Success), 999),
	)
	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged, "tx0 evaluated; tx1 silently skipped (no receipt match)")
	r.True(out.BlockFile.Txs[0].Status)
	r.EqualValues(999, out.BlockFile.Txs[0].GasUsed.Int64())
	r.False(out.BlockFile.Txs[1].Status, "tx1 untouched (no receipt)")
	r.EqualValues(200, out.BlockFile.Txs[1].GasUsed.Int64(), "tx1 gas untouched")
}
