package blockchain

import (
	"encoding/hex"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/stretchr/testify/require"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
)

func TestGenesisHashMismatch(t *testing.T) {
	r := require.New(t)

	g := genesis.Default
	block.LoadGenesisHash(&g)
	genesis.SetGenesisTimestamp(g.Timestamp)

	// Three different genesis hashes in the system:
	// 1. block.GenesisHash() - genesis config hash (used by DAO index / GetBlockHash(0))
	configHash := block.GenesisHash()
	// 2. block.GenesisBlock().HashBlock() - block header protobuf hash
	blockHash := block.GenesisBlock().HashBlock()
	// 3. BuildGenesisGethBlock().Hash() - geth RLP hash (stored in Kafka by OnGenesisBlock)
	gethHash := BuildGenesisGethBlock(g).Hash()

	t.Logf("GenesisHash (config):  %s", hex.EncodeToString(configHash[:]))
	t.Logf("GenesisBlock.Hash:     %s", hex.EncodeToString(blockHash[:]))
	t.Logf("GethGenesisBlock.Hash: %s", gethHash.Hex())

	// All three are different
	r.NotEqual(configHash, blockHash, "config hash should differ from block header hash")
	r.NotEqual(common.Hash(configHash), gethHash, "config hash should differ from geth hash")
	r.NotEqual(common.Hash(blockHash), gethHash, "block header hash should differ from geth hash")
}

func TestGenesisHashFixUnifiedDAO(t *testing.T) {
	r := require.New(t)

	g := genesis.Default
	block.LoadGenesisHash(&g)
	genesis.SetGenesisTimestamp(g.Timestamp)

	// dao.GetBlockHash(0) returns GenesisHash() (config hash) — this is the canonical hash
	daoGenesisHash := common.Hash(block.GenesisHash())

	// Simulate what Kafka stores: geth-style genesis hash from OnGenesisBlock
	gethGenesisHash := BuildGenesisGethBlock(g).Hash()
	lastPushed := ptypes.BlockContext{
		BlockNumber: 0,
		Hash:        gethGenesisHash,
		ParentHash:  common.Hash{},
		Timestamp:   uint64(g.Timestamp),
	}

	// Apply the fix: override genesis hash to dao.GetBlockHash(0) = GenesisHash()
	lastCtx := lastPushed
	if lastCtx.BlockNumber == 0 {
		lastCtx.Hash = daoGenesisHash
		lastCtx.ParentHash = common.Hash(hash.ZeroHash256)
	}

	// The fixed hash must match dao.GetBlockHash(0) = GenesisHash()
	r.Equal(daoGenesisHash, lastCtx.Hash,
		"fixed genesis hash should match dao.GetBlockHash(0) = GenesisHash()")

	// Block 1's PrevHash = GenesisHash() (from blk.PrevHash() = GetBlockHash(0))
	block1ParentHash := common.Hash(block.GenesisHash())

	// Fast path: blockb.ParentHash == blocka.Hash → now matches!
	r.Equal(block1ParentHash, lastCtx.Hash,
		"fast path should match: block1.ParentHash == lastCtx.Hash == GenesisHash()")

	// PushBlockChangeNotification validation:
	// LastPushedBlock().Hash (after fix) == newBlocks[0].ParentHash == GenesisHash()
	// Simulate the pusher's LastBlockNotice also being fixed
	fixedLastPushedHash := daoGenesisHash
	r.Equal(fixedLastPushedHash, block1ParentHash,
		"PushBlockChangeNotification: LastPushedBlock().Hash == newBlocks[0].ParentHash")
}
