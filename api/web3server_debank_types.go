package api

import (
	"encoding/json"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// Wire-compatible with the DeBank RPC standard. The canonical reference is
// leafage-evm's `crates/leafage-evm-types/src/rpc/debank.rs` (DebankErrorCode
// enum and the Debank* structs); the cosmos-evm fork
// (`rpc/types/debank.go` + `x/vm/types/simulate_result.go`) is another
// reference impl, but its error-code list is incomplete (missing -39002 etc.)
// — follow leafage when they disagree.
//
// Field names and JSON tags must match exactly so nodex-proxy can forward
// requests/responses without chain-specific knowledge.

const (
	debankSimulateErrorReverted              = -39000 // EvmRevert
	debankSimulateErrorGasExhausted          = -39001 // GasExhausted
	debankSimulateErrorInsufficientBalance   = -39002 // BalanceExhausted
	debankSimulateErrorNonceError            = -39003 // NonceError
	debankSimulateErrorUnknown               = -39004 // EvmFailed
	debankSimulateErrorUnsupportedPrecompile = -39008 // UnsupportedPrecompile
)

type debankBlockType int

const (
	debankBlockTypeEquals debankBlockType = iota
	debankBlockTypeContains
)

func (bt debankBlockType) String() string {
	switch bt {
	case debankBlockTypeContains:
		return "Contains"
	default:
		return "Equals"
	}
}

func (bt debankBlockType) MarshalJSON() ([]byte, error) {
	return json.Marshal(bt.String())
}

func (bt *debankBlockType) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "Contains":
		*bt = debankBlockTypeContains
	default:
		*bt = debankBlockTypeEquals
	}
	return nil
}

type debankBlockContext struct {
	BlockId   rpc.BlockNumberOrHash `json:"block_id"`
	BlockType debankBlockType       `json:"type"`
}

type debankCallArgs struct {
	From     *common.Address `json:"from"`
	To       *common.Address `json:"to"`
	Gas      *hexutil.Uint64 `json:"gas"`
	GasPrice *hexutil.Big    `json:"gasPrice"`
	Value    *hexutil.Big    `json:"value"`
	Data     *hexutil.Bytes  `json:"data"`
	Nonce    *hexutil.Uint64 `json:"nonce"`
	ChainID  *big.Int        `json:"chainId,omitempty"`
}

type debankTransactionArgs struct {
	From                 *common.Address   `json:"from"`
	To                   *common.Address   `json:"to"`
	Gas                  *hexutil.Uint64   `json:"gas"`
	GasPrice             *hexutil.Big      `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big      `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big      `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big      `json:"value"`
	Nonce                *hexutil.Uint64   `json:"nonce"`
	Data                 *hexutil.Bytes    `json:"data"`
	Input                *hexutil.Bytes    `json:"input"`
	AccessList           *types.AccessList `json:"accessList,omitempty"`
	ChainID              *hexutil.Big      `json:"chainId,omitempty"`
}

type debankSingleSimulateResult struct {
	Code    int           `json:"code"`
	Err     string        `json:"err"`
	GasUsed uint64        `json:"gas_used"`
	Traces  []debankTrace `json:"traces"`
	Events  []debankEvent `json:"events"`
}

type debankTrace struct {
	ID                string        `json:"id"`
	From              string        `json:"from_addr"`
	Gas               *big.Int      `json:"gas_limit"`
	Input             hexutil.Bytes `json:"input"`
	To                string        `json:"to_addr"`
	Value             *hexutil.Big  `json:"value"`
	GasUsed           *big.Int      `json:"gas_used"`
	Output            hexutil.Bytes `json:"output"`
	CallCreateType    string        `json:"type"`
	CallType          string        `json:"call_type"`
	TxID              common.Hash   `json:"tx_id"`
	ParentTraceID     string        `json:"parent_trace_id"`
	PosInParentTrace  int64         `json:"pos_in_parent_trace"`
	SelfStorageChange bool          `json:"self_storage_change"`
	StorageChange     bool          `json:"storage_change"`
}

type debankEvent struct {
	ID            string        `json:"id"`
	Address       string        `json:"contract_id"`
	Selector      string        `json:"selector"`
	Topics        []string      `json:"topics"`
	Data          hexutil.Bytes `json:"data"`
	TxId          common.Hash   `json:"tx_id"`
	ParentTraceID string        `json:"parent_trace_id"`
	Position      int64         `json:"pos_in_parent_trace"`
}

type debankSimulateStats struct {
	BlockNum  uint64      `json:"block_num"`
	BlockHash common.Hash `json:"block_hash"`
	BlockTime int64       `json:"block_time"`
	Success   bool        `json:"success"`
}

type debankSimulateResp struct {
	Results []debankSingleSimulateResult `json:"results"`
	Stats   debankSimulateStats          `json:"stats"`
}

type debankSingleCallResult struct {
	Code      int           `json:"code"`
	Err       string        `json:"err"`
	FromCache bool          `json:"from_cache"`
	Result    hexutil.Bytes `json:"result"`
	GasUsed   int64         `json:"gas_used"`
	TimeCost  float64       `json:"time_cost"`
}

type debankMultiCallStats struct {
	BlockNum     uint64      `json:"block_num"`
	BlockHash    common.Hash `json:"block_hash"`
	BlockTime    int64       `json:"block_time"`
	Success      bool        `json:"success"`
	CacheEnabled bool        `json:"cache_enabled"`
}

type debankMultiCallResp struct {
	Results []*debankSingleCallResult `json:"results"`
	Stats   *debankMultiCallStats     `json:"stats"`
}
