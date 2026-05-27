// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/iotexproject/iotex-address/address"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
)

// GenesisStateRoot is set during createGenesisStates() to the genesis DeltaStateDigest.
// BuildGenesisGethBlock uses it so the genesis geth block's Root field matches
// block 1's originRoot, preventing leafage from fetching a non-existent state diff.
var GenesisStateRoot common.Hash

// ConvertToGethBlock converts an iotex block.Block to a geth types.Block.
// The geth block serves as a data carrier for the pipeline tracer. We embed the
// IoTeX native hash in header.MixDigest so the tracer can extract it without
// cross-package imports. ParentHash is set to the IoTeX native parent hash
// (GenesisHash for block 1, blk.PrevHash() for others).
func ConvertToGethBlock(blk *block.Block, g genesis.Genesis) *types.Block {
	nativeHash := blk.HashBlock()
	var parentHash common.Hash
	if blk.Height() == 1 {
		genesisHash := block.GenesisHash()
		parentHash = common.BytesToHash(genesisHash[:])
	} else {
		prevHash := blk.PrevHash()
		parentHash = common.BytesToHash(prevHash[:])
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
		MixDigest:   common.BytesToHash(nativeHash[:]),
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
	// v1.15.11 WithBody takes a Body struct (was 2 separate args in v1.13).
	// Uncles/Withdrawals are empty for iotex blocks.
	return types.NewBlockWithHeader(header).WithBody(types.Body{Transactions: txs})
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

// BuildGenesisGethBlock creates a geth types.Block for the genesis block (height=0).
// MixDigest is set to GenesisHash() (config hash) — the IoTeX native "block hash" for genesis.
func BuildGenesisGethBlock(g genesis.Genesis) *types.Block {
	genesisHash := block.GenesisHash()
	header := &types.Header{
		Number:     common.Big0,
		Time:       uint64(g.Timestamp),
		GasLimit:   g.BlockGasLimitByHeight(0),
		Difficulty: common.Big0,
		Root:       GenesisStateRoot,
		MixDigest:  common.BytesToHash(genesisHash[:]),
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
