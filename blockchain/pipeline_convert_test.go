// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
	"github.com/iotexproject/iotex-core/v2/test/identityset"
)

func TestConvertToGethBlock_GasLimitByHeight(t *testing.T) {
	g := genesis.Default
	g.BlockGasLimit = 100
	g.TsunamiBlockGasLimit = 200
	g.WakeBlockGasLimit = 300
	g.TsunamiBlockHeight = 2
	g.WakeBlockHeight = 4

	tests := []struct {
		height   uint64
		expected uint64
	}{
		{height: 1, expected: 100},
		{height: 2, expected: 200},
		{height: 3, expected: 200},
		{height: 4, expected: 300},
		{height: 5, expected: 300},
	}

	for _, tc := range tests {
		blk := testBlock(t, tc.height)
		gethBlock := ConvertToGethBlock(blk, g)
		require.Equal(t, tc.expected, gethBlock.GasLimit(), "height=%d", tc.height)
	}
}

func TestBuildGenesisGethBlock_GasLimitByHeight(t *testing.T) {
	g := genesis.Default
	g.BlockGasLimit = 123
	g.TsunamiBlockGasLimit = 456
	g.WakeBlockGasLimit = 789
	g.TsunamiBlockHeight = 2
	g.WakeBlockHeight = 4

	gethBlock := BuildGenesisGethBlock(g)
	require.Equal(t, uint64(123), gethBlock.GasLimit())
}

func testBlock(t *testing.T, height uint64) *block.Block {
	t.Helper()

	blk, err := block.NewTestingBuilder().
		SetHeight(height).
		SetTimeStamp(time.Unix(1700000000, 0)).
		SignAndBuild(identityset.PrivateKey(0))
	require.NoError(t, err)
	return &blk
}
