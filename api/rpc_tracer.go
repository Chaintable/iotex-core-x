package api

import (
	"math/big"

	ptracer "github.com/Chaintable/pipeline/tracer"
	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/iotexproject/iotex-core/v2/action"
)

var _ vm.EVMLogger = (*iotexRPCTracer)(nil)

// iotexRPCTracer wraps pipeline's RPCTracer to bridge IoTeX's action-based execution
// model (CaptureTxStart without tx info) to RPCTracer's OnTxStart(tx, from).
//
// IoTeX calls CaptureTxStart twice for *action.Execution:
// once in TraceStart and once in executeInEVM. txStarted prevents double init.
type iotexRPCTracer struct {
	inner *ptracer.RPCTracer

	// pre-computed action list for CaptureTxStart → OnTxStart bridging
	actions    []*action.SealedEnvelope
	currentIdx int
	txStarted  bool // guards against double CaptureTxStart for Execution actions
}

func newIotexRPCTracer() *iotexRPCTracer {
	return &iotexRPCTracer{
		inner: &ptracer.RPCTracer{},
	}
}

// SetActions pre-registers the action list. CaptureTxStart will use currentIdx
// to look up the corresponding ethTx and call inner.OnTxStart.
func (t *iotexRPCTracer) SetActions(actions []*action.SealedEnvelope) {
	t.actions = actions
	t.currentIdx = 0
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
		ethTx, err := selp.ToEthTx()
		if err != nil {
			// non-EVM action — skip OnTxStart but keep txStarted=true
			return
		}
		senderAddr := selp.SenderAddress()
		from := common.BytesToAddress(senderAddr.Bytes())
		t.inner.OnTxStart(ethTx, from)
	}
}

func (t *iotexRPCTracer) CaptureTxEnd(restGas uint64) {
	if !t.txStarted {
		// Duplicate CaptureTxEnd from executeInEVM defer — skip
		return
	}
	t.txStarted = false
	t.currentIdx++
	t.inner.CaptureTxEnd(restGas)
}

func (t *iotexRPCTracer) CaptureStart(env *vm.EVM, from common.Address, to common.Address, create bool, input []byte, gas uint64, value *big.Int) {
	t.inner.CaptureStart(env, from, to, create, input, gas, value)
}

func (t *iotexRPCTracer) CaptureEnd(output []byte, gasUsed uint64, err error) {
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

// OnLog delegates to inner RPCTracer for event collection.
func (t *iotexRPCTracer) OnLog(l *types.Log) {
	t.inner.OnLog(l)
}
