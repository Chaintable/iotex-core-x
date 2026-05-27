// Copyright (c) 2026 IoTeX Foundation
// Licensed under Apache License 2.0.

package api

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
)

// Capture* bridge methods preserve the v1.13 EVMLogger surface for tests that
// were written against the original iotexRPCTracer. Production code uses the
// On* hooks via Hooks(); these helpers are test-only and exist so the existing
// 491-line rpc_tracer_test.go suite (PR #9 invariants P9-1..P9-5 coverage)
// continues to compile and run without rewriting every assertion.
//
// Depth handling: v1.15.11 OnEnter/OnExit take an explicit depth parameter;
// the bridge derives it from len(t.stack) at the call site, which matches what
// the original Capture* methods computed internally.

// CaptureTxStart bridges to OnTxStart(nil VMContext, nil tx, zero addr). The
// inner pipeline RPCTracer ignores tx args and uses t.actions[currentIdx], so
// nil is safe when t.actions is unset (the common test setup).
func (t *iotexRPCTracer) CaptureTxStart(_ uint64) {
	t.OnTxStart(nil, nil, common.Address{})
}

// CaptureTxEnd bridges to OnTxEnd(nil receipt, nil err).
func (t *iotexRPCTracer) CaptureTxEnd(_ uint64) {
	t.OnTxEnd(nil, nil)
}

// CaptureStart bridges to OnEnter(0, CALL|CREATE, ...).
func (t *iotexRPCTracer) CaptureStart(_ *vm.EVM, from, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	op := byte(vm.CALL)
	if create {
		op = byte(vm.CREATE)
	}
	t.OnEnter(0, op, from, to, input, gas, value)
}

// CaptureEnd bridges to OnExit(0, output, gasUsed, err, false).
func (t *iotexRPCTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	t.OnExit(0, output, gasUsed, err, false)
}

// CaptureEnter bridges to OnEnter(depth=len(stack), ...). depth tracks the
// depth at which the new frame opens — matches v1.15.11 EVM internal sequence
// (push happens at depth=1 for first sub, 2 for nested, etc.).
//
// Edge case: when stack is empty (CaptureStart never fired), v1.13 contract
// says CaptureEnter is a protocol violation that must panic. We force depth=1
// here so OnEnter's "empty stack on sub-frame depth" panic invariant fires.
func (t *iotexRPCTracer) CaptureEnter(typ vm.OpCode, from, to common.Address, input []byte, gas uint64, value *big.Int) {
	depth := len(t.stack)
	if depth == 0 {
		depth = 1 // force the panic path in OnEnter
	}
	t.OnEnter(depth, byte(typ), from, to, input, gas, value)
}

// CaptureExit bridges to OnExit(depth=len(stack)-1, output, gasUsed, err, false).
//
// Edge case: v1.13 contract says CaptureExit is always for a sub-frame; with
// only root in stack (or empty), it's a protocol violation that must panic. We
// force depth=1 here so OnExit's underflow panic invariant fires (rather than
// my OnExit treating depth=0 as the valid root-frame-exit semantics).
func (t *iotexRPCTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	depth := len(t.stack) - 1
	if depth <= 0 {
		depth = 1 // force the panic path in OnExit
	}
	t.OnExit(depth, output, gasUsed, err, false)
}
