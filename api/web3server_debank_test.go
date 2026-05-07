package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-proto/golang/iotexapi"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.uber.org/mock/gomock"

	"github.com/iotexproject/iotex-core/v2/action"
)

// TestContractMultiCallDebank_ProtocolAddr verifies that protocol addresses
// (rewarding/staking/poll/rolldpos) are routed through callProtocolAddr and
// return ABI-encoded data instead of "0x".
func TestContractMultiCallDebank_ProtocolAddr(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	amount := big.NewInt(12345)
	core.EXPECT().ReadState(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&iotexapi.ReadStateResponse{Data: []byte(amount.String())}, nil)
	core.EXPECT().TipHeight().Return(uint64(999)).AnyTimes()
	core.EXPECT().BlockHashByBlockHeight(uint64(999)).Return(hash.ZeroHash256, nil).AnyTimes()

	in := gjson.Parse(`{"params":[
		[{
			"from": "0x0000000000000000000000000000000000000000",
			"to":   "0xA576C141e5659137ddDa4223d209d4744b2106BE",
			"gas":  "0x4e20",
			"data": "0xad7a672f"
		}],
		{"block_id": "latest", "type": "Equals"}
	]}`)
	out, err := web3svr.contractMultiCallDebank(context.Background(), &in)
	require.NoError(err)

	resp, ok := out.(*debankMultiCallResp)
	require.True(ok)
	require.Len(resp.Results, 1)
	require.Equal(0, resp.Results[0].Code, "protocol addr call should succeed")
	require.NotEmpty(resp.Results[0].Result, "protocol addr should not return empty result")
	require.True(resp.Stats.Success)
	require.Equal(uint64(999), resp.Stats.BlockNum)
	require.False(resp.Stats.CacheEnabled)
}

// TestContractMultiCallDebank_RegularContract verifies the EVM contract path
// (ReadContract) for non-protocol addresses.
func TestContractMultiCallDebank_RegularContract(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	receipt := &iotextypes.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		GasConsumed: 21000,
	}
	core.EXPECT().ReadContract(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("deadbeef", receipt, nil)
	core.EXPECT().TipHeight().Return(uint64(50)).AnyTimes()
	core.EXPECT().BlockHashByBlockHeight(uint64(50)).Return(hash.ZeroHash256, nil).AnyTimes()

	in := gjson.Parse(`{"params":[
		[{
			"from": "0x0000000000000000000000000000000000000000",
			"to":   "0x7c13866F9253DEf79e20034eDD011e1d69E67fe5",
			"gas":  "0x4e20",
			"data": "0x1234"
		}],
		{"block_id": "latest", "type": "Equals"}
	]}`)
	out, err := web3svr.contractMultiCallDebank(context.Background(), &in)
	require.NoError(err)
	resp := out.(*debankMultiCallResp)
	require.Len(resp.Results, 1)
	require.Equal(0, resp.Results[0].Code)
	require.Equal(int64(21000), resp.Results[0].GasUsed)
	require.Equal("deadbeef", hex.EncodeToString(resp.Results[0].Result))
}

// TestContractMultiCallDebank_RevertMapsErrorCode verifies that an EVM revert
// is mapped to the cosmos-evm-compatible -39000 error code.
func TestContractMultiCallDebank_RevertMapsErrorCode(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	receipt := &iotextypes.Receipt{
		Status:             uint64(iotextypes.ReceiptStatus_ErrExecutionReverted),
		ExecutionRevertMsg: "boom",
	}
	core.EXPECT().ReadContract(gomock.Any(), gomock.Any(), gomock.Any()).
		Return("", receipt, nil)
	core.EXPECT().TipHeight().Return(uint64(1)).AnyTimes()
	core.EXPECT().BlockHashByBlockHeight(gomock.Any()).Return(hash.ZeroHash256, nil).AnyTimes()

	in := gjson.Parse(`{"params":[
		[{"to":"0x7c13866F9253DEf79e20034eDD011e1d69E67fe5","data":"0x1234"}],
		{"block_id":"latest","type":"Equals"}
	]}`)
	out, err := web3svr.contractMultiCallDebank(context.Background(), &in)
	require.NoError(err)
	resp := out.(*debankMultiCallResp)
	require.Equal(debankSimulateErrorReverted, resp.Results[0].Code)
	require.Equal("boom", resp.Results[0].Err)
	require.False(resp.Stats.Success)
}

// TestSimulateTransactionsDebank_BatchAndTxIDInjection drives the handler's
// SimulateExecutionBatch path with a mocked backend and checks that response
// statistics, tx_id injection, and per-tx success bits are wire-compatible.
func TestSimulateTransactionsDebank_BatchAndTxIDInjection(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	successReceipt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		GasConsumed: 21000,
	}
	revertedReceipt := (&action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_ErrExecutionReverted),
		GasConsumed: 5000,
	}).SetExecutionRevertMsg("simulate-revert")
	batchResults := []SimulateBatchResult{
		{Output: nil, Receipt: successReceipt, Err: nil},
		{Output: nil, Receipt: revertedReceipt, Err: nil},
	}
	batchInfo := SimulateBatchInfo{
		BlockHeight: 42,
		BlockHash:   hash.ZeroHash256,
		BlockTime:   time.Unix(1700000000, 0),
	}
	core.EXPECT().
		SimulateExecutionBatch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(batchResults, batchInfo, nil)

	in := gjson.Parse(`{"params":[
		[
			{"from":"0x0000000000000000000000000000000000000001","to":"0x7c13866F9253DEf79e20034eDD011e1d69E67fe5","data":"0x"},
			{"from":"0x0000000000000000000000000000000000000002","to":"0x7c13866F9253DEf79e20034eDD011e1d69E67fe5","data":"0x"}
		],
		{"block_id":"latest","type":"Equals"}
	]}`)
	out, err := web3svr.simulateTransactionsDebank(context.Background(), &in)
	require.NoError(err)
	resp := out.(debankSimulateResp)
	require.Len(resp.Results, 2)

	// Tx 0: success
	require.Equal(0, resp.Results[0].Code)
	require.Equal(uint64(21000), resp.Results[0].GasUsed)
	// Tx 1: reverted, mapped to -39000
	require.Equal(debankSimulateErrorReverted, resp.Results[1].Code)
	require.Equal("simulate-revert", resp.Results[1].Err)

	// Stats success bit reflects the worst-case tx (one revert => false).
	require.False(resp.Stats.Success)
	require.Equal(uint64(42), resp.Stats.BlockNum)
	require.Equal(int64(1700000000), resp.Stats.BlockTime)
}

// TestParseDebankBlockContext exercises the BlockType=Contains downgrade and
// raw block_id passthrough on the request side.
func TestParseDebankBlockContext(t *testing.T) {
	require := require.New(t)

	// "Equals" with an explicit number.
	bnh, err := parseDebankBlockContextHeight(gjson.Parse(`{"block_id":"0x10","type":"Equals"}`))
	require.NoError(err)
	require.NotNil(bnh.BlockNumber)
	require.Equal(int64(16), bnh.BlockNumber.Int64())

	// "Contains" downgrades to latest regardless of block_id.
	bnh, err = parseDebankBlockContextHeight(gjson.Parse(`{"block_id":"0x10","type":"Contains"}`))
	require.NoError(err)
	require.NotNil(bnh.BlockNumber)
	require.True(bnh.BlockNumber.Int64() < 0, "Contains should downgrade to LatestBlockNumber sentinel")

	// Empty/missing => default to latest.
	bnh, err = parseDebankBlockContextHeight(gjson.Result{})
	require.NoError(err)
	require.NotNil(bnh.BlockNumber)
}

// TestDebankBlockTypeJSONRoundtrip ensures BlockType serializes as the wire
// strings "Equals" / "Contains" required for cosmos-evm parity.
func TestDebankBlockTypeJSONRoundtrip(t *testing.T) {
	require := require.New(t)

	bts, err := json.Marshal(debankBlockTypeEquals)
	require.NoError(err)
	require.Equal(`"Equals"`, string(bts))

	bts, err = json.Marshal(debankBlockTypeContains)
	require.NoError(err)
	require.Equal(`"Contains"`, string(bts))

	var bt debankBlockType
	require.NoError(json.Unmarshal([]byte(`"Contains"`), &bt))
	require.Equal(debankBlockTypeContains, bt)
	require.NoError(json.Unmarshal([]byte(`"Equals"`), &bt))
	require.Equal(debankBlockTypeEquals, bt)
	// Unknown values fall back to Equals.
	require.NoError(json.Unmarshal([]byte(`"???"`), &bt))
	require.Equal(debankBlockTypeEquals, bt)
}

// TestSimulateTransactionsDebank_ProtocolAddrRouting verifies that protocol
// addresses inside a simulate batch are diverted to callProtocolAddr (eth_call
// parity) and the SimulateExecutionBatch call only sees Skip slots for them.
func TestSimulateTransactionsDebank_ProtocolAddrRouting(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	// callProtocolAddr internally calls ReadState for the protocol-addr slot.
	amount := big.NewInt(99)
	core.EXPECT().ReadState(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&iotexapi.ReadStateResponse{Data: []byte(amount.String())}, nil)

	// SimulateExecutionBatch should still be called even though both slots are
	// Skip=true (handler delegates header/stats info to the batch helper).
	// Capture the args to assert routing.
	var captured []SimulateBatchArg
	core.EXPECT().
		SimulateExecutionBatch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ uint64, _ bool, args []SimulateBatchArg) ([]SimulateBatchResult, SimulateBatchInfo, error) {
			captured = args
			out := make([]SimulateBatchResult, len(args))
			return out, SimulateBatchInfo{BlockHeight: 100, BlockHash: hash.ZeroHash256, BlockTime: time.Unix(1700000000, 0)}, nil
		})

	in := gjson.Parse(`{"params":[
		[{
			"from":"0x0000000000000000000000000000000000000001",
			"to":"0xA576C141e5659137ddDa4223d209d4744b2106BE",
			"data":"0xad7a672f"
		}],
		{"block_id":"latest","type":"Equals"}
	]}`)
	out, err := web3svr.simulateTransactionsDebank(context.Background(), &in)
	require.NoError(err)

	require.Len(captured, 1)
	require.True(captured[0].Skip, "protocol addr slot must be Skip=true")
	require.Nil(captured[0].Envelope)

	resp := out.(debankSimulateResp)
	require.Len(resp.Results, 1)
	require.Equal(0, resp.Results[0].Code)
	require.Equal(uint64(21000), resp.Results[0].GasUsed)
	require.Len(resp.Results[0].Traces, 1, "protocol addr should emit one synthetic trace")
	require.Equal("STATICCALL", resp.Results[0].Traces[0].CallType)
	require.NotEmpty(resp.Results[0].Traces[0].Output, "protocol-addr trace should carry ABI-encoded output")
}

// TestFlattenCallFrames verifies that nested callTracer JSON is flattened
// depth-first with correct ID/parent/pos linking.
func TestFlattenCallFrames(t *testing.T) {
	require := require.New(t)
	raw := json.RawMessage(`{
		"type": "CALL",
		"from": "0x1111111111111111111111111111111111111111",
		"to":   "0x2222222222222222222222222222222222222222",
		"value": "0xa",
		"gas": "0x5208",
		"gasUsed": "0x5208",
		"input": "0xabcd",
		"output": "0xbeef",
		"calls": [
			{
				"type": "STATICCALL",
				"from": "0x2222222222222222222222222222222222222222",
				"to":   "0x3333333333333333333333333333333333333333",
				"gas": "0x100",
				"gasUsed": "0x10",
				"input": "0x"
			},
			{
				"type": "DELEGATECALL",
				"from": "0x2222222222222222222222222222222222222222",
				"to":   "0x4444444444444444444444444444444444444444",
				"gas": "0x100",
				"gasUsed": "0x20",
				"input": "0x",
				"calls": [
					{
						"type": "CREATE",
						"from": "0x4444444444444444444444444444444444444444",
						"gas": "0x50",
						"gasUsed": "0x40",
						"input": "0x6080"
					}
				]
			}
		]
	}`)
	traces := flattenCallFrames(raw, 7)
	require.Len(traces, 4)

	// DFS order: root, child[0], child[1], child[1].child[0]
	require.Equal("0", traces[0].ID)
	require.Equal("", traces[0].ParentTraceID)
	require.Equal(int64(0), traces[0].PosInParentTrace)
	require.Equal("call", traces[0].CallCreateType)
	require.Equal("CALL", traces[0].CallType)

	require.Equal("0_0", traces[1].ID)
	require.Equal("0", traces[1].ParentTraceID)
	require.Equal(int64(0), traces[1].PosInParentTrace)
	require.Equal("STATICCALL", traces[1].CallType)

	require.Equal("0_1", traces[2].ID)
	require.Equal("0", traces[2].ParentTraceID)
	require.Equal(int64(1), traces[2].PosInParentTrace)
	require.Equal("DELEGATECALL", traces[2].CallType)

	require.Equal("0_1_0", traces[3].ID)
	require.Equal("0_1", traces[3].ParentTraceID)
	require.Equal("create", traces[3].CallCreateType)
	require.Equal("CREATE", traces[3].CallType)

	// All traces share the tx_id seeded from txIdx=7.
	expected := common.BigToHash(big.NewInt(7))
	for _, tr := range traces {
		require.Equal(expected, tr.TxID)
	}
}

// TestClassifyCallType covers the type-string mapping table.
func TestClassifyCallType(t *testing.T) {
	cases := []struct {
		in     string
		create string
		call   string
	}{
		{"CALL", "call", "CALL"},
		{"STATICCALL", "call", "STATICCALL"},
		{"DELEGATECALL", "call", "DELEGATECALL"},
		{"CALLCODE", "call", "CALLCODE"},
		{"CREATE", "create", "CREATE"},
		{"CREATE2", "create2", "CREATE2"},
		{"SELFDESTRUCT", "suicide", "SELFDESTRUCT"},
	}
	for _, c := range cases {
		got1, got2 := classifyCallType(c.in)
		require.Equal(t, c.create, got1, "create type for %s", c.in)
		require.Equal(t, c.call, got2, "call type for %s", c.in)
	}
}

// TestSimulateBatchToDebank_TraceDataPropagated ensures TraceData JSON from
// SimulateBatchResult is decoded and inserted into traces[].
func TestSimulateBatchToDebank_TraceDataPropagated(t *testing.T) {
	require := require.New(t)
	tracerOut := json.RawMessage(`{
		"type":"CALL",
		"from":"0x0000000000000000000000000000000000000001",
		"to":"0x0000000000000000000000000000000000000002",
		"gas":"0x5208","gasUsed":"0x5208","input":"0x"
	}`)
	r := &SimulateBatchResult{
		Receipt: &action.Receipt{
			Status:      uint64(iotextypes.ReceiptStatus_Success),
			GasConsumed: 21000,
		},
		TraceData: tracerOut,
	}
	out := simulateBatchToDebank(r, 1)
	require.Equal(0, out.Code)
	require.Len(out.Traces, 1)
	require.Equal("CALL", out.Traces[0].CallType)
}

// TestSimulateBatchToDebank_ReceiptLogsToEvents covers the conversion of
// receipt logs into wire-level events with selector/topic/tx_id fields.
func TestSimulateBatchToDebank_ReceiptLogsToEvents(t *testing.T) {
	require := require.New(t)

	logTopic := hash.Hash256b([]byte("Transfer(address,address,uint256)"))
	receipt := (&action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		GasConsumed: 30000,
	})
	receipt.AddLogs(&action.Log{
		Address: "io1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqd39ym7",
		Topics:  []hash.Hash256{logTopic, hash.ZeroHash256},
		Data:    []byte{0x01, 0x02},
	})

	out := simulateBatchToDebank(&SimulateBatchResult{Receipt: receipt}, 7)
	require.Equal(0, out.Code)
	require.Equal(uint64(30000), out.GasUsed)
	require.Len(out.Events, 1)
	ev := out.Events[0]
	require.Equal("0x"+hex.EncodeToString(logTopic[:]), ev.Selector)
	require.Len(ev.Topics, 1)
	require.Equal(common.BigToHash(big.NewInt(7)), ev.TxId)
	require.Equal([]byte{0x01, 0x02}, []byte(ev.Data))
}
