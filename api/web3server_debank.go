package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/pkg/errors"
	"github.com/tidwall/gjson"

	"github.com/iotexproject/iotex-core/v2/action"
)

// simulateTransactionsDebank handles debank_simulateTransactions.
// JSON-RPC params: [args []debankCallArgs, blockContext debankBlockContext, blockOverrides? *debankBlockOverrides]
func (svr *web3Handler) simulateTransactionsDebank(ctx context.Context, in *gjson.Result) (interface{}, error) {
	args, err := parseDebankCallArgsArray(in.Get("params.0"))
	if err != nil {
		return nil, err
	}
	bnh, err := parseDebankBlockContextHeight(in.Get("params.1"))
	if err != nil {
		return nil, err
	}
	height, archive, err := svr.blockNumberOrHashToHeight(bnh)
	if err != nil {
		return nil, err
	}

	// Pre-route protocol addrs through callProtocolAddr (parity with eth_call):
	// these are read-only ABI dispatches that don't touch ws state, so the
	// batch engine skips them and we splice the synthetic result back in.
	batchArgs := make([]SimulateBatchArg, len(args))
	protoOverrides := make([]*debankSingleSimulateResult, len(args))
	for i := range args {
		if override, ok := svr.protocolAddrSimulateResult(&args[i], height, int64(i+1)); ok {
			protoOverrides[i] = override
			batchArgs[i] = SimulateBatchArg{Skip: true}
			continue
		}
		caller, elp, err := buildEnvelopeFromDebankCallArgs(&args[i])
		if err != nil {
			return nil, err
		}
		batchArgs[i] = SimulateBatchArg{Caller: caller, Envelope: elp}
	}

	results, info, err := svr.coreService.SimulateExecutionBatch(ctx, height, archive, batchArgs)
	if err != nil {
		return nil, err
	}

	resp := debankSimulateResp{
		Results: make([]debankSingleSimulateResult, len(results)),
		Stats: debankSimulateStats{
			BlockNum:  info.BlockHeight,
			BlockHash: common.BytesToHash(info.BlockHash[:]),
			BlockTime: info.BlockTime.Unix(),
			Success:   true,
		},
	}
	for i := range results {
		if protoOverrides[i] != nil {
			resp.Results[i] = *protoOverrides[i]
		} else {
			resp.Results[i] = simulateBatchToDebank(&results[i], int64(i+1))
		}
		if resp.Results[i].Code != 0 {
			resp.Stats.Success = false
		}
	}
	return resp, nil
}

// contractMultiCallDebank handles debank_contractMultiCall.
// Read-only batch; protocol addrs route to callProtocolAddr; ordinary calls
// hit coreService.ReadContract. v1 ignores fastFail/useParallel/disableCache
// and the 0xeeee native-token shortcut (leafage handles that path itself).
func (svr *web3Handler) contractMultiCallDebank(ctx context.Context, in *gjson.Result) (interface{}, error) {
	args, err := parseDebankTransactionArgsArray(in.Get("params.0"))
	if err != nil {
		return nil, err
	}
	if len(args) > 50 {
		return nil, errors.New("debank: contractMultiCall accepts at most 50 calls")
	}
	bnh, err := parseDebankBlockContextHeight(in.Get("params.1"))
	if err != nil {
		return nil, err
	}
	height, archive, err := svr.blockNumberOrHashToHeight(bnh)
	if err != nil {
		return nil, err
	}

	results := make([]*debankSingleCallResult, len(args))
	for i := range args {
		results[i] = svr.executeMultiCallOne(ctx, &args[i], height, archive)
	}

	stats := &debankMultiCallStats{
		Success:      true,
		CacheEnabled: false,
	}
	tipHeight := height
	if !archive || tipHeight == 0 {
		tipHeight = svr.coreService.TipHeight()
	}
	stats.BlockNum = tipHeight
	if blkHash, err := svr.coreService.BlockHashByBlockHeight(tipHeight); err == nil {
		stats.BlockHash = common.BytesToHash(blkHash[:])
	}
	for _, r := range results {
		if r.Code != 0 {
			stats.Success = false
			break
		}
	}
	return &debankMultiCallResp{Results: results, Stats: stats}, nil
}

// estimateGasDebank handles debank_estimateGas. Aligned with eth_estimateGas;
// any extra blockContext / blockOverrides params are accepted but ignored.
func (svr *web3Handler) estimateGasDebank(ctx context.Context, in *gjson.Result) (interface{}, error) {
	return svr.estimateGas(ctx, in)
}

// executeMultiCallOne runs one read-only call: protocol addr or contract.
func (svr *web3Handler) executeMultiCallOne(
	ctx context.Context,
	arg *debankTransactionArgs,
	height uint64,
	archive bool,
) *debankSingleCallResult {
	r := &debankSingleCallResult{}
	if arg.To == nil {
		r.Code = debankSimulateErrorUnknown
		r.Err = "debank: missing 'to' address"
		return r
	}
	toAddr, err := address.FromBytes(arg.To.Bytes())
	if err != nil {
		r.Code = debankSimulateErrorUnknown
		r.Err = err.Error()
		return r
	}
	to := toAddr.String()
	var data []byte
	switch {
	case arg.Data != nil:
		data = *arg.Data
	case arg.Input != nil:
		data = *arg.Input
	}

	// Protocol addr routing
	if result, handled, perr := svr.callProtocolAddr(to, data, height); handled {
		if perr != nil {
			r.Code = debankSimulateErrorReverted
			r.Err = perr.Error()
			return r
		}
		raw, decErr := hex.DecodeString(strip0x(result))
		if decErr != nil {
			r.Code = debankSimulateErrorUnknown
			r.Err = decErr.Error()
			return r
		}
		r.Result = raw
		return r
	}

	// Regular EVM read
	caller, err := callerFromDebankFrom(arg.From)
	if err != nil {
		r.Code = debankSimulateErrorUnknown
		r.Err = err.Error()
		return r
	}
	value := big.NewInt(0)
	if arg.Value != nil {
		value = arg.Value.ToInt()
	}
	gasLimit := uint64(0)
	if arg.Gas != nil {
		gasLimit = uint64(*arg.Gas)
	}
	elp := (&action.EnvelopeBuilder{}).
		SetAction(action.NewExecution(to, value, data)).
		SetGasLimit(gasLimit).Build()
	var (
		ret     string
		receipt *iotextypes.Receipt
	)
	if !archive {
		ret, receipt, err = svr.coreService.ReadContract(ctx, caller, elp)
	} else {
		ret, receipt, err = svr.coreService.WithHeight(height).ReadContract(ctx, caller, elp)
	}
	if err != nil {
		r.Code = debankSimulateErrorUnknown
		r.Err = err.Error()
		return r
	}
	if receipt != nil {
		r.GasUsed = int64(receipt.GetGasConsumed())
		if receipt.GetStatus() == uint64(iotextypes.ReceiptStatus_ErrExecutionReverted) {
			r.Code = debankSimulateErrorReverted
			r.Err = receipt.GetExecutionRevertMsg()
			if r.Err == "" {
				r.Err = "execution reverted"
			}
		}
	}
	if raw, decErr := hex.DecodeString(strip0x(ret)); decErr == nil {
		r.Result = raw
	}
	return r
}

// simulateBatchToDebank converts SimulateBatchResult into the wire-level
// per-tx result.  txIdx is 1-based per cosmos-evm convention.
func simulateBatchToDebank(r *SimulateBatchResult, txIdx int64) debankSingleSimulateResult {
	out := debankSingleSimulateResult{
		Traces: []debankTrace{},
		Events: []debankEvent{},
	}
	if r.Err != nil {
		if errors.Is(r.Err, action.ErrInsufficientFunds) {
			out.Code = debankSimulateErrorInsufficientBalance
		} else {
			out.Code = debankSimulateErrorUnknown
		}
		out.Err = r.Err.Error()
		return out
	}
	if r.Receipt == nil {
		return out
	}
	out.GasUsed = r.Receipt.GasConsumed
	if r.Receipt.Status == uint64(iotextypes.ReceiptStatus_ErrExecutionReverted) {
		out.Code = debankSimulateErrorReverted
		out.Err = r.Receipt.ExecutionRevertMsg()
		if out.Err == "" {
			out.Err = "execution reverted"
		}
	}
	txIDHash := common.BigToHash(big.NewInt(txIdx))
	for _, lg := range r.Receipt.Logs() {
		ev := debankEvent{
			Address: ioAddrToEthHex(lg.Address),
			Data:    lg.Data,
			TxId:    txIDHash,
		}
		if len(lg.Topics) > 0 {
			ev.Selector = "0x" + hex.EncodeToString(lg.Topics[0][:])
			ev.Topics = make([]string, 0, len(lg.Topics)-1)
			for _, t := range lg.Topics[1:] {
				ev.Topics = append(ev.Topics, "0x"+hex.EncodeToString(t[:]))
			}
		}
		out.Events = append(out.Events, ev)
	}
	if traces := flattenCallFrames(r.TraceData, txIdx); len(traces) > 0 {
		out.Traces = traces
	}
	return out
}

// protocolAddrSimulateResult routes a CallArg through the protocol-addr ABI
// dispatcher (eth_call parity). Returns (result, true) when `to` is a protocol
// address (regardless of whether the dispatch succeeded), (nil, false) for
// regular contract addresses so the batch engine handles them as EVM.
func (svr *web3Handler) protocolAddrSimulateResult(arg *debankCallArgs, height uint64, txIdx int64) (*debankSingleSimulateResult, bool) {
	if arg.To == nil {
		return nil, false
	}
	toAddr, err := address.FromBytes(arg.To.Bytes())
	if err != nil {
		return nil, false
	}
	var data []byte
	if arg.Data != nil {
		data = *arg.Data
	}
	raw, handled, perr := svr.callProtocolAddr(toAddr.String(), data, height)
	if !handled {
		return nil, false
	}
	txID := common.BigToHash(big.NewInt(txIdx))
	fromHex := "0x0000000000000000000000000000000000000000"
	if arg.From != nil {
		fromHex = "0x" + hex.EncodeToString(arg.From.Bytes())
	}
	toHex := "0x" + hex.EncodeToString(arg.To.Bytes())
	out := &debankSingleSimulateResult{
		GasUsed: 21000,
		Traces:  []debankTrace{},
		Events:  []debankEvent{},
	}
	if perr != nil {
		out.Code = debankSimulateErrorReverted
		out.Err = perr.Error()
		out.Traces = append(out.Traces, debankTrace{
			ID: "0", From: fromHex, To: toHex, Input: data,
			Gas: new(big.Int), GasUsed: new(big.Int),
			CallCreateType: "call", CallType: "STATICCALL",
			TxID: txID,
		})
		return out, true
	}
	rawBytes, _ := hex.DecodeString(strip0x(raw))
	out.Traces = append(out.Traces, debankTrace{
		ID: "0", From: fromHex, To: toHex, Input: data, Output: rawBytes,
		Gas: new(big.Int), GasUsed: new(big.Int),
		CallCreateType: "call", CallType: "STATICCALL",
		TxID: txID,
	})
	return out, true
}

// callFrame mirrors go-ethereum's native callTracer JSON output.
type callFrame struct {
	Type    string          `json:"type"`
	From    common.Address  `json:"from"`
	To      *common.Address `json:"to,omitempty"`
	Value   *hexutil.Big    `json:"value,omitempty"`
	Gas     hexutil.Uint64  `json:"gas"`
	GasUsed hexutil.Uint64  `json:"gasUsed"`
	Input   hexutil.Bytes   `json:"input"`
	Output  hexutil.Bytes   `json:"output,omitempty"`
	Error   string          `json:"error,omitempty"`
	Revert  string          `json:"revertReason,omitempty"`
	Calls   []callFrame     `json:"calls,omitempty"`
}

// flattenCallFrames decodes callTracer JSON into a flat []debankTrace ordered
// depth-first. Frame IDs are path-style ("0", "0_1", "0_1_0") so the parent_id
// link is reconstructable from the ID alone.
func flattenCallFrames(data json.RawMessage, txIdx int64) []debankTrace {
	if len(data) == 0 {
		return nil
	}
	var root callFrame
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	txID := common.BigToHash(big.NewInt(txIdx))
	var out []debankTrace
	var walk func(f *callFrame, id, parent string, pos int64)
	walk = func(f *callFrame, id, parent string, pos int64) {
		out = append(out, callFrameToDebankTrace(f, id, parent, pos, txID))
		for j := range f.Calls {
			walk(&f.Calls[j], id+"_"+strconv.FormatInt(int64(j), 10), id, int64(j))
		}
	}
	walk(&root, "0", "", 0)
	return out
}

func callFrameToDebankTrace(f *callFrame, id, parentID string, pos int64, txID common.Hash) debankTrace {
	var toStr string
	if f.To != nil {
		toStr = "0x" + hex.EncodeToString(f.To.Bytes())
	}
	cct, ct := classifyCallType(f.Type)
	return debankTrace{
		ID:               id,
		From:             "0x" + hex.EncodeToString(f.From.Bytes()),
		Gas:              new(big.Int).SetUint64(uint64(f.Gas)),
		Input:            f.Input,
		To:               toStr,
		Value:            f.Value,
		GasUsed:          new(big.Int).SetUint64(uint64(f.GasUsed)),
		Output:           f.Output,
		CallCreateType:   cct,
		CallType:         ct,
		TxID:             txID,
		ParentTraceID:    parentID,
		PosInParentTrace: pos,
	}
}

func classifyCallType(t string) (string, string) {
	switch t {
	case "CREATE":
		return "create", "CREATE"
	case "CREATE2":
		return "create2", "CREATE2"
	case "SELFDESTRUCT":
		return "suicide", "SELFDESTRUCT"
	default:
		return "call", t
	}
}

// buildEnvelopeFromDebankCallArgs converts a debankCallArgs into an Envelope
// + caller for SimulateExecutionBatch.
func buildEnvelopeFromDebankCallArgs(arg *debankCallArgs) (address.Address, action.Envelope, error) {
	caller, err := callerFromDebankFrom(arg.From)
	if err != nil {
		return nil, nil, err
	}
	if arg.To == nil {
		return nil, nil, errors.New("debank simulate: missing 'to'")
	}
	toAddr, err := address.FromBytes(arg.To.Bytes())
	if err != nil {
		return nil, nil, err
	}
	value := big.NewInt(0)
	if arg.Value != nil {
		value = arg.Value.ToInt()
	}
	var data []byte
	if arg.Data != nil {
		data = *arg.Data
	}
	gasLimit := uint64(0)
	if arg.Gas != nil {
		gasLimit = uint64(*arg.Gas)
	}
	elp := (&action.EnvelopeBuilder{}).
		SetAction(action.NewExecution(toAddr.String(), value, data)).
		SetGasLimit(gasLimit).Build()
	return caller, elp, nil
}

func callerFromDebankFrom(from *common.Address) (address.Address, error) {
	if from == nil {
		return address.FromString(address.ZeroAddress)
	}
	return address.FromBytes(from.Bytes())
}

func ioAddrToEthHex(ioAddr string) string {
	if ioAddr == "" {
		return "0x0000000000000000000000000000000000000000"
	}
	a, err := address.FromString(ioAddr)
	if err != nil {
		return "0x0000000000000000000000000000000000000000"
	}
	return "0x" + hex.EncodeToString(a.Bytes())
}

func parseDebankCallArgsArray(params gjson.Result) ([]debankCallArgs, error) {
	if !params.IsArray() {
		return nil, errors.New("debank: params[0] must be array of call args")
	}
	var out []debankCallArgs
	if err := json.Unmarshal([]byte(params.Raw), &out); err != nil {
		return nil, fmt.Errorf("debank: failed to parse args[]: %w", err)
	}
	return out, nil
}

func parseDebankTransactionArgsArray(params gjson.Result) ([]debankTransactionArgs, error) {
	if !params.IsArray() {
		return nil, errors.New("debank: params[0] must be array of transaction args")
	}
	var out []debankTransactionArgs
	if err := json.Unmarshal([]byte(params.Raw), &out); err != nil {
		return nil, fmt.Errorf("debank: failed to parse args[]: %w", err)
	}
	return out, nil
}

func parseDebankBlockContextHeight(params gjson.Result) (rpc.BlockNumberOrHash, error) {
	latest := rpc.LatestBlockNumber
	def := rpc.BlockNumberOrHashWithNumber(latest)
	if !params.Exists() || params.Type == gjson.Null {
		return def, nil
	}
	var bc debankBlockContext
	if err := json.Unmarshal([]byte(params.Raw), &bc); err != nil {
		return def, fmt.Errorf("debank: failed to parse block context: %w", err)
	}
	if bc.BlockType == debankBlockTypeContains {
		return def, nil
	}
	return bc.BlockId, nil
}

func strip0x(s string) string {
	if len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return s[2:]
	}
	return s
}
