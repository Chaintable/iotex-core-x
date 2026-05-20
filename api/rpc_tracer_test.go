// Copyright (c) 2026 IoTeX Foundation
// Licensed under Apache License 2.0.

package api

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
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
		require.Same(t, evm0, tr.pendingLogs[0], "EVM log #0 must come first")
		require.Same(t, evm1, tr.pendingLogs[1], "EVM log #1 must come second")
		require.Same(t, syn0, tr.pendingLogs[2], "synthetic #0 must come after EVM logs")
		require.Same(t, syn1, tr.pendingLogs[3], "synthetic #1 must come last")
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
		for _, l := range tr.pendingLogs {
			l.Index = tr.logIndex
			tr.logIndex++
		}
		tr.pendingLogs = tr.pendingLogs[:0]

		require.Equal(t, uint(0), l2a.Index, "action 2 first log indexes from 0")
		require.Equal(t, uint(1), l2b.Index)
		require.Equal(t, uint(2), tr.logIndex)
	})
}
