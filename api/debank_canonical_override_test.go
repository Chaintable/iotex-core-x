package api

import (
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

func mkReceipt(status uint64, gasUsed uint64) *action.Receipt {
	return &action.Receipt{
		Status:      status,
		GasConsumed: gasUsed,
		BlockHeight: 1,
		ActionHash:  hash.Hash256b([]byte("dummy")),
	}
}

func TestOverrideTxs_AlwaysSyncsStatusAndGas(t *testing.T) {
	r := require.New(t)

	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx("0xtx0", false, 1234), // replay says fail/1234
				mkPipelineTx("0xtx1", true, 5678),  // replay says success/5678
			},
		},
	}
	receipts := []*action.Receipt{
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 9999),
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 8888),
	}

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged, "tx0 status diverged (replay false vs canonical success)")
	r.True(out.BlockFile.Txs[0].Status, "tx0 overridden to canonical success")
	r.EqualValues(9999, out.BlockFile.Txs[0].GasUsed.Int64(), "tx0 gas overridden")
	r.True(out.BlockFile.Txs[1].Status)
	r.EqualValues(8888, out.BlockFile.Txs[1].GasUsed.Int64(), "tx1 gas overridden even when status agrees")
}

// canonical SUCCESS / replay FAIL: replay's "failed" call tree went to ErrorTraces +
// ErrorEvents. Both must be stripped, AND a canonical-minimal trace appended to Traces
// (since canonical status = success).
func TestOverrideTxs_DivergedReplayFail_StripAndSynthesize(t *testing.T) {
	r := require.New(t)

	failedTraceID := "trace-tx0-failed"
	otherTraceID := "trace-tx1-success"
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{
					ID: "0xtx0", From: "0xsender0", To: "0xcontract", Status: false,
					GasUsed: big.NewInt(100), Gas: big.NewInt(50000),
					Value: (*hexutil.Big)(big.NewInt(123)),
					Input: hexutil.Bytes{0xab, 0xcd},
				},
				{ID: "0xtx1", Status: true, GasUsed: big.NewInt(200)},
			},
			Traces: []ptypes.Trace{
				{ID: otherTraceID, TxID: "0xtx1"}, // tx1 OK in replay → kept
			},
			ErrorTraces: []ptypes.Trace{
				{ID: failedTraceID, TxID: "0xtx0"}, // diverged → drop
			},
			ErrorEvents: []ptypes.Event{
				{ParentTraceID: failedTraceID, Address: "0xsomecontract"},
				{ParentTraceID: "stale-other", Address: "0xunaffected"},
			},
		},
	}
	receipts := []*action.Receipt{
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 7777), // diverged
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 200),
	}

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged)
	r.True(out.BlockFile.Txs[0].Status, "tx0 status overridden to canonical success")
	r.EqualValues(7777, out.BlockFile.Txs[0].GasUsed.Int64(), "gas overridden")

	// ErrorTraces should be empty: diverged trace was stripped.
	r.Empty(out.BlockFile.ErrorTraces)

	// Traces should now have: tx1's surviving original + tx0's synthesized canonical-minimal.
	r.Len(out.BlockFile.Traces, 2)
	var synth *ptypes.Trace
	for i := range out.BlockFile.Traces {
		if out.BlockFile.Traces[i].TxID == "0xtx0" {
			synth = &out.BlockFile.Traces[i]
		}
	}
	r.NotNil(synth, "canonical-minimal trace synthesized for diverged tx0")
	r.Equal("0xsender0", synth.From)
	r.Equal("0xcontract", synth.To)
	r.EqualValues(7777, synth.GasUsed.Int64(), "synthesized GasUsed = canonical")
	r.Equal("call", synth.CallCreateType)
	r.EqualValues(0, synth.Subtraces, "no internal calls")
	r.Empty(synth.Output, "canonical receipts don't expose Output")
	r.Empty(synth.Error, "canonical SUCCESS → no error string")

	// ErrorEvents: only the orphan-of-stripped-trace removed; unaffected one kept.
	r.Len(out.BlockFile.ErrorEvents, 1)
	r.Equal("0xunaffected", out.BlockFile.ErrorEvents[0].Address)
}

// canonical FAIL / replay SUCCESS: replay's call tree was in Traces (success bucket).
// Drop it; synthesize canonical-minimal in ErrorTraces (since canonical = revert).
func TestOverrideTxs_DivergedReplaySuccess_StripAndSynthesize(t *testing.T) {
	r := require.New(t)

	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: "0xtx0", From: "0xsender", To: "0xtarget", Status: true,
					Gas: big.NewInt(100000), GasUsed: big.NewInt(100),
					Value: (*hexutil.Big)(big.NewInt(0))},
			},
			Traces: []ptypes.Trace{
				{ID: "wrong-success-1", TxID: "0xtx0"},
				{ID: "wrong-success-2", TxID: "0xtx0"},
			},
		},
	}
	r1 := mkReceipt(uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 9999)
	r1.SetExecutionRevertMsg("OOG in inner call")
	receipts := []*action.Receipt{r1}

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged)
	r.False(out.BlockFile.Txs[0].Status)
	r.Empty(out.BlockFile.Traces, "wrong success traces dropped")
	r.Len(out.BlockFile.ErrorTraces, 1, "synthesized minimal in ErrorTraces")

	synth := out.BlockFile.ErrorTraces[0]
	r.Equal("0xtx0", synth.TxID)
	r.Equal("0xsender", synth.From)
	r.Equal("0xtarget", synth.To)
	r.EqualValues(9999, synth.GasUsed.Int64())
	r.Contains(synth.Error, "execution reverted", "error string set on FAIL")
	r.Contains(synth.Error, "OOG in inner call", "revert message propagated")
}

// CREATE tx (To empty) gets CallCreateType="create"
func TestOverrideTxs_DivergedSynthesizeCreate(t *testing.T) {
	r := require.New(t)
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				{ID: "0xtx_create", From: "0xdeployer", To: "", Status: false,
					Gas: big.NewInt(0), GasUsed: big.NewInt(0)},
			},
			Traces: []ptypes.Trace{
				{ID: "wrong-tx0-success", TxID: "0xtx_create"},
			},
		},
	}
	receipts := []*action.Receipt{
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 50000), // canonical SUCCESS
	}
	overrideTxsAndStripDivergedTraces(out, receipts)
	r.Len(out.BlockFile.Traces, 1)
	r.Equal("create", out.BlockFile.Traces[0].CallCreateType)
	r.Empty(out.BlockFile.Traces[0].CallType, "CREATE has empty CallType")
}

func TestOverrideTxs_NoDivergence_KeepsTracesIntact(t *testing.T) {
	r := require.New(t)
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx("0xtx0", true, 100),
				mkPipelineTx("0xtx1", false, 50),
			},
			Traces: []ptypes.Trace{
				{ID: "t0", TxID: "0xtx0"},
			},
			ErrorTraces: []ptypes.Trace{
				{ID: "t1", TxID: "0xtx1"},
			},
			ErrorEvents: []ptypes.Event{
				{ParentTraceID: "t1"},
			},
		},
	}
	receipts := []*action.Receipt{
		mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 0),
		mkReceipt(uint64(iotextypes.ReceiptStatus_ErrExecutionReverted), 0),
	}

	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(0, diverged, "both txs match canonical")
	r.Len(out.BlockFile.Traces, 1)
	r.Len(out.BlockFile.ErrorTraces, 1)
	r.Len(out.BlockFile.ErrorEvents, 1, "all replay traces/events kept when status agrees")
}

func TestOverrideTxs_NilSafety(t *testing.T) {
	// Should not panic.
	overrideTxsAndStripDivergedTraces(nil, nil)
	overrideTxsAndStripDivergedTraces(&ptypes.DebankOutPut{}, nil)
	overrideTxsAndStripDivergedTraces(&ptypes.DebankOutPut{BlockFile: &ptypes.BlockFile{}}, nil)
}

func TestOverrideTxs_ReceiptCountLessThanTxs(t *testing.T) {
	r := require.New(t)
	out := &ptypes.DebankOutPut{
		BlockFile: &ptypes.BlockFile{
			Txs: []ptypes.Transaction{
				mkPipelineTx("0xtx0", false, 100),
				mkPipelineTx("0xtx1", false, 200),
			},
		},
	}
	// Only 1 receipt for 2 txs — second tx should be skipped, not crash
	receipts := []*action.Receipt{mkReceipt(uint64(iotextypes.ReceiptStatus_Success), 999)}
	diverged := overrideTxsAndStripDivergedTraces(out, receipts)
	r.Equal(1, diverged, "only tx0 evaluated")
	r.True(out.BlockFile.Txs[0].Status)
	r.False(out.BlockFile.Txs[1].Status, "tx1 untouched (no receipt)")
}
