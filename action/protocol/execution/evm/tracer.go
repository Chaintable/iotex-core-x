// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package evm

import (
	"context"
	"math/big"

	erigonstate "github.com/erigontech/erigon/core/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-address/address"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

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
		StateDB:     nil, // pipeline RPCTracer does not introspect
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
			gethReceipt := convertReceipt(receipt)
			output := receipt.Output
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
			if t, ok := GetTracerCtx(ctx); ok && t.CaptureTx != nil {
				t.CaptureTx(output, receipt)
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
	var stateDB stateDB
	stateDB, err := prepareStateDB(ctx, sm)
	if err != nil {
		return nil, err
	}
	if erigonsm, ok := sm.(interface {
		Erigon() (*erigonstate.IntraBlockState, bool)
	}); ok {
		if in, dryrun := erigonsm.Erigon(); in != nil {
			if !dryrun {
				log.S().Panic("should not happen, use dryrun instead")
			}
			stateDB = NewErigonStateDBAdapterDryrun(stateDB.(*StateDBAdapter), in)
		}
	}
	evmParams, err := newParams(ctx, execution)
	if err != nil {
		return nil, err
	}
	evm := vm.NewEVM(evmParams.context, stateDB, evmParams.chainConfig, evmParams.evmConfig)
	return evm, nil
}
