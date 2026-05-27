// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package evm

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-address/address"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

// Compile-time guarantees that the three EVM stateDB adapter types satisfy
// v1.15.11 tracing.StateDB. Required so that VMContext.StateDB can be wired
// at OnTxStart fire points (see HIGH-1 report); without these, v1.15.11
// generic tracers (StructLogger / prestateTracer) hit nil-deref on
// env.StateDB.GetRefund() / GetNonce() etc.
//
// tracing.StateDB is a strict subset of vm.StateDB (8 methods: GetBalance /
// GetNonce / GetCode / GetCodeHash / GetState / GetTransientState / Exist /
// GetRefund), all of which *StateDBAdapter already implements; the Erigon
// adapters inherit them via embedding.
var (
	_ tracing.StateDB = (*StateDBAdapter)(nil)
	_ tracing.StateDB = (*ErigonStateDBAdapter)(nil)
	_ tracing.StateDB = (*ErigonStateDBAdapterDryrun)(nil)
)

// PrepareTracingStateDB builds a fresh tracing.StateDB adapter over the given
// workingSet (sm). Used at OnTxStart fire points (state/factory/workingset.go
// runAction + this package's TraceStart) to populate VMContext.StateDB so
// generic v1.15.11 tracers don't nil-deref. Returns nil on error — caller
// degrades to nil StateDB which is the prior buggy behavior, but keeps the
// path running (logged so panic-on-deref still surfaces via the test failure
// rather than silently going wrong).
//
// The returned StateDB shadows the one EVM constructs internally during
// execution; both wrap the same workingSet so they observe identical state.
// Slight allocation overhead per fire (one adapter struct + maps); not on the
// EVM hot path.
func PrepareTracingStateDB(ctx context.Context, sm protocol.StateManager) tracing.StateDB {
	sdb, err := prepareStateDB(ctx, sm)
	if err != nil {
		log.S().Warn("PrepareTracingStateDB: prepareStateDB failed, VMContext.StateDB will be nil", zap.Error(err))
		return nil
	}
	if adapter, ok := sdb.(tracing.StateDB); ok {
		return adapter
	}
	log.S().Warn("PrepareTracingStateDB: prepareStateDB result does not satisfy tracing.StateDB (impossible per compile-time assertions); returning nil")
	return nil
}

// TraceCleanup is the deferred closure returned by TraceStart. The caller MUST
// invoke it exactly once (typically via defer) to keep the underlying tracer's
// OnEnter/OnExit + OnTxStart/OnTxEnd pairs balanced.
//
// Pass the final receipt when the action completed:
//   - receipt != nil: full end-of-action sequence (EmitTransferLogs, OnExit(0)
//     for non-Execution, OnTxEnd, CaptureTx callback).
//   - receipt == nil: minimal frame-close sequence — DiscardPendingLogs (drop
//     anything OnLog buffered during the failed attempt) plus OnExit(0) +
//     OnTxEnd with an explicit error. The CaptureTx callback is skipped because
//     the downstream rpcTracer.OnTxEnd treats a nil receipt as a fatal abort.
type TraceCleanup func(receipt *action.Receipt)

// noopTraceCleanup is returned when no tracer is installed or when TraceStart
// took the non-eth-compatible path that already paired OnTxStart/OnTxEnd
// internally.
func noopTraceCleanup(*action.Receipt) {}

// TraceStart starts tracing the execution of the action in the sealed envelope
// and returns a cleanup closure that the caller MUST invoke (via defer) before
// the enclosing runAction returns. See TraceCleanup's doc for the semantics of
// nil vs. non-nil receipt.
//
// This drives vm.Config.Tracer (a *tracing.Hooks set by trace_debankBlock /
// SimulateExecutionBatch via vmConfig); the chain's pipeline hooks ctx is
// dispatched separately by state/factory/workingset.go around runAction.
func TraceStart(ctx context.Context, ws protocol.StateManager, elp action.TxDataForSimulation) (TraceCleanup, error) {
	vmCfg, ok := protocol.GetVMConfigCtx(ctx)
	if !ok || vmCfg.Tracer == nil {
		return noopTraceCleanup, nil
	}
	hooks := vmCfg.Tracer

	evmInst, err := newEVM(ctx, ws, elp)
	if err != nil {
		return noopTraceCleanup, errors.Wrap(err, "failed to create EVM instance for tracing")
	}
	_ = evmInst // keep EVM alive for the tracing scope; some hooks may consult it later

	blkCtx := protocol.MustGetBlockCtx(ctx)
	vmCtx := &tracing.VMContext{
		Coinbase:    common.BytesToAddress(blkCtx.Producer.Bytes()),
		BlockNumber: new(big.Int).SetUint64(blkCtx.BlockHeight),
		Time:        uint64(blkCtx.BlockTimeStamp.Unix()),
		Random:      nil, // Rolldpos: no PoW Random
		BaseFee:     blkCtx.BaseFee,
		// StateDB: wire a real tracing.StateDB adapter wrapping workingSet (HIGH-1).
		// iotex's inner PipelineTracer / iotexRPCTracer don't read env.StateDB,
		// but v1.15.11 generic tracers (StructLogger / prestateTracer / native
		// prestateTracer) cache env at OnTxStart then deref env.StateDB.GetRefund()
		// / GetNonce() in later hooks — nil would panic. The adapter shadows EVM's
		// own internal stateDB; both wrap the same ws so behavior is consistent.
		StateDB: PrepareTracingStateDB(ctx, ws),
	}

	actCtx := protocol.MustGetActionCtx(ctx)
	from := common.BytesToAddress(actCtx.Caller.Bytes())

	// OnTxStart for vmConfig.Tracer side. iotexRPCTracer ignores the tx arg and
	// uses its own actions[currentIdx]; downstream tracers that consult tx will
	// need to fetch it via a tracer-specific helper (out of scope here).
	if hooks.OnTxStart != nil {
		hooks.OnTxStart(vmCtx, nil, from)
	}

	var (
		to    *common.Address
		value = big.NewInt(0)
		input = elp.Data()
	)
	switch a := elp.Action().(type) {
	case action.EthCompatibleAction:
		to, err = a.EthTo()
		if err != nil {
			return noopTraceCleanup, errors.Wrap(err, "failed to get eth-compatible action To address")
		}
		if elp.Value() != nil {
			value = elp.Value()
		}
		input, err = a.EthData()
		if err != nil {
			return noopTraceCleanup, errors.Wrap(err, "failed to get eth-compatible action data")
		}
	default:
		// Non-eth-compatible action (e.g. PutPollResult). Fire OnTxEnd
		// synchronously so the tracer's currentIdx advances even though no
		// receipt will be processed — this keeps actions[currentIdx] aligned
		// with the real action list. No cleanup closure is needed.
		if hooks.OnTxEnd != nil {
			hooks.OnTxEnd(nil, nil)
		}
		return noopTraceCleanup, nil
	}

	_, isExecution := elp.Action().(*action.Execution)

	// For non-Execution but eth-compatible actions (e.g. StakeCreate that may
	// internally invoke EVM via ExecuteContract), fire OnEnter(0) manually:
	// EVM's internal evm.Call/Create only fires OnEnter for its own frames; it
	// never fires the outer iotex-action frame. Without this manual fire,
	// inner frames are attached to depth=0 with no parent context.
	//
	// For Execution actions, the outer frame IS EVM's depth=0 call — letting
	// EVM fire OnEnter(0) avoids double-emitting the root frame.
	if !isExecution {
		var dest common.Address
		if to != nil {
			dest = *to
		}
		if hooks.OnEnter != nil {
			hooks.OnEnter(0, byte(vm.CALL), from, dest, input, elp.Gas(), value)
		}
	}

	// Cleanup closure: only the final return value of receipt determines which
	// path runs. The caller defers it; it MUST run exactly once.
	return func(receipt *action.Receipt) {
		if receipt != nil {
			// Success path.
			//
			// CaptureTx fires BEFORE convertReceipt + OnTxEnd so the caller has
			// a chance to populate receipt.TxIndex (and any other index-derived
			// fields) on the iotex receipt. Downstream convertReceipt then maps
			// those fields onto the geth receipt, and pipeline's
			// BuildPipelineTransaction (used by OnTxEnd) writes them into
			// block_file.txs[*]. Without this ordering, every tx in the
			// pipeline output shows TransactionIndex (json: "idx") = 0 because
			// receipt.TxIndex stays at the zero value until the per-action
			// updateReceiptIndex pass runs at block-commit time — a pass that
			// never runs in the trace_debankBlock dryrun path.
			output := receipt.Output
			if t, ok := GetTracerCtx(ctx); ok && t.CaptureTx != nil {
				t.CaptureTx(output, receipt)
			}
			gethReceipt := convertReceipt(receipt)
			if t, ok := GetTracerCtx(ctx); ok && t.EmitTransferLogs != nil {
				// For non-Execution actions, receipt.Logs() never reached the
				// tracer via OnLog (handler does not go through EVM). Re-emit
				// only in that case. TransactionLogs (synthetic GRANT_REWARD /
				// GAS_FEE / ...) are never produced by EVM and must be emitted
				// for every action type.
				t.EmitTransferLogs(receipt, !isExecution)
			}
			// Fire OnExit(0) only for non-Execution: EVM's internal exit
			// already fired OnExit(0) for Execution actions.
			if !isExecution && hooks.OnExit != nil {
				hooks.OnExit(0, output, receipt.GasConsumed, nil, false)
			}
			if hooks.OnTxEnd != nil {
				hooks.OnTxEnd(gethReceipt, nil)
			}
			return
		}
		// Failure path (Simulate mode skipped this action).
		if t, ok := GetTracerCtx(ctx); ok && t.DiscardPendingLogs != nil {
			t.DiscardPendingLogs()
		}
		// Emit an empty state-diff record so evmDiffs keeps one entry per
		// action attempt; downstream mergeStateDiffs is key-based, so empty
		// maps are a clean no-op. Preserves the invariant
		// "len(evmDiffs) == number of action attempts".
		if t, ok := GetTracerCtx(ctx); ok && t.CaptureStateDiff != nil {
			t.CaptureStateDiff(
				map[common.Hash]struct{}{},
				map[common.Hash][]byte{},
				map[common.Hash]map[common.Hash][]byte{},
				map[common.Hash][]byte{},
			)
		}
		// Force OnExit(0) so the tracer's frame depth returns to 0; without
		// this, OnLog fired by the NEXT action can land on the abandoned frame.
		simulateErr := errors.New("simulate skip")
		if hooks.OnExit != nil {
			hooks.OnExit(0, nil, 0, simulateErr, false)
		}
		if hooks.OnTxEnd != nil {
			hooks.OnTxEnd(nil, simulateErr)
		}
	}, nil
}

// ToEthReceipt converts an iotex action.Receipt to a geth types.Receipt.
// Public so tests and upstream-v2.4.1 callers (web3server_test.go) can use it.
// Local copy (not blockchain.ConvertToGethReceipt) to avoid a blockchain → evm
// import cycle.
func ToEthReceipt(receipt *action.Receipt) *types.Receipt {
	return convertReceipt(receipt)
}

// convertReceipt mirrors blockchain.ConvertToGethReceipt but is kept local to
// avoid a blockchain → evm import cycle. The signature surface is small enough
// that duplication is cheaper than refactoring the shared helper into a leaf
// package. Keep in sync with blockchain/pipeline_convert.go:ConvertToGethReceipt.
func convertReceipt(receipt *action.Receipt) *types.Receipt {
	if receipt == nil {
		return nil
	}
	r := &types.Receipt{
		Status:            receipt.Status,
		GasUsed:           receipt.GasConsumed,
		BlobGasUsed:       receipt.BlobGasUsed,
		BlobGasPrice:      receipt.BlobGasPrice,
		TxHash:            common.BytesToHash(receipt.ActionHash[:]),
		BlockNumber:       new(big.Int).SetUint64(receipt.BlockHeight),
		TransactionIndex:  uint(receipt.TxIndex),
		EffectiveGasPrice: receipt.EffectiveGasPrice,
	}
	if receipt.ContractAddress != "" {
		if addr, err := address.FromString(receipt.ContractAddress); err == nil {
			r.ContractAddress = common.BytesToAddress(addr.Bytes())
		}
	}
	for _, l := range receipt.Logs() {
		ethLog := &types.Log{
			Data:        l.Data,
			BlockNumber: l.BlockHeight,
			TxHash:      common.BytesToHash(l.ActionHash[:]),
			TxIndex:     uint(l.TxIndex),
			Index:       uint(l.Index),
		}
		if addr, err := address.FromString(l.Address); err == nil {
			ethLog.Address = common.BytesToAddress(addr.Bytes())
		}
		for _, topic := range l.Topics {
			ethLog.Topics = append(ethLog.Topics, common.BytesToHash(topic[:]))
		}
		r.Logs = append(r.Logs, ethLog)
	}
	return r
}

func newEVM(ctx context.Context, sm protocol.StateManager, execution action.TxData) (*vm.EVM, error) {
	// v2.4.1 upstream prepareStateDB (evm.go:488) already wraps Erigon
	// (dryrun -> *ErigonStateDBAdapterDryrun, normal -> *ErigonStateDBAdapter).
	// Fork v2.3.8 prepareStateDB returned bare *StateDBAdapter so the original
	// newEVM here had to wrap Erigon a second time; with v2.4.1 that becomes a
	// double-wrap and the cast stateDB.(*StateDBAdapter) panics because stateDB
	// is already the Erigon dryrun wrapper. Removing the redundant block fixes
	// the trace_debankBlock dryrun panic (see docs/v2.4.1-plan/6-2-test-report.md).
	stateDB, err := prepareStateDB(ctx, sm)
	if err != nil {
		return nil, err
	}
	evmParams, err := newParams(ctx, execution)
	if err != nil {
		return nil, err
	}
	evm := vm.NewEVM(evmParams.context, stateDB, evmParams.chainConfig, evmParams.evmConfig)
	return evm, nil
}
