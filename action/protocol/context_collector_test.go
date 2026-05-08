// Copyright (c) 2026 IoTeX Foundation
// Licensed under Apache License 2.0.

package protocol

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

// TestPipelineStateDiffCollectorSnapshotRevert exercises the Snapshot /
// Revert / DiscardSnapshot API directly. These are the low-level primitives
// that runAction relies on to give Simulate-mode skipped actions a clean
// per-action transaction boundary in the collector; if this test fails,
// the Commit-1 fix's correctness claim is wrong at the source.
func TestPipelineStateDiffCollectorSnapshotRevert(t *testing.T) {
	kA := common.HexToHash("0xaa")
	kB := common.HexToHash("0xbb")
	kC := common.HexToHash("0xcc")
	kD := common.HexToHash("0xdd")
	kE := common.HexToHash("0xee")
	kF := common.HexToHash("0xff")

	t.Run("revert drops action's writes", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()

		// Action A (successful) writes.
		c.Accounts[kA] = []byte("A-value")

		// Snapshot before action B.
		snap := c.Snapshot()

		// Action B writes across all four maps.
		c.Accounts[kB] = []byte("B-value")
		c.Destructs[kC] = struct{}{}
		c.Storages[kD] = map[common.Hash][]byte{kE: []byte("E-slot")}
		c.Codes[kF] = []byte("F-code")

		// Action B fails → revert.
		c.Revert(snap)

		// Only action A's writes remain.
		require.Equal(t, map[common.Hash][]byte{kA: []byte("A-value")}, c.Accounts)
		require.Empty(t, c.Destructs)
		require.Empty(t, c.Storages)
		require.Empty(t, c.Codes)
	})

	t.Run("discard keeps action's writes", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()

		c.Accounts[kA] = []byte("A-value")
		snap := c.Snapshot()

		c.Accounts[kB] = []byte("B-value")

		// Action B succeeds → discard snapshot, keep the writes.
		c.DiscardSnapshot(snap)

		require.Equal(t, []byte("A-value"), c.Accounts[kA])
		require.Equal(t, []byte("B-value"), c.Accounts[kB])
		require.Len(t, c.Accounts, 2)
	})

	t.Run("revert restores modifications, not just additions", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()

		// Initial state: kA=old-value.
		c.Accounts[kA] = []byte("old-value")

		snap := c.Snapshot()

		// Action mutates the same key.
		c.Accounts[kA] = []byte("new-value")

		// Revert restores the pre-snapshot value.
		c.Revert(snap)
		require.Equal(t, []byte("old-value"), c.Accounts[kA])
	})

	t.Run("nested snapshots revert through the stack", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()

		c.Accounts[kA] = []byte("A0")

		snap1 := c.Snapshot()
		c.Accounts[kB] = []byte("B1")

		snap2 := c.Snapshot()
		c.Accounts[kC] = []byte("C2")

		// Revert to snap1 — drops everything since snap1 (both B and C).
		c.Revert(snap1)
		require.Equal(t, []byte("A0"), c.Accounts[kA])
		_, hasB := c.Accounts[kB]
		_, hasC := c.Accounts[kC]
		require.False(t, hasB)
		require.False(t, hasC)

		// snap2 has been popped too — second Revert is a safe no-op.
		c.Revert(snap2)
		require.Equal(t, []byte("A0"), c.Accounts[kA])
	})

	t.Run("storage inner maps are deep-copied", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()
		c.Storages[kA] = map[common.Hash][]byte{kB: []byte("before")}

		snap := c.Snapshot()

		// Mutate the inner map, not just the outer.
		c.Storages[kA][kB] = []byte("after")
		c.Storages[kA][kC] = []byte("added")

		c.Revert(snap)

		require.Equal(t, []byte("before"), c.Storages[kA][kB])
		_, hasC := c.Storages[kA][kC]
		require.False(t, hasC, "added slot should have been reverted")
	})

	t.Run("nil collector methods are safe no-ops", func(t *testing.T) {
		var c *PipelineStateDiffCollector
		require.Equal(t, -1, c.Snapshot())
		// Revert / DiscardSnapshot on nil or with out-of-range id must not panic.
		require.NotPanics(t, func() { c.Revert(0) })
		require.NotPanics(t, func() { c.DiscardSnapshot(0) })
		c2 := NewPipelineStateDiffCollector()
		require.NotPanics(t, func() { c2.Revert(-1) })
		require.NotPanics(t, func() { c2.Revert(100) })
	})

	t.Run("discard pops snapshots above id for stack symmetry", func(t *testing.T) {
		c := NewPipelineStateDiffCollector()

		snap1 := c.Snapshot()
		c.Accounts[kA] = []byte("A")

		snap2 := c.Snapshot()
		c.Accounts[kB] = []byte("B")

		// Discard snap1 — conceptually "action 1 succeeded, don't need its
		// snapshot anymore". snap2 (nested) is popped too since the parent's
		// success implies the child context no longer exists.
		c.DiscardSnapshot(snap1)
		require.Equal(t, []byte("A"), c.Accounts[kA])
		require.Equal(t, []byte("B"), c.Accounts[kB])

		// Subsequent Revert on either id is a safe no-op.
		require.NotPanics(t, func() { c.Revert(snap2) })
	})
}

// TestPipelineStateDiffCollectorGhostAccountRegression captures the concrete
// scenario described in the audit: action A writes to addr X, then fails;
// no later action touches X. Without the fix, addr X would remain in
// Accounts as a "ghost"; with the fix, Revert clears it.
func TestPipelineStateDiffCollectorGhostAccountRegression(t *testing.T) {
	c := NewPipelineStateDiffCollector()

	ghostAddr := common.HexToHash("0xbadbad")
	realAddr := common.HexToHash("0x600d")

	// Prior successful action wrote realAddr's state.
	c.Accounts[realAddr] = []byte("real-account")

	// Failing action starts — snapshot.
	snap := c.Snapshot()

	// Failing action writes ghost state before it fails.
	c.Accounts[ghostAddr] = []byte("ghost-account")

	// Simulate-mode skip: runAction returns error; defer fires Revert.
	c.Revert(snap)

	// No later action touches ghostAddr — it must not appear in state_diff output.
	_, present := c.Accounts[ghostAddr]
	require.False(t, present, "ghost address must not survive revert")

	// Real action's state is preserved.
	require.Equal(t, []byte("real-account"), c.Accounts[realAddr])
}
