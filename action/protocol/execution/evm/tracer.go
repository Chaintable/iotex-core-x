package evm

import (
	"context"
	"math/big"

	erigonstate "github.com/erigontech/erigon/core/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

type tracerWrapper struct {
	vm.EVMLogger
	depth int
}

// NewTracerWrapper wraps the EVMLogger
func NewTracerWrapper(tracer vm.EVMLogger) vm.EVMLogger {
	return &tracerWrapper{EVMLogger: tracer}
}

func (tw *tracerWrapper) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	tw.depth++
	if tw.depth > 1 {
		op := vm.CALL
		if create {
			op = vm.CREATE
		}
		tw.EVMLogger.CaptureEnter(op, from, to, input, gas, value)
		return
	}
	tw.EVMLogger.CaptureStart(env, from, to, create, input, gas, value)
}

func (tw *tracerWrapper) CaptureEnd(output []byte, gasUsed uint64, err error) {
	if tw.depth < 1 {
		return
	}
	defer func() { tw.depth-- }()
	if tw.depth > 1 {
		tw.EVMLogger.CaptureExit(output, gasUsed, err)
		return
	}
	tw.EVMLogger.CaptureEnd(output, gasUsed, err)
}

func (tw *tracerWrapper) Unwrap() vm.EVMLogger {
	return tw.EVMLogger
}

// TraceCleanup runs the tracing pair of CaptureTxStart (for eth-compatible
// actions). The cleanup closure MUST be called exactly once per successful
// TraceStart to keep the tracer's CaptureTxStart/CaptureTxEnd symmetric — the
// pipeline tracer's internal state (txStarted flag, currentIdx counter, log
// buffer) depends on the pair being balanced, and a missed CaptureTxEnd
// silently corrupts every subsequent action's trace.
//
// Pass the final receipt when the action completed:
//   - receipt != nil: run the full end-of-action sequence (CaptureEnd,
//     EmitTransferLogs synthesising native transfer logs from
//     receipt.TransactionLogs, CaptureTxEnd, and the CaptureTx callback).
//   - receipt == nil: run the minimal frame-close sequence to balance the
//     pair — DiscardPendingLogs (drop anything OnLog buffered during the
//     failed attempt) plus CaptureTxEnd with a full gas refund. The
//     CaptureTx callback is intentionally skipped because the downstream
//     rpcTracer.OnTxEnd treats a nil receipt as a fatal abort.
type TraceCleanup func(receipt *action.Receipt)

// noopTraceCleanup is returned when no tracer is installed or when
// TraceStart took the non-eth-compatible path that already paired
// CaptureTxStart/CaptureTxEnd internally.
func noopTraceCleanup(*action.Receipt) {}

// TraceStart starts tracing the execution of the action in the sealed
// envelope and returns a cleanup closure that the caller MUST invoke (via
// defer) before the enclosing runAction returns. See TraceCleanup's doc for
// the semantics of nil vs. non-nil receipt.
func TraceStart(ctx context.Context, ws protocol.StateManager, elp action.Envelope) (TraceCleanup, error) {
	vmCtx, vmCtxExist := protocol.GetVMConfigCtx(ctx)
	if !vmCtxExist || vmCtx.Tracer == nil {
		return noopTraceCleanup, nil
	}
	evm, err := newEVM(ctx, ws, elp)
	if err != nil {
		return noopTraceCleanup, errors.Wrap(err, "failed to create EVM instance for tracing")
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
			return noopTraceCleanup, errors.Wrap(err, "failed to get eth compatible action to address")
		}
		if elp.Value() != nil {
			value = elp.Value()
		}
		input, err = a.EthData()
		if err != nil {
			return noopTraceCleanup, errors.Wrap(err, "failed to get eth compatible action data")
		}
	default:
		// Non-eth-compatible action (e.g. PutPollResult). CaptureTxStart and
		// CaptureTxEnd are paired here synchronously so the tracer's currentIdx
		// advances even though no receipt will be processed — this keeps
		// actions[currentIdx] aligned with the real action list. No cleanup
		// closure is needed because the pair is already closed.
		vmCtx.Tracer.CaptureTxStart(elp.Gas())
		vmCtx.Tracer.CaptureTxEnd(elp.Gas())
		return noopTraceCleanup, nil
	}
	vmCtx.Tracer.CaptureTxStart(elp.Gas())
	_, isExecution := elp.Action().(*action.Execution)
	if !isExecution {
		actCtx := protocol.MustGetActionCtx(ctx)
		vmCtx.Tracer.CaptureStart(evm, common.Address(actCtx.Caller.Bytes()), *to, false, input, elp.Gas(), value)
	}
	// For Execution actions, CaptureStart fires inside evm.executeInEVM.
	//
	// Build the cleanup closure: it captures the ctx/elp/tracer and the
	// isExecution flag. The caller defers it, and only runAction's final
	// return value of receipt determines which path runs.
	return func(receipt *action.Receipt) {
		if receipt != nil {
			// Success path — run the full end-of-action sequence.
			output := receipt.Output
			vmCtx.Tracer.CaptureEnd(output, receipt.GasConsumed, nil)
			if t, ok := GetTracerCtx(ctx); ok && t.EmitTransferLogs != nil {
				// For non-Execution actions the handler does not go through
				// EVM, so its receipt.Logs() never reached the tracer via
				// OnLog. Re-emit them only in that case. TransactionLogs
				// (synthetic GRANT_REWARD/GAS_FEE/...) are never produced by
				// the EVM and must be emitted for every action type.
				t.EmitTransferLogs(receipt, !isExecution)
			}
			vmCtx.Tracer.CaptureTxEnd(elp.Gas() - receipt.GasConsumed)
			if t, ok := GetTracerCtx(ctx); ok && t.CaptureTx != nil {
				t.CaptureTx(output, receipt)
			}
			return
		}
		// Failure path (Simulate mode skipped this action). Drop anything
		// the tracer buffered during the failed run so it does not leak into
		// the next action, then close the frame with full gas refund to
		// balance CaptureTxStart. Do NOT call CaptureTx — downstream treats
		// a nil receipt as fatal.
		if t, ok := GetTracerCtx(ctx); ok && t.DiscardPendingLogs != nil {
			t.DiscardPendingLogs()
		}
		// If EVM's CaptureStart fired but CaptureEnd did not (e.g. Execution
		// failed mid-execution), the tracerWrapper depth is still > 0 and
		// the pipeline tracer's captureStarted flag is still true. Force a
		// CaptureEnd with a zero-output / zero-gas sentinel so depth returns
		// to 0 and captureStarted clears; without this, OnLog fired by the
		// NEXT action can end up attributed to the abandoned frame.
		vmCtx.Tracer.CaptureEnd(nil, 0, errors.New("simulate skip"))
		vmCtx.Tracer.CaptureTxEnd(elp.Gas())
	}, nil
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
	evm := vm.NewEVM(evmParams.context, evmParams.txCtx, stateDB, evmParams.chainConfig, evmParams.evmConfig)
	return evm, nil
}
