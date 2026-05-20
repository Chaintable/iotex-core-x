// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package api

import (
	"encoding/hex"
	"math/big"

	ptracer "github.com/Chaintable/pipeline/tracer"
	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"go.uber.org/zap"
)

var _ vm.EVMLogger = (*iotexRPCTracer)(nil)

// pendingLog buffers the OnLog / EmitTransferLog snapshot taken during EVM
// execution: the log itself plus the originating frame's trace_address path
// and Position value at the OnLog moment. flushPendingLogs uses these
// snapshots to call inner.InsertLog so the log is physically attached to the
// originating sub-frame; without the snapshot, all logs would land on the root
// frame (because every sub-frame has been CaptureExit'd by flush time).
type pendingLog struct {
	log          *types.Log
	traceAddress []int64
	position     int64
}

// frameCtx tracks how many sub-calls have already finalized into the frame's
// parent.Calls and how many logs OnLog has staged for the frame, both since
// CaptureStart/CaptureEnter opened it. Used to compute Position
// (childCount + logCount) at OnLog time and to derive the next child's
// trace_address index at CaptureEnter time.
type frameCtx struct {
	childCount int64
	logCount   int64
}

// iotexRPCTracer wraps pipeline's RPCTracer to bridge IoTeX's action-based execution
// model (CaptureTxStart without tx info) to RPCTracer's OnTxStart(tx, from).
//
// IoTeX calls CaptureTxStart twice for *action.Execution:
// once in TraceStart and once in executeInEVM. txStarted prevents double init.
//
// OnLog buffering: logs emitted between CaptureTxStart and CaptureTxEnd are
// staged in pendingLogs instead of forwarded to inner.OnLog immediately.
// This lets Simulate-mode skip paths call DiscardPendingLogs to drop logs
// from a failed action before CaptureTxEnd runs, preventing them from
// leaking into the next action's log stream. Normal (success) paths flush
// pendingLogs inside CaptureTxEnd. logIndex advances only at flush time so
// a discarded action leaves no gap in the global index sequence.
//
// Frame-level attribution: the inner callTracer's callstack at flush time is
// collapsed back to root (every sub-frame already CaptureExit'd into its
// parent.Calls). So OnLog records the originating frame's trace_address path
// and Position at the moment the log fires, by maintaining a parallel stack
// (stack/path below) driven by the same CaptureEnter/CaptureExit signals the
// callTracer sees. flushPendingLogs then calls inner.InsertLog with the
// snapshotted path so each log is physically attached to the originating
// sub-frame; otherwise OnTxEnd's addTraceAndLog would walk every log under
// root.TraceID and yield event.parent_trace_id = root for everything.
type iotexRPCTracer struct {
	inner *ptracer.RPCTracer

	// pre-computed action list for CaptureTxStart → OnTxStart bridging
	actions    []*action.SealedEnvelope
	currentIdx int
	chainID    uint32 // for constructing signed eth tx hash
	txStarted      bool // guards against double CaptureTxStart for Execution actions
	captureStarted bool // true after CaptureStart, safe to call OnLog
	logIndex       uint // global log index counter within block

	// pendingLogs buffers OnLog / EmitTransferLog calls for the currently open
	// tx frame, paired with the (traceAddress, position) snapshot taken at the
	// emit moment. Flushed via inner.InsertLog on normal CaptureTxEnd; dropped
	// by DiscardPendingLogs on the Simulate-skip path.
	pendingLogs []pendingLog

	// stack and path mirror the inner callTracer's call stack one entry per
	// active frame (stack[0] = root). path holds the trace_address of the
	// frame currently on top, i.e. len(path) == len(stack) - 1 except during
	// transient hook transitions. CaptureEnter / CaptureExit keep them in sync
	// with the EVM's actual call depth; OnLog snapshots them.
	stack []frameCtx
	path  []int64
}

func newIotexRPCTracer(chainID uint32) *iotexRPCTracer {
	return &iotexRPCTracer{
		inner:   &ptracer.RPCTracer{},
		chainID: chainID,
	}
}

// SetActions pre-registers the action list. CaptureTxStart will use currentIdx
// to look up the corresponding ethTx and call inner.OnTxStart.
func (t *iotexRPCTracer) SetActions(actions []*action.SealedEnvelope) {
	t.actions = actions
	t.currentIdx = 0
	t.logIndex = 0
}

func (t *iotexRPCTracer) OnBlockStart(block *types.Block) {
	t.inner.OnBlockStart(block)
}

// OnTxEnd finalizes the current transaction trace via the inner RPCTracer.
func (t *iotexRPCTracer) OnTxEnd(receipt *types.Receipt, err error) {
	t.inner.OnTxEnd(receipt, err)
}

func (t *iotexRPCTracer) GetOutPut(originRoot, root common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storages map[common.Hash]map[common.Hash][]byte, codes map[common.Hash][]byte) *ptypes.DebankOutPut {
	return t.inner.GetOutPut(originRoot, root, destructs, accounts, storages, codes)
}

// vm.EVMLogger interface implementation

func (t *iotexRPCTracer) CaptureTxStart(gasLimit uint64) {
	if t.txStarted {
		// Already initialized by TraceStart, skip duplicate call from executeInEVM
		return
	}
	t.txStarted = true
	// Safety net: reset the parallel stack/path in case a prior tx left residue
	// (e.g. EVM faulted between CaptureEnter and the corresponding CaptureExit
	// in some pathological scenario). CaptureStart fills root in immediately.
	t.stack = t.stack[:0]
	t.path = t.path[:0]
	// Bridge: look up pre-computed ethTx by index, call inner.OnTxStart
	if t.currentIdx < len(t.actions) {
		selp := t.actions[t.currentIdx]
		rawTx, err := selp.ToEthTx()
		if err != nil {
			// non-EVM action — skip OnTxStart but keep txStarted=true
			return
		}
		// construct signed tx so tx hash matches eth_getBlockByNumber
		signer, err := action.NewEthSigner(iotextypes.Encoding(selp.Encoding()), t.chainID)
		if err != nil {
			log.L().Debug("failed to create eth signer for debankBlock", zap.Error(err))
			// fallback to unsigned tx
			senderAddr := selp.SenderAddress()
			from := common.BytesToAddress(senderAddr.Bytes())
			t.inner.OnTxStart(rawTx, from)
			return
		}
		signedTx, err := action.RawTxToSignedTx(rawTx, signer, selp.Signature())
		if err != nil {
			log.L().Debug("failed to sign eth tx for debankBlock", zap.Error(err))
			senderAddr := selp.SenderAddress()
			from := common.BytesToAddress(senderAddr.Bytes())
			t.inner.OnTxStart(rawTx, from)
			return
		}
		senderAddr := selp.SenderAddress()
		from := common.BytesToAddress(senderAddr.Bytes())
		t.inner.OnTxStart(signedTx, from)
		// override tx hash with IoTeX native action hash (eth_getBlockByNumber uses this)
		if actHash, err := selp.Hash(); err == nil {
			t.inner.SetTxHash("0x" + hex.EncodeToString(actHash[:]))
		}
	}
}

func (t *iotexRPCTracer) CaptureTxEnd(restGas uint64) {
	if !t.txStarted {
		// Duplicate CaptureTxEnd from executeInEVM defer — skip
		return
	}
	t.flushPendingLogs()
	t.txStarted = false
	t.currentIdx++
	t.inner.CaptureTxEnd(restGas)
}

// flushPendingLogs stamps each buffered log's global logIndex and forwards it
// to inner.InsertLog with the (traceAddress, position) snapshot captured at
// OnLog time. Called inside CaptureTxEnd on the success path.
//
// We use InsertLog (not OnLog) because the callstack inside the inner
// callTracer has been collapsed back to root by now — every sub-frame already
// CaptureExit'd into its parent.Calls. The pre-captured trace_address lets
// inner.InsertLog walk root.Calls down to the originating frame and attach
// the log there, restoring frame-level attribution that OnLog could not have
// preserved because of the deferred flush.
func (t *iotexRPCTracer) flushPendingLogs() {
	for i := range t.pendingLogs {
		p := &t.pendingLogs[i]
		p.log.Index = t.logIndex
		t.logIndex++
		t.inner.InsertLog(p.traceAddress, p.position, p.log)
	}
	t.pendingLogs = t.pendingLogs[:0]
}

// DiscardPendingLogs drops buffered logs for the currently open tx frame
// without flushing to inner.OnLog. Called by TraceStart's cleanup closure
// on the Simulate-skip path so a failed action's partial logs never leak
// into the next action's events. Does NOT touch logIndex — a discarded
// action consumes zero indices.
func (t *iotexRPCTracer) DiscardPendingLogs() {
	t.pendingLogs = t.pendingLogs[:0]
}

func (t *iotexRPCTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	t.captureStarted = true
	// Initialize the parallel stack with the root frame. OnLog snapshots here
	// from now on observe stack/path consistent with the inner callTracer's
	// own callstack[0]. (path stays empty; root's trace_address is the empty
	// slice.)
	t.stack = append(t.stack[:0], frameCtx{})
	t.path = t.path[:0]
	t.inner.CaptureStart(env, from, to, create, input, gas, value)
}

func (t *iotexRPCTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	t.captureStarted = false
	// Intentionally do NOT pop the root frame: EmitTransferLog runs in the
	// cleanup-closure window (CaptureEnd → EmitTransferLogs → CaptureTxEnd)
	// and needs an active root entry to snapshot trace_address=[] / position.
	// The next action's CaptureStart will reset the stack.
	t.inner.CaptureEnd(output, gasUsed, err)
}

func (t *iotexRPCTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	if len(t.stack) > 0 {
		// The new child's index inside parent.Calls equals parent.childCount
		// BEFORE the increment (CaptureExit will append the finalized frame to
		// parent.Calls in this exact order). Mirror that here so OnLog's
		// snapshot path matches inner callTracer's trace_address assignment.
		parent := &t.stack[len(t.stack)-1]
		t.path = append(t.path, parent.childCount)
		parent.childCount++
		t.stack = append(t.stack, frameCtx{})
	}
	t.inner.CaptureEnter(typ, from, to, input, gas, value)
}

func (t *iotexRPCTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	if len(t.stack) > 1 {
		t.stack = t.stack[:len(t.stack)-1]
	}
	if len(t.path) > 0 {
		t.path = t.path[:len(t.path)-1]
	}
	t.inner.CaptureExit(output, gasUsed, err)
}

func (t *iotexRPCTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	t.inner.CaptureState(pc, op, gas, cost, scope, rData, depth, err)
}

func (t *iotexRPCTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
	t.inner.CaptureFault(pc, op, gas, cost, scope, depth, err)
}

// OnLog buffers logs emitted during EVM execution along with the originating
// frame's trace_address path and Position at the OnLog moment. Actual forward
// to inner.InsertLog + logIndex assignment happens in CaptureTxEnd's flush
// (success path) or is dropped by DiscardPendingLogs (Simulate-skip path).
//
// Guard against !captureStarted — IoTeX's MakeTransfer emits logs before
// CaptureStart; those logs have no frame to attach to and are intentionally
// dropped (matches pre-existing behavior).
func (t *iotexRPCTracer) OnLog(l *types.Log) {
	if !t.captureStarted {
		return
	}
	t.pendingLogs = append(t.pendingLogs, t.snapshotForLog(l))
}

// snapshotForLog packages a log with the originating frame's current
// trace_address and Position so flushPendingLogs can later call
// inner.InsertLog with the captured attribution.
func (t *iotexRPCTracer) snapshotForLog(l *types.Log) pendingLog {
	pathCopy := append([]int64(nil), t.path...)
	var position int64
	if n := len(t.stack); n > 0 {
		top := &t.stack[n-1]
		position = top.childCount + top.logCount
		top.logCount++
	}
	return pendingLog{log: l, traceAddress: pathCopy, position: position}
}

// EmitTransferLog stages a log converted from a native TransactionLog
// (GRANT_REWARD, CLAIM_FROM_REWARDING, GAS_FEE, BUCKET_CREATE_AMOUNT, ...) or
// from receipt.Logs() of a non-Execution action into pendingLogs, to be
// flushed in CaptureTxEnd alongside the EVM logs already queued by OnLog.
//
// Ordering relative to canonical-events rebuild matters:
//
// The cleanup closure in evm/tracer.go runs (1) CaptureEnd → (2)
// EmitTransferLogs(receipt, ...) → (3) CaptureTxEnd. Canonical-events
// rebuild in debank_canonical_events.go iterates each receipt as
// r.Logs() (EVM) first, then r.TransferLogs() (synthetic). For the
// (txID, InTxLogIdx) binding map to align with that iteration, the
// inner callTracer must stamp InTxLogIdx onto EVM logs first and
// synthetic logs second.
//
// Buffering both into pendingLogs and flushing in CaptureTxEnd gives
// exactly that: EVM LOG opcodes append during EVM execution (already
// queued before step 2), then EmitTransferLog appends synthetic logs
// in step 2, then flushPendingLogs forwards in append order. Earlier
// versions bypassed pendingLogs because the wrapper layer's inner
// CaptureTxEnd would clear txStarted before the outer flush ran;
// tracerWrapper's txDepth tracking now suppresses that nested
// CaptureTxEnd, so the outer flush in step 3 always runs and the
// buffer path is safe again.
//
// DiscardPendingLogs concern: EmitTransferLog only runs from the
// success branch of the cleanup closure (failure branch calls
// DiscardPendingLogs before adding any synthetic, and never calls
// EmitTransferLogs), so synthetic logs are never staged on a path
// that would also drop them.
func (t *iotexRPCTracer) EmitTransferLog(l *types.Log) {
	if !t.txStarted {
		return
	}
	// EmitTransferLog runs after CaptureEnd in the cleanup closure; by that
	// point the parallel stack still has the root frame (CaptureEnd does NOT
	// pop it) and path is empty, so the synthetic log lands on the root
	// frame's trace_address (`[]`). Position keeps incrementing from where
	// the EVM OnLog calls left off, preserving the OnLog vs synthetic order
	// canonical-events rebuild expects.
	t.pendingLogs = append(t.pendingLogs, t.snapshotForLog(l))
}
