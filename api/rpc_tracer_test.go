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

	t.Run("EmitTransferLog forwards directly to inner.OnLog and advances logIndex immediately", func(t *testing.T) {
		// Synthetic logs from EmitTransferLogs fire after CaptureEnd in the
		// cleanup closure (captureStarted=false at that point). Buffering them
		// in pendingLogs would risk losing them to the txStarted=false short-
		// circuit in the outer CaptureTxEnd (set by evm.go's inner defer).
		// Direct emit fixes that.
		tr := newIotexRPCTracer(1)
		tr.captureStarted = false
		tr.txStarted = true

		l := &types.Log{Address: common.HexToAddress("0xccd3")}
		tr.EmitTransferLog(l)

		require.Empty(t, tr.pendingLogs, "synthetic logs go straight to inner.OnLog, not pendingLogs")
		require.Equal(t, uint(1), tr.logIndex, "EmitTransferLog must advance logIndex")
		require.EqualValues(t, 0, l.Index, "the emitted log carries the assigned block-global Index")
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
