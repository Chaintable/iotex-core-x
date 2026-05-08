package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
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
	core.EXPECT().BlockByHash(gomock.Any()).Return(nil, nil).AnyTimes()

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
	core.EXPECT().BlockByHash(gomock.Any()).Return(nil, nil).AnyTimes()

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
	core.EXPECT().BlockByHash(gomock.Any()).Return(nil, nil).AnyTimes()

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
	require.Equal(uint64(0), resp.Results[0].GasUsed,
		"protocol-addr synthetic gas_used must be 0 (not 21000) — these calls bypass EVM and have no measured cost")
	require.Len(resp.Results[0].Traces, 1, "protocol addr should emit one synthetic trace")
	require.Equal("STATICCALL", resp.Results[0].Traces[0].CallType)
	require.NotEmpty(resp.Results[0].Traces[0].Output, "protocol-addr trace should carry ABI-encoded output")
}

// TestDebankArgsCallData_InputDataPrecedence pins the calldata-field
// resolution rules used by simulateTransactionsDebank /
// contractMultiCallDebank. eth_call's parseCallObject picks `input` over
// `data` (web3server_utils.go:370-374); both debank arg types must agree, so
// any client that emits the EVM-standard `input` field gets its calldata
// honored instead of silently dropped.
//
// Pre-fix: debankCallArgs (used by simulate) had no Input field at all, so
// JSON unmarshal of `{"input":"0x..."}` left Data=nil and the EVM saw an
// empty calldata; non-trivial contract calls returned successful-but-empty
// traces, and protocol-addr READ selectors hit BuildReadStateRequest's
// `len<4` branch with errInvalidCallData ("invalid call binary data").
func TestDebankArgsCallData_InputDataPrecedence(t *testing.T) {
	require := require.New(t)
	mk := func(s string) *hexutil.Bytes { b := hexutil.Bytes(common.FromHex(s)); return &b }

	t.Run("debankCallArgs", func(t *testing.T) {
		// input only — the modern EVM-standard shape that used to be dropped.
		require.Equal([]byte{0xad, 0x7a, 0x67, 0x2f}, (&debankCallArgs{Input: mk("0xad7a672f")}).callData())
		// data only — legacy shape, still honored.
		require.Equal([]byte{0xab, 0x2f, 0x0e, 0x51}, (&debankCallArgs{Data: mk("0xab2f0e51")}).callData())
		// both — input wins, matching parseCallObject (web3server_utils.go:370).
		got := (&debankCallArgs{Input: mk("0x01"), Data: mk("0x02")}).callData()
		require.Equal([]byte{0x01}, got, "input must win over data when both supplied")
		// neither — nil for downstream `len(data) < 4` short-circuits.
		require.Nil((&debankCallArgs{}).callData())
	})

	t.Run("debankTransactionArgs", func(t *testing.T) {
		require.Equal([]byte{0xad, 0x7a, 0x67, 0x2f}, (&debankTransactionArgs{Input: mk("0xad7a672f")}).callData())
		require.Equal([]byte{0xab, 0x2f, 0x0e, 0x51}, (&debankTransactionArgs{Data: mk("0xab2f0e51")}).callData())
		got := (&debankTransactionArgs{Input: mk("0x01"), Data: mk("0x02")}).callData()
		require.Equal([]byte{0x01}, got, "input must win over data when both supplied")
		require.Nil((&debankTransactionArgs{}).callData())
	})
}

// TestSimulateTransactionsDebank_InputFieldHonored is the regression test
// for Bug 3: a debank_simulateTransactions request that carries calldata in
// the EVM-standard `input` field (rather than legacy `data`) must reach
// callProtocolAddr / SimulateExecutionBatch with the calldata intact.
//
// We assert both halves of the dispatch:
//   - Protocol-addr READ selector via `input` -> protocolAddrSimulateResult
//     calls callProtocolAddr with the right calldata, which routes through
//     BuildReadStateRequest -> ReadState. Pre-fix this hit the `len<4`
//     branch with errInvalidCallData.
//   - Regular EVM call via `input` -> buildEnvelopeFromDebankCallArgs sets
//     the envelope's data to the input bytes, surfaced via the captured
//     SimulateBatchArg's Envelope.
func TestSimulateTransactionsDebank_InputFieldHonored(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	// keccak("totalBalance()")[:4] == 0xad7a672f, the rewarding-protocol READ
	// selector used by callProtocolAddr's BuildReadStateRequest.
	amount := big.NewInt(12345)
	core.EXPECT().ReadState(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&iotexapi.ReadStateResponse{Data: []byte(amount.String())}, nil)

	// Capture batch args to verify the regular-call envelope received the
	// input bytes verbatim. Selector 0x12345678 is arbitrary — we only need
	// to confirm it round-trips through buildEnvelopeFromDebankCallArgs.
	var captured []SimulateBatchArg
	core.EXPECT().
		SimulateExecutionBatch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ uint64, _ bool, args []SimulateBatchArg) ([]SimulateBatchResult, SimulateBatchInfo, error) {
			captured = args
			out := make([]SimulateBatchResult, len(args))
			return out, SimulateBatchInfo{BlockHeight: 100, BlockHash: hash.ZeroHash256, BlockTime: time.Unix(1700000000, 0)}, nil
		})

	in := gjson.Parse(`{"params":[
		[
			{
				"from":"0x0000000000000000000000000000000000000001",
				"to":"0xA576C141e5659137ddDa4223d209d4744b2106BE",
				"input":"0xad7a672f"
			},
			{
				"from":"0x0000000000000000000000000000000000000001",
				"to":"0x7c13866F9253DEf79e20034eDD011e1d69E67fe5",
				"input":"0x12345678deadbeef"
			}
		],
		{"block_id":"latest","type":"Equals"}
	]}`)
	out, err := web3svr.simulateTransactionsDebank(context.Background(), &in)
	require.NoError(err)

	resp := out.(debankSimulateResp)
	require.Len(resp.Results, 2)

	// Slot 0: protocol addr — the input field reached callProtocolAddr, the
	// READ selector parsed cleanly, and the synthetic trace carries non-empty
	// ABI-encoded output. Pre-fix this slot returned -39000 with
	// "invalid call binary data".
	require.Equal(0, resp.Results[0].Code, "protocol-addr READ via input field must succeed")
	require.NotEmpty(resp.Results[0].Traces[0].Output)

	// Slot 1: regular EVM call — captured envelope must carry the input bytes
	// (not an empty []byte). Pre-fix this slot's envelope had data=[].
	require.Len(captured, 2)
	require.False(captured[1].Skip)
	require.NotNil(captured[1].Envelope)
	exec, ok := captured[1].Envelope.Action().(*action.Execution)
	require.True(ok, "second slot must build an Execution envelope")
	require.Equal([]byte{0x12, 0x34, 0x56, 0x78, 0xde, 0xad, 0xbe, 0xef}, exec.Data(),
		"calldata from `input` field must reach the envelope verbatim")
}

// TestMapEvmErrorToDebankCode pins the error -> wire-code classification used
// by the simulate batch path (when Receipt is nil) and the multi-call path.
//
// Pre-fix the simulate path checked only `errors.Is(action.ErrInsufficientFunds)`
// (the iotex-side sentinel "insufficient funds for gas * price + value") and
// missed go-ethereum's vm.ErrInsufficientBalance ("insufficient balance for
// transfer"). The multi-call path lumped every error into -39004. Both now
// route through this helper so a balance failure returns -39002
// BalanceExhausted regardless of which sentinel the underlying engine raises.
func TestMapEvmErrorToDebankCode(t *testing.T) {
	require := require.New(t)

	// Nil error -> success.
	code, msg := mapEvmErrorToDebankCode(nil)
	require.Equal(0, code)
	require.Equal("", msg)

	// iotex sentinel -> -39002. Original message preserved for traceability.
	code, msg = mapEvmErrorToDebankCode(action.ErrInsufficientFunds)
	require.Equal(debankSimulateErrorInsufficientBalance, code)
	require.Equal(action.ErrInsufficientFunds.Error(), msg)

	// EVM-side error string ("insufficient balance for transfer") — the case
	// that was being misclassified as -39004 before the fix.
	code, _ = mapEvmErrorToDebankCode(errors.New("insufficient balance for transfer"))
	require.Equal(debankSimulateErrorInsufficientBalance, code)

	// gRPC-wrapped variant we observed from coreService.ReadContract:
	// "rpc error: code = Internal desc = insufficient balance for transfer"
	code, _ = mapEvmErrorToDebankCode(errors.New("rpc error: code = Internal desc = insufficient balance for transfer"))
	require.Equal(debankSimulateErrorInsufficientBalance, code)

	// "insufficient funds" variant — covers wrapped errors from any layer.
	code, _ = mapEvmErrorToDebankCode(errors.New("insufficient funds for gas * price + value"))
	require.Equal(debankSimulateErrorInsufficientBalance, code)

	// Unrelated error -> -39004 catch-all.
	code, _ = mapEvmErrorToDebankCode(errors.New("something broke"))
	require.Equal(debankSimulateErrorUnknown, code)
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

// TestMapReceiptStatusToDebankCode pins the EVM-receipt-status -> DeBank
// error code table. Catches regressions where new EVM error codes are
// added but the mapping is forgotten (ReadContract / ExecuteContract sets
// receipt.Status with the actual EVM error and silently err=nil; without
// this mapping a tx that OOG'd would be reported as success).
func TestMapReceiptStatusToDebankCode(t *testing.T) {
	cases := []struct {
		name    string
		status  iotextypes.ReceiptStatus
		revert  string
		wantCod int
		wantMsg string
	}{
		{"success", iotextypes.ReceiptStatus_Success, "", 0, ""},
		{"reverted with reason", iotextypes.ReceiptStatus_ErrExecutionReverted, "boom", debankSimulateErrorReverted, "boom"},
		{"reverted no reason", iotextypes.ReceiptStatus_ErrExecutionReverted, "", debankSimulateErrorReverted, "execution reverted"},
		{"oog", iotextypes.ReceiptStatus_ErrOutOfGas, "", debankSimulateErrorGasExhausted, "out of gas"},
		{"code store oog", iotextypes.ReceiptStatus_ErrCodeStoreOutOfGas, "", debankSimulateErrorGasExhausted, "out of gas"},
		{"gas overflow", iotextypes.ReceiptStatus_ErrGasUintOverflow, "", debankSimulateErrorGasExhausted, "out of gas"},
		{"insufficient balance", iotextypes.ReceiptStatus_ErrInsufficientBalance, "", debankSimulateErrorInsufficientBalance, "insufficient balance"},
		{"not enough balance (200-series)", iotextypes.ReceiptStatus_ErrNotEnoughBalance, "", debankSimulateErrorInsufficientBalance, "insufficient balance"},
		{"unknown evm error", iotextypes.ReceiptStatus_ErrUnknown, "", debankSimulateErrorUnknown, "evm receipt status 100"},
		{"depth exceeded", iotextypes.ReceiptStatus_ErrDepth, "", debankSimulateErrorUnknown, "evm receipt status 103"},
		{"invalid jump", iotextypes.ReceiptStatus_ErrInvalidJump, "", debankSimulateErrorUnknown, "evm receipt status 111"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotMsg := mapReceiptStatusToDebankCode(uint64(tc.status), tc.revert)
			require.Equal(t, tc.wantCod, gotCode)
			require.Equal(t, tc.wantMsg, gotMsg)
		})
	}
}

// TestSimulateBatchToDebank_RevertSuppressesEvents pins Ethereum semantics:
// a reverted tx produces no events. Receipt.Logs() shouldn't leak into the
// wire response under revert.
func TestSimulateBatchToDebank_RevertSuppressesEvents(t *testing.T) {
	require := require.New(t)
	receipt := (&action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_ErrExecutionReverted),
		GasConsumed: 12345,
	}).SetExecutionRevertMsg("oops")
	receipt.AddLogs(&action.Log{
		Address: "io1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqd39ym7",
		Topics:  []hash.Hash256{hash.Hash256b([]byte("Transfer"))},
		Data:    []byte{0xff},
	})

	out := simulateBatchToDebank(&SimulateBatchResult{Receipt: receipt}, 1)
	require.Equal(debankSimulateErrorReverted, out.Code)
	require.Equal("oops", out.Err)
	require.Empty(out.Events, "reverted tx must emit zero events")
}

// TestProtocolAddrSimulateResult_GasUsedZero pins that protocol-address
// synthetic results report gas_used=0 (not 21000) — these calls bypass EVM
// entirely and have no measured gas; downstream must not interpret the
// value as a fee estimate.
func TestProtocolAddrSimulateResult_GasUsedZero(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	amount := big.NewInt(42)
	core.EXPECT().ReadState(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&iotexapi.ReadStateResponse{Data: []byte(amount.String())}, nil)

	rewarding := common.HexToAddress("0xa576c141e5659137ddda4223d209d4744b2106be")
	from := common.HexToAddress("0x0000000000000000000000000000000000000001")
	data := hexutil.Bytes{0xad, 0x7a, 0x67, 0x2f}
	arg := &debankCallArgs{From: &from, To: &rewarding, Data: &data}

	out, ok := web3svr.protocolAddrSimulateResult(arg, 0, 1)
	require.True(ok)
	require.Equal(uint64(0), out.GasUsed, "protocol addr synthetic gas_used must be 0, not 21000")
	require.Equal(0, out.Code)
}

// TestEstimateGasDebank_DebankBlockContextDoesNotPanic locks in the fix
// for the writer-side panic where debank_estimateGas with a `{block_id, type}`
// shaped params.1 used to nil-deref in blockNumberOrHashToHeight.
//
// Pre-fix: parseCallObject called rpc.BlockNumberOrHash.UnmarshalJSON on the
// debank object, silently overwriting the LatestBlockNumber default with both
// fields nil. estimateGas then dereferenced *bn.BlockNumber → HTTP 500 panic.
// Pre-fix the panic only manifested for POLL/ROLLDPOS because the staking and
// rewarding branches in ethTxToEnvelope error out earlier on "invalid abi
// binary data"; for POLL the request fell through to BuildTransfer which
// reached blockNumberOrHashToHeight.
//
// Post-fix: estimateGasDebank resolves the debank block context up front via
// parseDebankBlockContextHeight and rebuilds the params payload eth-style
// before delegating. blockNumberOrHashToHeight has a defensive nil guard as
// belt-and-braces.
func TestEstimateGasDebank_DebankBlockContextDoesNotPanic(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	// Wiring needed by estimateGas → ethTxToEnvelope → checkContractAddr →
	// EstimateGasForNonExecution path. POLL is not a contract and not in
	// the special staking/rewarding branches, so it falls through to
	// BuildTransfer and eventually EstimateGasForNonExecution(*action.Transfer).
	core.EXPECT().EVMNetworkID().Return(uint32(4689)).AnyTimes()
	core.EXPECT().ChainID().Return(uint32(1)).AnyTimes()
	core.EXPECT().Account(gomock.Any()).
		Return(&iotextypes.AccountMeta{IsContract: false}, nil, nil).AnyTimes()
	core.EXPECT().EstimateGasForNonExecution(gomock.Any()).
		Return(uint64(21000), nil).AnyTimes()

	// POLL protocol address — pre-fix HTTP 500 nil-ptr; post-fix returns 21000.
	in := gjson.Parse(`{"params":[
		{
			"from":  "0xd776f4166ac8d757120864398312401b9c24dd0a",
			"to":    "0x166b743c2c1a57c93c2e2bc3e169d28bbb9f6da3",
			"gas":   "0x186a0",
			"value": "0x0",
			"input": "0x12345678"
		},
		{"block_id":"latest","type":"Equals"},
		null
	]}`)
	out, err := web3svr.estimateGasDebank(context.Background(), &in)
	require.NoError(err, "must not panic or error on debank-shape block context")
	require.Equal("0x5208", out, "21000 gas (default for non-execution action)")

	// Same flow with params.1 = null — also previously panicked because
	// rpc.BlockNumberOrHash.UnmarshalJSON of "null" left both fields nil.
	in2 := gjson.Parse(`{"params":[
		{
			"from":  "0xd776f4166ac8d757120864398312401b9c24dd0a",
			"to":    "0x166b743c2c1a57c93c2e2bc3e169d28bbb9f6da3",
			"gas":   "0x186a0",
			"value": "0x0",
			"input": "0x12345678"
		},
		null,
		null
	]}`)
	out2, err := web3svr.estimateGasDebank(context.Background(), &in2)
	require.NoError(err)
	require.Equal("0x5208", out2)
}

// TestSimulateTransactionsDebank_BatchSizeLimit pins the 50-tx cap to
// prevent a single request from monopolising the writer's working set.
func TestSimulateTransactionsDebank_BatchSizeLimit(t *testing.T) {
	require := require.New(t)
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	core := NewMockCoreService(ctrl)
	web3svr := &web3Handler{core, nil, _defaultBatchRequestLimit}

	// Build params with 51 entries (just over the cap)
	var argsBuf strings.Builder
	argsBuf.WriteString(`{"params":[[`)
	for i := 0; i < debankBatchMaxSize+1; i++ {
		if i > 0 {
			argsBuf.WriteString(",")
		}
		argsBuf.WriteString(`{"from":"0x0000000000000000000000000000000000000001","to":"0x7c13866F9253DEf79e20034eDD011e1d69E67fe5","data":"0x"}`)
	}
	argsBuf.WriteString(`],{"block_id":"latest","type":"Equals"}]}`)

	in := gjson.Parse(argsBuf.String())
	_, err := web3svr.simulateTransactionsDebank(context.Background(), &in)
	require.ErrorContains(err, "at most")
}
