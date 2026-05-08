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

func TestOverrideTxs_DivergedReplayFail_StripAndSynthesize(t *testing.T) {
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
	r.True(out.BlockFile.Txs[0].Status, "tx0 status overridden to canonical success")
	r.EqualValues(7777, out.BlockFile.Txs[0].GasUsed.Int64())
	r.Empty(out.BlockFile.ErrorTraces)
	r.Len(out.BlockFile.Traces, 2)
	var synth *ptypes.Trace
	for i := range out.BlockFile.Traces {
		if out.BlockFile.Traces[i].TxID == tx0ID {
			synth = &out.BlockFile.Traces[i]
		}
	}
	r.NotNil(synth, "canonical-minimal trace synthesized for diverged tx0")
	r.Equal("0xsender0", synth.From)
	r.Equal("0xcontract", synth.To)
	r.EqualValues(7777, synth.GasUsed.Int64())
	r.Equal("call", synth.CallCreateType)
	r.EqualValues(0, synth.Subtraces)
	r.Empty(synth.Output)
	r.Empty(synth.Error)
	r.Len(out.BlockFile.ErrorEvents, 1)
	r.Equal("0xunaffected", out.BlockFile.ErrorEvents[0].Address)
}

func TestOverrideTxs_DivergedReplaySuccess_StripAndSynthesize(t *testing.T) {
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
				{ID: "wrong-success-1", TxID: tx0ID},
				{ID: "wrong-success-2", TxID: tx0ID},
			},
		},
	}
	r1 := mkReceipt(tx0ID, uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 9999)
	r1.SetExecutionRevertMsg("OOG in inner call")
	receipts := mkReceiptMap(tx0ID, r1)

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged)
	r.False(out.BlockFile.Txs[0].Status)
	r.Empty(out.BlockFile.Traces, "wrong success traces dropped")
	r.Len(out.BlockFile.ErrorTraces, 1, "synthesized minimal in ErrorTraces")

	synth := out.BlockFile.ErrorTraces[0]
	r.Equal(tx0ID, synth.TxID)
	r.Equal("0xsender", synth.From)
	r.Equal("0xtarget", synth.To)
	r.EqualValues(9999, synth.GasUsed.Int64())
	r.Contains(synth.Error, "execution reverted")
	r.Contains(synth.Error, "OOG in inner call")
}

func TestOverrideTxs_DivergedSynthesizeCreate(t *testing.T) {
	r := require.New(t)
	createTxID := padTxID("0xd0")
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: createTxID, From: "0xdeployer", To: "", Status: false,
					Gas: big.NewInt(0), GasUsed: big.NewInt(0)},
			},
			Traces: []ptypes.Trace{
				{ID: "wrong-tx0-success", TxID: createTxID},
			},
		},
	}
	receipts := mkReceiptMap(
		createTxID, mkReceipt(createTxID, uint64(iotextypes.ReceiptStatus_Success), 50000),
	)
	overrideTxsAndStripDivergedTraces(out, receipts)
	r.Len(out.BlockFile.Traces, 1)
	r.Equal("create", out.BlockFile.Traces[0].CallCreateType)
	r.Empty(out.BlockFile.Traces[0].CallType, "CREATE has empty CallType")
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
