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
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"go.uber.org/zap"
)

// pendingLog buffers the OnLog / EmitTransferLog snapshot taken during EVM
// execution: the log itself plus the originating frame's trace_address path
// and Position value at the OnLog moment. flushPendingLogs uses these
// snapshots to call inner.InsertLog so the log is physically attached to the
// originating sub-frame; without the snapshot, all logs would land on the root
// frame (because every sub-frame has been OnExit'd by flush time). [P9-1]
type pendingLog struct {
	log          *types.Log
	traceAddress []int64
	position     int64
}

// frameCtx tracks how many sub-calls have already finalized into the frame's
// parent.Calls and how many logs OnLog has staged for the frame, both since
// OnEnter opened it. Used to compute Position (childCount + logCount) at OnLog
// time and to derive the next child's trace_address index at OnEnter time. [P9-1]
type frameCtx struct {
	childCount int64
	logCount   int64
}

// iotexRPCTracer wraps pipeline's RPCTracer to bridge IoTeX's action-based
// execution model to v1.15.11 hook signatures. Originally implemented as
// vm.EVMLogger (Capture* hooks); after the v2.4.1 / v1.15.11 merge it exposes
// its method set as a *tracing.Hooks via Hooks().
//
// OnLog buffering: logs emitted between OnTxStart and OnTxEnd are staged in
// pendingLogs instead of forwarded to inner.OnLog immediately. This lets the
// Simulate-mode skip path call DiscardPendingLogs to drop logs from a failed
// action before OnTxEnd runs, preventing them from leaking into the next
// action's log stream. Normal (success) paths flush pendingLogs inside
// OnTxEnd. logIndex advances only at flush time so a discarded action leaves
// no gap in the global index sequence.
//
// Frame-level attribution: the inner callTracer's callstack at flush time is
// collapsed back to root (every sub-frame already OnExit'd into its
// parent.Calls). So OnLog records the originating frame's trace_address path
// and Position at the moment the log fires, by maintaining a parallel stack
// (stack/path below) driven by the same OnEnter/OnExit signals the callTracer
// sees. flushPendingLogs then calls inner.InsertLog with the snapshotted path
// so each log is physically attached to the originating sub-frame; otherwise
// OnTxEnd's addTraceAndLog would walk every log under root.TraceID and yield
// event.parent_trace_id = root for everything.
type iotexRPCTracer struct {
	inner *ptracer.RPCTracer

	// pre-computed action list for OnTxStart -> inner.OnTxStart bridging
	actions    []*action.SealedEnvelope
	currentIdx int
	chainID    uint32 // for constructing signed eth tx hash

	txStarted      bool // guards against double OnTxStart for Execution actions
	captureStarted bool // true after OnEnter(depth=0), safe to call OnLog
	logIndex       uint // global log index counter within block

	// pendingLogs buffers OnLog / EmitTransferLog calls for the currently open
	// tx frame, paired with the (traceAddress, position) snapshot taken at the
	// emit moment. Flushed via inner.InsertLog on normal OnTxEnd; dropped by
	// DiscardPendingLogs on the Simulate-skip path. [P9-1]
	pendingLogs []pendingLog

	// stack and path mirror the inner callTracer's call stack one entry per
	// active frame (stack[0] = root). path holds the trace_address of the frame
	// currently on top, i.e. len(path) == len(stack) - 1 except during transient
	// hook transitions. OnEnter / OnExit keep them in sync with the EVM's actual
	// call depth; OnLog snapshots them. [P9-1]
	stack []frameCtx
	path  []int64

	// onLogCount tallies real (non-marker) OnLog snapshots per tx. Used by
	// emitTransferLogsAsEvents to slice receipt.Logs() into a handler-native
	// head (= actLogs the handler wrote directly to receipt) and an inner-EVM
	// tail (= logs already buffered via OnLog). Without this slice, the
	// cleanup closure would re-buffer the inner-EVM tail and offset the (txID,
	// InTxLogIdx) binding used by canonical-events rebuild. Reset on every
	// OnTxStart. [P9-5]
	onLogCount int
}

func newIotexRPCTracer(chainID uint32) *iotexRPCTracer {
	return &iotexRPCTracer{
		inner:   &ptracer.RPCTracer{},
		chainID: chainID,
	}
}

// SetActions pre-registers the action list. OnTxStart will use currentIdx to
// look up the corresponding ethTx and call inner.OnTxStart.
func (t *iotexRPCTracer) SetActions(actions []*action.SealedEnvelope) {
	t.actions = actions
	t.currentIdx = 0
	t.logIndex = 0
}

// Hooks exposes the iotexRPCTracer as a *tracing.Hooks for use as
// vm.Config.Tracer in v1.15.11. The seven hooks below are the ones pipeline's
// RPCTracer implements (OnBlockStart / OnTxStart / OnTxEnd / OnEnter / OnExit /
// OnLog / OnOpcode); trace_debankBlock's replay path drives block-level
// boundaries by calling SetActions + OnBlockStart explicitly via coreservice,
// so OnBlockEnd / OnGenesisBlock / OnClose / OnBlockchainInit are intentionally
// omitted (they are not produced by the replay-time EVM path). OnBalanceChange
// / OnSystemCallStart are intentionally omitted under the "Live Tracer +
// StateDB-based [Priority 1]" mode — pipeline ingests the whole state diff via
// OnCommit, not per-write hooks.
func (t *iotexRPCTracer) Hooks() *tracing.Hooks {
	return &tracing.Hooks{
		OnBlockStart: t.OnBlockStart,
		OnTxStart:    t.OnTxStart,
		OnTxEnd:      t.OnTxEnd,
		OnEnter:      t.OnEnter,
		OnExit:       t.OnExit,
		OnLog:        t.OnLog,
		OnOpcode:     t.OnOpcode,
	}
}

// OnBlockStart forwards to pipeline RPCTracer (which still uses *types.Block,
// not the v1.15.11 BlockEvent wrapper). Pipeline's signature lags v1.15.11 here
// by design — the wrapper unpacks BlockEvent.Block before forwarding.
func (t *iotexRPCTracer) OnBlockStart(event tracing.BlockEvent) {
	if event.Block == nil {
		return
	}
	t.inner.OnBlockStart(event.Block)
}

// OnTxStart bridges IoTeX's action[currentIdx] to pipeline's OnTxStart(tx,from).
// Pipeline RPCTracer.OnTxStart ignores the env/tx args internally (its
// callTracer fills in tx fields from SetTxHash); we still pass real values so
// downstream consumers that inspect tx see correct data.
//
// Safety net: reset the parallel stack/path/onLogCount in case a prior tx left
// residue (e.g. EVM faulted between OnEnter and OnExit in some pathological
// scenario). OnEnter(depth=0) fills root in immediately. [P9-1, P9-5]
func (t *iotexRPCTracer) OnTxStart(env *tracing.VMContext, tx *types.Transaction, from common.Address) {
	if t.txStarted {
		// Already initialized — skip duplicate.
		return
	}
	t.txStarted = true
	t.stack = t.stack[:0]
	t.path = t.path[:0]
	t.onLogCount = 0
	// Bridge: look up pre-computed selp by index, call inner.OnTxStart.
	if t.currentIdx >= len(t.actions) {
		return
	}
	selp := t.actions[t.currentIdx]
	rawTx, err := selp.ToEthTx()
	if err != nil {
		// non-EVM action — skip OnTxStart but keep txStarted=true.
		return
	}
	// construct signed tx so tx hash matches eth_getBlockByNumber
	signer, sErr := action.NewEthSigner(iotextypes.Encoding(selp.Encoding()), t.chainID)
	if sErr != nil {
		log.L().Debug("failed to create eth signer for debankBlock", zap.Error(sErr))
		senderAddr := selp.SenderAddress()
		fromBytes := common.BytesToAddress(senderAddr.Bytes())
		t.inner.OnTxStart(env, rawTx, fromBytes)
	} else if signedTx, signErr := action.RawTxToSignedTx(rawTx, signer, selp.Signature()); signErr != nil {
		log.L().Debug("failed to sign eth tx for debankBlock", zap.Error(signErr))
		senderAddr := selp.SenderAddress()
		fromBytes := common.BytesToAddress(senderAddr.Bytes())
		t.inner.OnTxStart(env, rawTx, fromBytes)
	} else {
		senderAddr := selp.SenderAddress()
		fromBytes := common.BytesToAddress(senderAddr.Bytes())
		t.inner.OnTxStart(env, signedTx, fromBytes)
	}
	// override tx hash with IoTeX native action hash (eth_getBlockByNumber uses this)
	if actHash, err := selp.Hash(); err == nil {
		t.inner.SetTxHash("0x" + hex.EncodeToString(actHash[:]))
	}
}

// OnTxEnd flushes pendingLogs (success path) and advances currentIdx.
// PR #9 frame-level attribution lives in flushPendingLogs's use of
// inner.InsertLog with (traceAddress, position) snapshots.
func (t *iotexRPCTracer) OnTxEnd(receipt *types.Receipt, err error) {
	if !t.txStarted {
		return
	}
	t.flushPendingLogs()
	t.txStarted = false
	t.captureStarted = false
	t.currentIdx++
	t.inner.OnTxEnd(receipt, err)
}

// flushPendingLogs stamps each buffered log's global logIndex and forwards it
// to inner.InsertLog with the (traceAddress, position) snapshot captured at
// OnLog time. Called inside OnTxEnd on the success path. [P9-1]
//
// We use InsertLog (not OnLog) because the callstack inside the inner
// callTracer has been collapsed back to root by now — every sub-frame already
// OnExit'd into its parent.Calls. The pre-captured trace_address lets
// inner.InsertLog walk root.Calls down to the originating frame and attach
// the log there, restoring frame-level attribution.
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
// without flushing to inner.OnLog. Called by TraceStart's cleanup closure on
// the Simulate-skip path so a failed action's partial logs never leak into
// the next action's events. Does NOT touch logIndex — a discarded action
// consumes zero indices.
func (t *iotexRPCTracer) DiscardPendingLogs() {
	t.pendingLogs = t.pendingLogs[:0]
}

// GetOutPut forwards to inner RPCTracer; signature mirrors pipeline's exactly
// (common.Hash keys + []byte values).
func (t *iotexRPCTracer) GetOutPut(
	originRoot, root common.Hash,
	destructs map[common.Hash]struct{},
	accounts map[common.Hash][]byte,
	storages map[common.Hash]map[common.Hash][]byte,
	codes map[common.Hash][]byte,
) *ptypes.DebankOutPut {
	return t.inner.GetOutPut(originRoot, root, destructs, accounts, storages, codes)
}

// AdvanceIdx advances currentIdx for actions whose tracing path is wholly
// skipped (non-EVM action handler that calls neither OnTxStart nor any
// frame hook). [PR #3 6b716253c]
func (t *iotexRPCTracer) AdvanceIdx() {
	t.currentIdx++
}

// OnEnter handles both the root frame (depth=0) and sub-frames (depth>0).
// v1.15.11 EVM passes depth as a parameter; we use it to distinguish.
//
// Root frame (depth=0): captureStarted=true; reset stack to [root], path to
// empty. The next action's OnEnter(0) will reset again.
//
// Sub-frame (depth>0): push a new frame, advance parent.childCount, and append
// the new child's index to path. Invariant: root frame must have been opened
// by OnEnter(0) before any sub-frame fires. Empty stack here means upstream
// lifecycle is out of sync — panic so the violation is loud (matches inner
// callTracer's behavior). [P9-1, P9-4]
func (t *iotexRPCTracer) OnEnter(depth int, typ byte, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	if depth == 0 {
		t.captureStarted = true
		t.stack = append(t.stack[:0], frameCtx{})
		t.path = t.path[:0]
		t.inner.OnEnter(depth, typ, from, to, input, gas, value)
		return
	}
	if len(t.stack) == 0 {
		log.L().Panic("[iotexRPCTracer] OnEnter sub-frame with empty stack — OnEnter(0) did not fire",
			zap.Int("depth", depth),
			zap.String("from", from.Hex()), zap.String("to", to.Hex()))
	}
	parent := &t.stack[len(t.stack)-1]
	t.path = append(t.path, parent.childCount)
	parent.childCount++
	t.stack = append(t.stack, frameCtx{})
	t.inner.OnEnter(depth, typ, from, to, input, gas, value)
}

// OnExit handles both the root frame (depth=0) and sub-frames (depth>0).
//
// Root frame (depth=0): clear captureStarted. Intentionally do NOT pop the
// root frame — EmitTransferLog runs in the cleanup-closure window
// (OnExit(0) -> EmitTransferLogs -> OnTxEnd) and needs an active root entry
// to snapshot trace_address=[] / position. The next action's OnEnter(0) will
// reset the stack.
//
// Sub-frame (depth>0): pop the frame, validate len(path) == len(stack)-1, and
// inform inner callTracer of the new top frame's parent.logCount (the buffered
// OnLog count not yet inserted via flushPendingLogs). [P9-3, P9-4]
func (t *iotexRPCTracer) OnExit(depth int, output []byte, gasUsed uint64, err error, reverted bool) {
	if depth == 0 {
		t.captureStarted = false
		t.inner.OnExit(depth, output, gasUsed, err, reverted)
		return
	}
	if len(t.stack) <= 1 {
		log.L().Panic("[iotexRPCTracer] OnExit with no sub-frame — stack underflow",
			zap.Int("stackLen", len(t.stack)))
	}
	if len(t.path) != len(t.stack)-1 {
		log.L().Panic("[iotexRPCTracer] OnExit invariant: len(path) != len(stack)-1",
			zap.Int("stackLen", len(t.stack)), zap.Int("pathLen", len(t.path)))
	}
	t.stack = t.stack[:len(t.stack)-1]
	t.path = t.path[:len(t.path)-1]
	// Inform inner callTracer how many real EVM logs have been emitted on the
	// popping frame's parent so far but are still sitting in pendingLogs (not
	// yet inserted into parent.Logs). Without this, callTracer.OnExit would
	// compute PosInParentTrace using only len(parent.Calls) + len(parent.Logs)
	// == 0, colliding with the pos values snapshotForLog already assigned.
	parentLogCount := t.stack[len(t.stack)-1].logCount
	t.inner.SetPendingLogsOnTopParent(int(parentLogCount))
	t.inner.OnExit(depth, output, gasUsed, err, reverted)
}

// OnLog buffers logs emitted during EVM execution along with the originating
// frame's trace_address path and Position at the OnLog moment. Forwarding to
// inner.InsertLog + logIndex assignment happens in flushPendingLogs on the
// success path or is dropped by DiscardPendingLogs on Simulate-skip.
//
// Guards:
//   - !captureStarted: MakeTransfer can emit logs before OnEnter(0); those
//     logs have no frame to attach to and are intentionally dropped.
//   - IN_CONTRACT_TRANSFER markers: AddLog forwards every log (including the
//     marker) to OnLog, but the marker is NOT added to stateDB.logs and never
//     surfaces in receipt.Logs(). Including it in pendingLogs would offset
//     InTxLogIdx away from receipt-side iteration order, breaking
//     canonical-events rebuild's (txID, inTxIdx) binding. [P9-2]
func (t *iotexRPCTracer) OnLog(l *types.Log) {
	if !t.captureStarted {
		return
	}
	if isInContractTransferMarker(l) {
		return
	}
	t.onLogCount++
	t.pendingLogs = append(t.pendingLogs, t.snapshotForLog(l))
}

// snapshotForLog packages a log with the originating frame's current
// trace_address and Position so flushPendingLogs can later call
// inner.InsertLog with the captured attribution. [P9-1]
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

// isInContractTransferMarker returns true if l is an IN_CONTRACT_TRANSFER
// marker emitted by iotex's MakeTransfer (action/protocol/execution/evm/
// evm.go:67-79) — fired automatically for every EVM sub-call value transfer.
// Recognised by topic[0] == zero hash, the encoded TransactionLogType_IN_CONTRACT_TRANSFER
// value (= 0) wrapped through hash.BytesToHash256({0}). [P9-2]
func isInContractTransferMarker(l *types.Log) bool {
	return len(l.Topics) == 3 && l.Topics[0] == (common.Hash{})
}

// OnOpcode forwards to inner.
func (t *iotexRPCTracer) OnOpcode(pc uint64, op byte, gas, cost uint64, scope tracing.OpContext, rData []byte, depth int, err error) {
	t.inner.OnOpcode(pc, op, gas, cost, scope, rData, depth, err)
}

// EmitTransferLog stages a log converted from a native TransactionLog
// (GRANT_REWARD, CLAIM_FROM_REWARDING, GAS_FEE, BUCKET_CREATE_AMOUNT, ...) or
// from receipt.Logs() of a non-Execution action into pendingLogs, to be flushed
// in OnTxEnd alongside the EVM logs already queued by OnLog.
//
// EmitTransferLog runs after OnExit(0) in the cleanup closure; by that point
// the parallel stack still has the root frame (OnExit(0) does NOT pop it) and
// path is empty, so the synthetic log lands on the root frame's trace_address
// (`[]`). Position keeps incrementing from where the EVM OnLog calls left off,
// preserving the OnLog vs synthetic order canonical-events rebuild expects.
// [P9-1]
func (t *iotexRPCTracer) EmitTransferLog(l *types.Log) {
	if !t.txStarted {
		return
	}
	t.pendingLogs = append(t.pendingLogs, t.snapshotForLog(l))
}

// EmitTransferLogsAtHead snapshots the given handler-native actLogs (the head
// of receipt.Logs() that the action handler wrote directly, NOT via
// stateDB.AddLog -> OnLog) and prepends them to pendingLogs as a contiguous
// block, preserving the input order. [P9-5, commit b6e841f28]
//
// Why prepend (not append): canonical_events rebuild iterates receipt.Logs()
// in receipt order, where actLogs always appear at the HEAD before any inner
// EVM logs. To keep (txID, InTxLogIdx) <-> (txID, inTxPos) aligned,
// flushPendingLogs order must mirror that: actLogs first, then inner OnLog
// snapshots, then synthetic TransferLog (TransactionLogs) appended at tail.
//
// Bug this fixes: non-Execution EthCompatibleAction handlers that internally
// invoke evm.ExecuteContract (notably MigrateStake -> createNFTBucket) emit
// both handler-native actLogs AND inner EVM logs that reach receipt.Logs()
// together. The pre-fix emitTransferLogsAsEvents unconditionally re-emitted
// ALL receipt.Logs() at cleanup; the inner-EVM tail was therefore double-buffered,
// inflating pendingLogs length beyond what receipt iteration expects. Slicing
// the head-only and prepending here keeps alignment.
func (t *iotexRPCTracer) EmitTransferLogsAtHead(logs []*types.Log) {
	if !t.txStarted || len(logs) == 0 {
		return
	}
	head := make([]pendingLog, len(logs))
	for i, l := range logs {
		head[i] = t.snapshotForLog(l)
	}
	t.pendingLogs = append(head, t.pendingLogs...)
}
