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
type iotexRPCTracer struct {
	inner *ptracer.RPCTracer

	// pre-computed action list for CaptureTxStart → OnTxStart bridging
	actions    []*action.SealedEnvelope
	currentIdx int
	chainID    uint32 // for constructing signed eth tx hash
	txStarted      bool // guards against double CaptureTxStart for Execution actions
	captureStarted bool // true after CaptureStart, safe to call OnLog
	logIndex       uint // global log index counter within block

	// pendingLogs buffers OnLog calls for the currently open tx frame.
	// Flushed to inner.OnLog on normal CaptureTxEnd; dropped on
	// DiscardPendingLogs (Simulate-mode skip path).
	pendingLogs []*types.Log
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

// flushPendingLogs forwards buffered logs to inner.OnLog in order and assigns
// their global logIndex. Called inside CaptureTxEnd on the success path.
func (t *iotexRPCTracer) flushPendingLogs() {
	for _, l := range t.pendingLogs {
		l.Index = t.logIndex
		t.logIndex++
		t.inner.OnLog(l)
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
	t.inner.CaptureStart(env, from, to, create, input, gas, value)
}

func (t *iotexRPCTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
	t.captureStarted = false
	t.inner.CaptureEnd(output, gasUsed, err)
}

func (t *iotexRPCTracer) CaptureEnter(typ vm.OpCode, from common.Address, to common.Address, input []byte, gas uint64, value *big.Int) {
	t.inner.CaptureEnter(typ, from, to, input, gas, value)
}

func (t *iotexRPCTracer) CaptureExit(output []byte, gasUsed uint64, err error) {
	t.inner.CaptureExit(output, gasUsed, err)
}

func (t *iotexRPCTracer) CaptureState(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, rData []byte, depth int, err error) {
	t.inner.CaptureState(pc, op, gas, cost, scope, rData, depth, err)
}

func (t *iotexRPCTracer) CaptureFault(pc uint64, op vm.OpCode, gas, cost uint64, scope *vm.ScopeContext, depth int, err error) {
	t.inner.CaptureFault(pc, op, gas, cost, scope, depth, err)
}

// OnLog buffers logs emitted during EVM execution. Actual forward to
// inner.OnLog + logIndex assignment happens in CaptureTxEnd's flush
// (success path) or is dropped by DiscardPendingLogs (Simulate-skip path).
// Guard against empty callstack — IoTeX's MakeTransfer emits logs before CaptureStart.
func (t *iotexRPCTracer) OnLog(l *types.Log) {
	if !t.captureStarted {
		return
	}
	t.pendingLogs = append(t.pendingLogs, l)
}

// EmitTransferLog bypasses the captureStarted gate to buffer a log converted
// from a native TransactionLog (GRANT_REWARD, CLAIM_FROM_REWARDING, GAS_FEE,
// BUCKET_CREATE_AMOUNT, etc.). Called from the CaptureTx callback so non-EVM
// actions can still expose their pool flows as events. Like OnLog, these are
// buffered and only committed to the inner tracer at CaptureTxEnd flush.
func (t *iotexRPCTracer) EmitTransferLog(l *types.Log) {
	t.pendingLogs = append(t.pendingLogs, l)
}
