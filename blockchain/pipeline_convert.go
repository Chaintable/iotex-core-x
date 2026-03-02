// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

// LastGethBlockHash caches the geth RLP hash of the most recently converted block.
// This enables a consistent geth hash chain where each block's ParentHash matches
// the parent block's geth hash (used as S3 key). Without this, IoTeX native PrevHash
// (GenesisHash for block 0, protobuf hash for others) would be used, causing
// mismatches with S3-stored block keys.
// Initialized from Kafka (LastPushedBlock) on restart, or computed on fresh start.
var LastGethBlockHash common.Hash

// ConvertToGethBlock converts an iotex block.Block to a geth types.Block
func ConvertToGethBlock(blk *block.Block, g genesis.Genesis) *types.Block {
	// Determine parent geth hash for consistent hash chain in S3/Kafka pipeline.
	// IoTeX native PrevHash differs from geth RLP hash for all blocks.
	var parentHash common.Hash
	nativePrevHash := blk.PrevHash()
	if LastGethBlockHash != (common.Hash{}) {
		parentHash = LastGethBlockHash
		log.L().Info("ConvertToGethBlock: using cached LastGethBlockHash",
			zap.Uint64("height", blk.Height()),
			zap.String("parentHash", parentHash.Hex()),
			zap.String("nativePrevHash", hash.Hash256(nativePrevHash).Hex()))
	} else if blk.Height() == 1 {
		// Fresh start: genesis geth hash not cached yet, compute it
		parentHash = BuildGenesisGethBlock(g).Hash()
		log.L().Info("ConvertToGethBlock: fresh start block 1, computed genesis geth hash",
			zap.String("parentHash", parentHash.Hex()),
			zap.String("nativePrevHash", hash.Hash256(nativePrevHash).Hex()),
			zap.Int64("genesisTimestamp", g.Timestamp))
	} else {
		// Fallback: use IoTeX native hash (should not happen in normal operation)
		parentHash = common.BytesToHash(nativePrevHash[:])
		log.L().Warn("ConvertToGethBlock: FALLBACK to native PrevHash",
			zap.Uint64("height", blk.Height()),
			zap.String("parentHash", parentHash.Hex()))
	}
	stateDigest := blk.DeltaStateDigest()
	txRoot := blk.TxRoot()
	receiptRoot := blk.ReceiptRoot()
	header := &types.Header{
		Number:      new(big.Int).SetUint64(blk.Height()),
		Time:        uint64(blk.Timestamp().Unix()),
		GasUsed:     blk.GasUsed(),
		GasLimit:    g.BlockGasLimitByHeight(blk.Height()),
		ParentHash:  parentHash,
		Root:        common.BytesToHash(stateDigest[:]),
		TxHash:      common.BytesToHash(txRoot[:]),
		ReceiptHash: common.BytesToHash(receiptRoot[:]),
		Difficulty:  common.Big0,
	}
	if baseFee := blk.BaseFee(); baseFee != nil {
		header.BaseFee = new(big.Int).Set(baseFee)
	}
	blobGasUsed := blk.BlobGasUsed()
	header.BlobGasUsed = &blobGasUsed
	excessBlobGas := blk.ExcessBlobGas()
	header.ExcessBlobGas = &excessBlobGas
	if bloom := blk.LogsBloomfilter(); bloom != nil {
		copy(header.Bloom[:], bloom.Bytes())
	}
	if addr, err := address.FromString(blk.ProducerAddress()); err == nil {
		header.Coinbase = common.BytesToAddress(addr.Bytes())
	}

	var txs []*types.Transaction
	for _, selp := range blk.Actions {
		ethTx, err := selp.ToEthTx()
		if err != nil {
			// skip system actions that cannot be converted
			continue
		}
		txs = append(txs, ethTx)
	}
	gethBlock := types.NewBlockWithHeader(header).WithBody(txs, nil)
	LastGethBlockHash = gethBlock.Hash()
	return gethBlock
}

// ConvertToGethReceipt converts an iotex action.Receipt to a geth types.Receipt
func ConvertToGethReceipt(receipt *action.Receipt) *types.Receipt {
	if receipt == nil {
		return nil
	}
	r := &types.Receipt{
		Status:            receipt.Status,
		GasUsed:           receipt.GasConsumed,
		BlobGasUsed:       receipt.BlobGasUsed,
		BlobGasPrice:      receipt.BlobGasPrice,
		TxHash:            common.BytesToHash(receipt.ActionHash[:]),
		BlockNumber:       new(big.Int).SetUint64(receipt.BlockHeight),
		TransactionIndex:  uint(receipt.TxIndex),
		EffectiveGasPrice: receipt.EffectiveGasPrice,
	}
	if receipt.ContractAddress != "" {
		if addr, err := address.FromString(receipt.ContractAddress); err == nil {
			r.ContractAddress = common.BytesToAddress(addr.Bytes())
		}
	}
	for _, l := range receipt.Logs() {
		ethLog := &types.Log{
			Data:        l.Data,
			BlockNumber: l.BlockHeight,
			TxHash:      common.BytesToHash(l.ActionHash[:]),
			TxIndex:     uint(l.TxIndex),
			Index:       uint(l.Index),
		}
		if addr, err := address.FromString(l.Address); err == nil {
			ethLog.Address = common.BytesToAddress(addr.Bytes())
		}
		for _, topic := range l.Topics {
			ethLog.Topics = append(ethLog.Topics, common.BytesToHash(topic[:]))
		}
		r.Logs = append(r.Logs, ethLog)
	}
	return r
}

// BuildGenesisGethBlock creates a geth types.Block for the genesis block (height=0)
func BuildGenesisGethBlock(g genesis.Genesis) *types.Block {
	header := &types.Header{
		Number:     common.Big0,
		Time:       uint64(g.Timestamp),
		GasLimit:   g.BlockGasLimitByHeight(0),
		Difficulty: common.Big0,
	}
	return types.NewBlockWithHeader(header)
}

// BuildGenesisAlloc converts genesis InitBalanceMap to geth types.GenesisAlloc
func BuildGenesisAlloc(g genesis.Genesis) types.GenesisAlloc {
	alloc := make(types.GenesisAlloc)
	addrs, balances := g.Account.InitBalances()
	for i, addr := range addrs {
		ethAddr := common.BytesToAddress(addr.Bytes())
		alloc[ethAddr] = types.Account{
			Balance: balances[i],
		}
	}
	return alloc
}
