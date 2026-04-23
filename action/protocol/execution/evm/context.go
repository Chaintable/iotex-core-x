package evm

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

type (
	helperContextKey struct{}

	tracerContextKey struct{}

	// HelperContext is the context for EVM helper
	HelperContext struct {
		GetBlockHash   GetBlockHash
		GetBlockTime   GetBlockTime
		DepositGasFunc protocol.DepositGas
	}
	// TracerContext is the context for EVM tracer
	TracerContext struct {
		CaptureTx func([]byte, *action.Receipt)
		// CaptureStateDiff is called per-action before CommitContracts/clear to capture EVM storage/code diffs.
		CaptureStateDiff func(
			destructs map[common.Hash]struct{},
			accounts map[common.Hash][]byte,
			storages map[common.Hash]map[common.Hash][]byte,
			codes map[common.Hash][]byte,
		)
		// OnLog is called when a log is emitted during EVM execution (for trace_debankBlock events).
		OnLog func(*types.Log)
		// EmitTransferLogs is called after CaptureEnd but before CaptureTxEnd so that
		// native IoTeX TransactionLogs (GRANT_REWARD, CLAIM_FROM_REWARDING, GAS_FEE,
		// BUCKET_CREATE_AMOUNT, etc.) can be pushed as events and included in the
		// tracer's addTraceAndLog pass. includeEVMLogs is true for non-Execution
		// actions where the EVM did not run (so receipt.Logs() must be re-emitted);
		// Execution callers pass false because OnLog already captured EVM logs.
		EmitTransferLogs func(receipt *action.Receipt, includeEVMLogs bool)
		// DiscardPendingLogs tells the tracer to drop any logs buffered for the
		// currently open tx frame without flushing them to the trace output.
		// Called by TraceStart's cleanup closure on the Simulate-skip path so a
		// failed action's partial logs never leak into the next action's events.
		// Nil-safe — callers must check before invoking.
		DiscardPendingLogs func()
	}
)

// WithHelperCtx returns a new context with helper context
func WithHelperCtx(ctx context.Context, hctx HelperContext) context.Context {
	return context.WithValue(ctx, helperContextKey{}, hctx)
}

// mustGetHelperCtx returns the helper context from the context
func mustGetHelperCtx(ctx context.Context) HelperContext {
	hc, ok := ctx.Value(helperContextKey{}).(HelperContext)
	if !ok {
		log.S().Panic("Miss evm helper context")
	}
	return hc
}

// WithTracerCtx returns a new context with tracer context
func WithTracerCtx(ctx context.Context, tctx TracerContext) context.Context {
	return context.WithValue(ctx, tracerContextKey{}, tctx)
}

// GetTracerCtx returns the tracer context from the context
func GetTracerCtx(ctx context.Context) (TracerContext, bool) {
	tc, ok := ctx.Value(tracerContextKey{}).(TracerContext)
	return tc, ok
}
