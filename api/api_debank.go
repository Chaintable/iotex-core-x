// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package api

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	ptracer "github.com/Chaintable/pipeline/tracer"
	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/Chaintable/pipeline/util"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"go.uber.org/zap"

	iotexAddress "github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-core/v2/blockchain"
	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
)

// buildSyntheticGethBlock builds a minimal geth block for RPCTracer.OnBlockStart().
// BaseFee is always set (0 if nil) to prevent pipeline panic.
func buildSyntheticGethBlock(blk *block.Block, g genesis.Genesis) *types.Block {
	gethBlock := blockchain.ConvertToGethBlock(blk, g)
	// ensure BaseFee is non-nil to prevent pipeline BuildPipelineTransaction panic
	if gethBlock.BaseFee() == nil {
		header := *gethBlock.Header()
		header.BaseFee = big.NewInt(0)
		gethBlock = types.NewBlockWithHeader(&header).WithBody(gethBlock.Transactions(), nil)
	}
	return gethBlock
}

// buildGenesisDebankOutput constructs the DebankOutPut for genesis block (height=0).
// This is special-cased because genesis has no transactions to replay.
func buildGenesisDebankOutput(g genesis.Genesis) (*ptypes.DebankOutPut, error) {
	gethBlock := blockchain.BuildGenesisGethBlock(g)
	header := util.BuildPilelineBlockHeader(gethBlock)
	alloc := blockchain.BuildGenesisAlloc(g)

	blockDiff := ptracer.GenesisAllocToStateDiff(alloc)
	blockDiff.Hash = header.StateRoot

	blockFile := &ptypes.BlockFile{
		Block:            util.BuildPipelineBlock(gethBlock),
		Txs:              make([]ptypes.Transaction, 0),
		Events:           make([]ptypes.Event, 0),
		Traces:           make([]ptypes.Trace, 0),
		ErrorEvents:      make([]ptypes.Event, 0),
		ErrorTraces:      make([]ptypes.Trace, 0),
		StorageContracts: make([]string, 0),
	}

	// build synthetic genesis txs and traces (same as go-ethereum api_debank.go)
	zeroAddr := "0x0000000000000000000000000000000000000000"
	txIdx := int64(0)

	sortedAddrs := make([]common.Address, 0, len(alloc))
	for addr := range alloc {
		sortedAddrs = append(sortedAddrs, addr)
	}
	sort.Slice(sortedAddrs, func(i, j int) bool {
		return sortedAddrs[i].Hex() < sortedAddrs[j].Hex()
	})

	for _, addr := range sortedAddrs {
		account := alloc[addr]
		addrLower := strings.ToLower(addr.Hex())

		if len(account.Storage) > 0 {
			blockFile.StorageContracts = append(blockFile.StorageContracts, addrLower)
		}

		// balance transfers
		if account.Balance != nil && account.Balance.Sign() > 0 {
			txID := fmt.Sprintf("0xgenesis01%013d%s", 0, addrLower)
			blockFile.Txs = append(blockFile.Txs, ptypes.Transaction{
				ID: txID, From: zeroAddr, To: addrLower,
				Gas: big.NewInt(0), GasPrice: big.NewInt(0), GasUsed: big.NewInt(0),
				Status: true, GasFeeCap: big.NewInt(0), GasTipCap: big.NewInt(0),
				Input: []byte{}, Nonce: big.NewInt(0), TransactionIndex: txIdx,
				Value: (*hexutil.Big)(account.Balance),
			})
			traceID := util.ToHash([]string{txID, "", "0"})
			blockFile.Traces = append(blockFile.Traces, ptypes.Trace{
				ID: traceID, From: zeroAddr, To: addrLower,
				Gas: big.NewInt(0), GasUsed: big.NewInt(0),
				Input: []byte{}, Output: []byte{},
				Value: (*hexutil.Big)(account.Balance),
				CallCreateType: "call", CallType: "call",
				TxID: txID, TraceAddress: []int64{},
			})
			txIdx++
		}

		// code deployments
		if len(account.Code) > 0 {
			txID := fmt.Sprintf("0xgenesis02%013d%s", 0, addrLower)
			blockFile.Txs = append(blockFile.Txs, ptypes.Transaction{
				ID: txID, From: zeroAddr, To: addrLower,
				Gas: big.NewInt(0), GasPrice: big.NewInt(0), GasUsed: big.NewInt(0),
				Status: true, GasFeeCap: big.NewInt(0), GasTipCap: big.NewInt(0),
				Input: account.Code, Nonce: big.NewInt(0), TransactionIndex: txIdx,
				Value: (*hexutil.Big)(big.NewInt(0)),
			})
			traceID := util.ToHash([]string{txID, "", "0"})
			blockFile.Traces = append(blockFile.Traces, ptypes.Trace{
				ID: traceID, From: zeroAddr, To: addrLower,
				Gas: big.NewInt(0), GasUsed: big.NewInt(0),
				Input: account.Code, Output: account.Code,
				Value: (*hexutil.Big)(big.NewInt(0)),
				CallCreateType: "create", TxID: txID, TraceAddress: []int64{},
			})
			txIdx++
		}
	}

	// native token contract (0xEEEE...)
	nativeTokenAddr := "0xeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	nativeTokenTxID := fmt.Sprintf("0xgenesis03%013d%s", 0, nativeTokenAddr)
	blockFile.Txs = append(blockFile.Txs, ptypes.Transaction{
		ID: nativeTokenTxID, From: zeroAddr, To: nativeTokenAddr,
		Gas: big.NewInt(0), GasPrice: big.NewInt(0), GasUsed: big.NewInt(0),
		Status: true, GasFeeCap: big.NewInt(0), GasTipCap: big.NewInt(0),
		Input: []byte{}, Nonce: big.NewInt(0), TransactionIndex: txIdx,
		Value: (*hexutil.Big)(big.NewInt(0)),
	})
	nativeTokenTraceID := util.ToHash([]string{nativeTokenTxID, "", "0"})
	blockFile.Traces = append(blockFile.Traces, ptypes.Trace{
		ID: nativeTokenTraceID, From: zeroAddr, To: nativeTokenAddr,
		Gas: big.NewInt(0), GasUsed: big.NewInt(0),
		Input: []byte{}, Output: []byte{},
		Value: (*hexutil.Big)(big.NewInt(0)),
		CallCreateType: "create", TxID: nativeTokenTxID, TraceAddress: []int64{},
	})

	var stateDiffBytes []byte
	if blockDiff != nil {
		var err error
		stateDiffBytes, err = util.EncodeToRlp(blockDiff)
		if err != nil {
			log.L().Warn("Failed to RLP encode genesis state diff", zap.Error(err))
			stateDiffBytes = []byte{}
		}
	} else {
		stateDiffBytes = []byte{}
	}

	return &ptypes.DebankOutPut{
		BlockFile:      blockFile,
		Header:         header,
		StateDiff:      hexutil.Bytes(stateDiffBytes),
		ValidationHash: blockFile.Validation().ValidationHash,
	}, nil
}

// perActionStateDiff holds per-action EVM state diff data.
type perActionStateDiff struct {
	destructs map[common.Hash]struct{}
	accounts  map[common.Hash][]byte
	storages  map[common.Hash]map[common.Hash][]byte
	codes     map[common.Hash][]byte
}

// mergeStateDiffs merges multiple per-action EVM state diffs into a single block-level diff.
// Later actions overwrite earlier actions for the same keys (union merge).
func mergeStateDiffs(diffs []perActionStateDiff) (
	destructs map[common.Hash]struct{},
	accounts map[common.Hash][]byte,
	storages map[common.Hash]map[common.Hash][]byte,
	codes map[common.Hash][]byte,
) {
	destructs = make(map[common.Hash]struct{})
	accounts = make(map[common.Hash][]byte)
	storages = make(map[common.Hash]map[common.Hash][]byte)
	codes = make(map[common.Hash][]byte)

	for _, d := range diffs {
		for k, v := range d.destructs {
			destructs[k] = v
		}
		for k, v := range d.accounts {
			accounts[k] = v
		}
		for addr, slots := range d.storages {
			if existing, ok := storages[addr]; ok {
				for k, v := range slots {
					existing[k] = v
				}
			} else {
				storages[addr] = slots
			}
		}
		for k, v := range d.codes {
			codes[k] = v
		}
	}
	return
}

// convertToGethReceipt wraps blockchain.ConvertToGethReceipt with baseFee for RPC tracer.
func convertActionReceiptToGethReceipt(receipt *action.Receipt, selp *action.SealedEnvelope) *types.Receipt {
	gethReceipt := blockchain.ConvertToGethReceipt(receipt)
	if gethReceipt == nil {
		return nil
	}
	// set TxHash from the action's eth tx hash
	if ethTx, err := selp.ToEthTx(); err == nil {
		gethReceipt.TxHash = ethTx.Hash()
	}
	return gethReceipt
}

// emitTransferLogsAsEvents converts each TransactionLog in the receipt into an
// eth types.Log and pushes it through the tracer's log emission path so it
// appears in block_file.events. This surfaces pool flows (GRANT_REWARD,
// CLAIM_FROM_REWARDING, GAS_FEE, BUCKET_CREATE_AMOUNT, etc.) that are native
// IoTeX constructs, not EVM LOG opcodes.
func emitTransferLogsAsEvents(rpcTracer *iotexRPCTracer, receipt *action.Receipt) {
	if receipt == nil || len(receipt.TransactionLogs()) == 0 {
		return
	}
	// address.RewardingProtocol is the standard iotex-compat bech32 used by
	// eth_getTransactionReceipt for the emitter of transfer-style logs.
	transferLogs, err := receipt.TransferLogs(iotexAddress.RewardingProtocol, 0)
	if err != nil {
		return
	}
	for _, l := range transferLogs {
		ethLog := &types.Log{
			Data:        l.Data,
			BlockNumber: l.BlockHeight,
			TxHash:      common.BytesToHash(l.ActionHash[:]),
			TxIndex:     uint(l.TxIndex),
		}
		if addr, err := iotexAddress.FromString(l.Address); err == nil {
			ethLog.Address = common.BytesToAddress(addr.Bytes())
		}
		for _, topic := range l.Topics {
			ethLog.Topics = append(ethLog.Topics, common.BytesToHash(topic[:]))
		}
		rpcTracer.EmitTransferLog(ethLog)
	}
}
