// Copyright (c) 2026 IoTeX Foundation
// Licensed under Apache License 2.0.

package api

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/stretchr/testify/require"
)

// TestIotexRPCTracerOnLogBuffering ensures OnLog stages logs in a per-tx
// buffer rather than forwarding immediately, and that Discard vs. flush
// make different observable choices about logIndex and the inner tracer.
//
// The inner ptracer.RPCTracer is closed-source, so this test exercises the
// wrapper's state directly (pendingLogs slice and logIndex counter).
func TestIotexRPCTracerOnLogBuffering(t *testing.T) {
	t.Run("OnLog while captureStarted buffers into pendingLogs", func(t *testing.T) {
		tr := newIotexRPCTracer(1)
		tr.captureStarted = true // simulate post-CaptureStart state
		tr.txStarted = true

		l1 := &types.Log{Address: common.HexToAddress("0x1")}
		l2 := &types.Log{Address: common.HexToAddress("0x2")}
		tr.OnLog(l1)
		tr.OnLog(l2)

		require.Len(t, tr.pendingLogs, 2, "logs should be buffered, not forwarded")
		require.Equal(t, uint(0), tr.logIndex, "logIndex must not advance until flush")
	})

	t.Run("DiscardPendingLogs drops buffer without advancing logIndex", func(t *testing.T) {
		tr := newIotexRPCTracer(1)
		tr.captureStarted = true
		tr.txStarted = true

		tr.OnLog(&types.Log{Address: common.HexToAddress("0xaa")})
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xbb")})
		require.Len(t, tr.pendingLogs, 2)

		tr.DiscardPendingLogs()

		require.Empty(t, tr.pendingLogs, "buffer must be drained")
		require.Equal(t, uint(0), tr.logIndex, "logIndex must not advance on discard")
	})

	t.Run("OnLog with captureStarted=false is rejected", func(t *testing.T) {
		tr := newIotexRPCTracer(1)
		tr.captureStarted = false

		tr.OnLog(&types.Log{Address: common.HexToAddress("0x1")})
		require.Empty(t, tr.pendingLogs, "pre-CaptureStart OnLog must be ignored")
	})

	t.Run("EmitTransferLog stages synthetic into pendingLogs (no immediate flush)", func(t *testing.T) {
		// Synthetic logs from EmitTransferLogs fire after CaptureEnd in the
		// cleanup closure but BEFORE the outer CaptureTxEnd. They must enter
		// the same pendingLogs queue as EVM logs so that
		// (1) canonical-events rebuild's iteration order (r.Logs() then
		//     r.TransferLogs()) matches the order the inner callTracer
		//     receives them, and
		// (2) the (txID, InTxLogIdx) binding is consistent end-to-end.
		// The wrapper's txDepth tracking keeps txStarted=true here so the
		// outer CaptureTxEnd will still flush.
		tr := newIotexRPCTracer(1)
		tr.captureStarted = false // post-CaptureEnd, pre-CaptureTxEnd
		tr.txStarted = true

		l := &types.Log{Address: common.HexToAddress("0xccd3")}
		tr.EmitTransferLog(l)

		require.Len(t, tr.pendingLogs, 1, "synthetic log must be staged into pendingLogs")
		require.Equal(t, uint(0), tr.logIndex, "logIndex advances only at flush time")
	})

	t.Run("EmitTransferLog when txStarted=false is a no-op", func(t *testing.T) {
		// The cleanup closure's failure branch calls DiscardPendingLogs and
		// resets txStarted before any potential EmitTransferLog could fire,
		// but guard defensively in case a future caller violates the contract.
		tr := newIotexRPCTracer(1)
		tr.txStarted = false

		tr.EmitTransferLog(&types.Log{Address: common.HexToAddress("0xdead")})
		require.Empty(t, tr.pendingLogs, "outside an active tx, synthetic logs must be dropped")
	})

	t.Run("EVM logs and synthetic logs land in pendingLogs in canonical order", func(t *testing.T) {
		// Cleanup closure ordering:
		//   1. EVM execution -> tr.OnLog for each LOG opcode -> pendingLogs
		//   2. CaptureEnd
		//   3. EmitTransferLogs -> tr.EmitTransferLog per synthetic -> pendingLogs
		//   4. CaptureTxEnd -> flush
		// canonical-events rebuild iterates r.Logs() (EVM) before
		// r.TransferLogs() (synthetic) when assigning inTxPos, so the inner
		// callTracer must see EVM logs first and synthetic logs second.
		tr := newIotexRPCTracer(1)
		tr.captureStarted = true
		tr.txStarted = true

		evm0 := &types.Log{Address: common.HexToAddress("0xa0")}
		evm1 := &types.Log{Address: common.HexToAddress("0xa1")}
		tr.OnLog(evm0)
		tr.OnLog(evm1)

		tr.captureStarted = false // simulate post-CaptureEnd
		syn0 := &types.Log{Address: common.HexToAddress("0xb0")}
		syn1 := &types.Log{Address: common.HexToAddress("0xb1")}
		tr.EmitTransferLog(syn0)
		tr.EmitTransferLog(syn1)

		require.Len(t, tr.pendingLogs, 4)
		require.Same(t, evm0, tr.pendingLogs[0].log, "EVM log #0 must come first")
		require.Same(t, evm1, tr.pendingLogs[1].log, "EVM log #1 must come second")
		require.Same(t, syn0, tr.pendingLogs[2].log, "synthetic #0 must come after EVM logs")
		require.Same(t, syn1, tr.pendingLogs[3].log, "synthetic #1 must come last")
	})

	t.Run("Discard then next action's logs index from 0", func(t *testing.T) {
		// Simulates: action 1 fails → DiscardPendingLogs → action 2 runs
		// successfully → its logs should receive logIndex starting at 0
		// (not after a gap caused by action 1).
		tr := newIotexRPCTracer(1)

		// Action 1: OnLog + Discard
		tr.captureStarted = true
		tr.txStarted = true
		tr.OnLog(&types.Log{Address: common.HexToAddress("0x1")})
		tr.DiscardPendingLogs()
		require.Equal(t, uint(0), tr.logIndex)

		// Action 2: OnLog + flush (manual, to avoid touching inner tracer)
		tr.captureStarted = true
		tr.txStarted = true
		l2a := &types.Log{Address: common.HexToAddress("0x2a")}
		l2b := &types.Log{Address: common.HexToAddress("0x2b")}
		tr.OnLog(l2a)
		tr.OnLog(l2b)

		// Assign indices as flush would do (but skip inner to avoid
		// depending on closed-source ptracer state).
		for i := range tr.pendingLogs {
			tr.pendingLogs[i].log.Index = tr.logIndex
			tr.logIndex++
		}
		tr.pendingLogs = tr.pendingLogs[:0]

		require.Equal(t, uint(0), l2a.Index, "action 2 first log indexes from 0")
		require.Equal(t, uint(1), l2b.Index)
		require.Equal(t, uint(2), tr.logIndex)
	})
}

// TestIotexRPCTracerStackAndSnapshot covers the parallel stack/path used to
// snapshot a log's trace_address + position at OnLog time so the deferred
// flush can call inner.InsertLog with the right attribution.
//
// The inner ptracer.RPCTracer's callTracer is nil here (no OnTxStart was ever
// called), so its CaptureStart / CaptureEnter / CaptureExit / CaptureEnd are
// safe no-ops — we drive the wrapper directly through its EVMLogger hooks.
func TestIotexRPCTracerStackAndSnapshot(t *testing.T) {
	openTracer := func() *iotexRPCTracer {
		tr := newIotexRPCTracer(1)
		tr.CaptureTxStart(0)
		tr.CaptureStart(nil, common.Address{}, common.Address{}, false, nil, 0, big.NewInt(0))
		return tr
	}
	enterCall := func(tr *iotexRPCTracer) {
		tr.CaptureEnter(vm.CALL, common.Address{}, common.Address{}, nil, 0, big.NewInt(0))
	}

	t.Run("CaptureStart installs root frame with empty path", func(t *testing.T) {
		tr := openTracer()
		require.Len(t, tr.stack, 1)
		require.Equal(t, frameCtx{}, tr.stack[0])
		require.Empty(t, tr.path)
	})

	t.Run("CaptureEnter pushes child, advances parent.childCount, extends path", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		require.Len(t, tr.stack, 2)
		require.Equal(t, int64(1), tr.stack[0].childCount, "root counts the new child")
		require.Equal(t, frameCtx{}, tr.stack[1], "child starts fresh")
		require.Equal(t, []int64{0}, tr.path, "child's trace_address index = 0")
	})

	t.Run("CaptureExit pops stack and path, parent.childCount survives", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)
		require.Len(t, tr.stack, 1)
		require.Empty(t, tr.path)
		require.Equal(t, int64(1), tr.stack[0].childCount, "childCount records the finalized sub-call")
	})

	t.Run("CaptureEnd keeps root so EmitTransferLog can snapshot it", func(t *testing.T) {
		tr := openTracer()
		tr.CaptureEnd(nil, 0, nil)
		require.Len(t, tr.stack, 1, "root frame must survive CaptureEnd")
		require.False(t, tr.captureStarted)
	})

	t.Run("CaptureTxStart resets a stale stack/path as a safety net", func(t *testing.T) {
		tr := newIotexRPCTracer(1)
		tr.stack = []frameCtx{{childCount: 99}, {childCount: 5}}
		tr.path = []int64{42}
		tr.CaptureTxStart(0)
		require.Empty(t, tr.stack)
		require.Empty(t, tr.path)
	})

	t.Run("3 levels of nested CaptureEnter give path [0,0,0]", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		enterCall(tr)
		enterCall(tr)
		require.Len(t, tr.stack, 4)
		require.Equal(t, []int64{0, 0, 0}, tr.path)
	})

	t.Run("a failed sub-call still occupies a trace_address index", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		tr.CaptureExit(nil, 0, fmt.Errorf("revert")) // EVM emits CaptureExit even for reverted call
		enterCall(tr)
		require.Equal(t, []int64{1}, tr.path, "second child indexes after the failed sibling")
	})

	t.Run("OnLog at depth 2 snapshots trace_address [0,0] and position 0", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		enterCall(tr)
		l := &types.Log{Address: common.HexToAddress("0xdead")}
		tr.OnLog(l)
		require.Len(t, tr.pendingLogs, 1)
		require.Same(t, l, tr.pendingLogs[0].log)
		require.Equal(t, []int64{0, 0}, tr.pendingLogs[0].traceAddress)
		require.Equal(t, int64(0), tr.pendingLogs[0].position, "no prior sub-calls or logs in this frame")
	})

	t.Run("position counts finalized sub-calls plus prior logs in the same frame", func(t *testing.T) {
		tr := openTracer()
		// root.calls[0]: one finalized sub-call → root.childCount=1
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa1")}) // position = 1 + 0
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa2")}) // position = 1 + 1
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa3")}) // position = 2 + 2

		require.Len(t, tr.pendingLogs, 3)
		require.Equal(t, int64(1), tr.pendingLogs[0].position)
		require.Equal(t, int64(2), tr.pendingLogs[1].position)
		require.Equal(t, int64(4), tr.pendingLogs[2].position)
		for _, p := range tr.pendingLogs {
			require.Empty(t, p.traceAddress, "all three logs are on root")
		}
	})

	t.Run("snapshot path copies are independent (mutation safety)", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa1")}) // snapshot at path=[0]
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa2")}) // snapshot at path=[0,0]

		require.Equal(t, []int64{0}, tr.pendingLogs[0].traceAddress,
			"first snapshot must be a copy unaffected by later CaptureEnter")
		require.Equal(t, []int64{0, 0}, tr.pendingLogs[1].traceAddress)

		// Mutate the tracer's path further to prove the snapshots are detached.
		tr.path = append(tr.path, 99)
		require.Equal(t, []int64{0}, tr.pendingLogs[0].traceAddress)
		require.Equal(t, []int64{0, 0}, tr.pendingLogs[1].traceAddress)
	})

	t.Run("EmitTransferLog after CaptureEnd snapshots root attribution", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil) // root.childCount = 1
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa1")}) // root, position = 1
		tr.CaptureEnd(nil, 0, nil)
		tr.EmitTransferLog(&types.Log{Address: common.HexToAddress("0xccd3")})

		require.Len(t, tr.pendingLogs, 2)
		require.Empty(t, tr.pendingLogs[1].traceAddress, "synthetic lands on root")
		require.Equal(t, int64(2), tr.pendingLogs[1].position,
			"position = root.childCount(1) + root.logCount-before(1) = 2")
	})

	t.Run("CaptureExit on root panics (invariant: must have sub-frame)", func(t *testing.T) {
		tr := openTracer()
		// only root in stack → CaptureExit is an upstream lifecycle violation
		require.Panics(t, func() {
			tr.CaptureExit(nil, 0, nil)
		}, "CaptureExit with no sub-frame must panic, not silently no-op")
	})

	t.Run("CaptureEnter on empty stack panics (invariant: CaptureStart must precede)", func(t *testing.T) {
		tr := newIotexRPCTracer(1)
		// no CaptureStart fired → stack is empty
		require.Panics(t, func() {
			tr.CaptureEnter(vm.CALL, common.Address{}, common.Address{}, nil, 0, big.NewInt(0))
		}, "CaptureEnter before CaptureStart must panic")
	})

	// IN_CONTRACT_TRANSFER markers (emitted by MakeTransfer for every EVM
	// sub-call value transfer) are forwarded to OnLog by AddLog but NOT
	// appended to receipt.Logs(). Including them in pendingLogs misaligns
	// InTxLogIdx with the receipt-side iteration that canonical-events
	// rebuild uses, causing wrong frame attribution for all subsequent
	// real EVM logs. OnLog must drop these markers.
	t.Run("IN_CONTRACT_TRANSFER markers are dropped from OnLog", func(t *testing.T) {
		tr := openTracer()
		enterCall(tr)
		// MakeTransfer emits a marker on every sub-call with value: topic[0]
		// = zero hash (encoded TransactionLogType_IN_CONTRACT_TRANSFER == 0).
		marker := &types.Log{
			Address: common.Address{},
			Topics: []common.Hash{
				{}, // zero hash → IN_CONTRACT_TRANSFER marker
				common.HexToHash("0x000000000000000000000000aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), // from
				common.HexToHash("0x000000000000000000000000bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), // to
			},
		}
		tr.OnLog(marker)
		require.Empty(t, tr.pendingLogs, "marker must be filtered out of pendingLogs")

		// A real ERC20 Transfer event (3 topics but topic[0] is the Transfer
		// signature, not zero) must still be staged.
		realTransfer := &types.Log{
			Address: common.HexToAddress("0xdead"),
			Topics: []common.Hash{
				common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"),
				common.HexToHash("0x000000000000000000000000aaaa"),
				common.HexToHash("0x000000000000000000000000bbbb"),
			},
		}
		tr.OnLog(realTransfer)
		require.Len(t, tr.pendingLogs, 1, "real ERC20 Transfer must be staged")
		require.Same(t, realTransfer, tr.pendingLogs[0].log)
	})

	// Replays the exact CaptureEnter/CaptureExit/OnLog sequence for real tx
	// 0xad1f3743... (block 48348286, ground truth from debug_traceBlockByNumber
	// callTracer/withLog). 5 logs should snapshot trace_address [1], [2], [3],
	// [3], [3,0,0] respectively.
	t.Run("realistic CaptureEnter/Exit/OnLog sequence matches gt attribution", func(t *testing.T) {
		tr := openTracer()

		// sub[0] STATICCALL (no log)
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)

		// sub[1] CALL with one log
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa00744")})
		tr.CaptureExit(nil, 0, nil)

		// sub[2] CALL with one log
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xa00744")})
		tr.CaptureExit(nil, 0, nil)

		// sub[3] CALL with two logs + nested sub-calls
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0x6cafc26f")})
		tr.OnLog(&types.Log{Address: common.HexToAddress("0x6cafc26f")})

		// sub[3,0] CALL → sub[3,0,0] DELEGATECALL with one log
		enterCall(tr)
		enterCall(tr)
		tr.OnLog(&types.Log{Address: common.HexToAddress("0xbfe6dfa7")})
		tr.CaptureExit(nil, 0, nil)
		tr.CaptureExit(nil, 0, nil)

		// sub[3,1] STATICCALL (no log)
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)

		// sub[3,2] STATICCALL → sub[3,2,0] DELEGATECALL (no log)
		enterCall(tr)
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)
		tr.CaptureExit(nil, 0, nil)

		// sub[3,3] STATICCALL (no log)
		enterCall(tr)
		tr.CaptureExit(nil, 0, nil)

		tr.CaptureExit(nil, 0, nil) // exit sub[3]
		tr.CaptureEnd(nil, 0, nil)

		require.Len(t, tr.pendingLogs, 5)
		require.Equal(t, []int64{1}, tr.pendingLogs[0].traceAddress, "log #1 must be at [1]")
		require.Equal(t, []int64{2}, tr.pendingLogs[1].traceAddress, "log #2 must be at [2]")
		require.Equal(t, []int64{3}, tr.pendingLogs[2].traceAddress, "log #3 must be at [3]")
		require.Equal(t, []int64{3}, tr.pendingLogs[3].traceAddress, "log #4 must be at [3]")
		require.Equal(t, []int64{3, 0, 0}, tr.pendingLogs[4].traceAddress, "log #5 must be at [3,0,0]")
	})
}
