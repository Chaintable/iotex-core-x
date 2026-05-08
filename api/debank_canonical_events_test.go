package api

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol/account"
	"github.com/iotexproject/iotex-core/v2/test/identityset"
)

func TestBuildCanonicalEvents_EmptyReceipts(t *testing.T) {
	r := require.New(t)
	out := buildCanonicalEvents(nil, nil)
	r.Empty(out)

	out = buildCanonicalEvents([]*action.Receipt{}, []string{})
	r.Empty(out)
}

func TestBuildCanonicalEvents_FromEVMLogs(t *testing.T) {
	r := require.New(t)

	contractAddr := identityset.Address(1)
	topic0 := hash.Hash256b([]byte("Transfer(address,address,uint256)"))
	topic1 := hash.Hash256b([]byte("topic1"))
	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx0")),
	}
	rcpt.AddLogs(&action.Log{
		Address: contractAddr.String(),
		Topics:  action.Topics{topic0, topic1},
		Data:    []byte{0xab, 0xcd},
	})

	txID := "0x" + strings.Repeat("aa", 32)
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID})
	r.Len(out, 1)

	ev := out[0]
	expectedAddr := strings.ToLower(common.BytesToAddress(contractAddr.Bytes()).Hex())
	r.Equal(expectedAddr, ev.Address)
	r.Equal(strings.ToLower(common.BytesToHash(topic0[:]).Hex()), ev.Selector)
	r.Len(ev.Topics, 1)
	r.Equal(strings.ToLower(common.BytesToHash(topic1[:]).Hex()), ev.Topics[0])
	r.Equal([]byte{0xab, 0xcd}, []byte(ev.Data))
	r.EqualValues(0, ev.LogIndex)
	// Trace attribution intentionally left empty — leafage RPC re-derives from its own
	// CallTraceNode tree (crates/leafage-evm-rpc/src/api_impl/utils.rs:build_trace_node).
	r.Empty(ev.ParentTraceID)
	r.EqualValues(0, ev.Position)
	r.Empty(ev.ID)
}

func TestBuildCanonicalEvents_FromTransactionLogs(t *testing.T) {
	r := require.New(t)

	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx_synthetic")),
	}
	// Synthetic transfer: GAS_FEE from sender to coinbase
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_GAS_FEE,
		Amount:    big.NewInt(1000),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	txID := "0x" + strings.Repeat("bb", 32)
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID})
	r.Len(out, 1)

	ev := out[0]
	expectedAddr := strings.ToLower(common.BytesToAddress(account.ProtocolAddr().Bytes()).Hex())
	r.Equal(expectedAddr, ev.Address, "synthetic log emitter must be account.ProtocolAddr")
	r.NotEmpty(ev.Selector, "PackAccountTransferEvent always sets topic[0]")
	r.NotEmpty(ev.Data)
	r.EqualValues(0, ev.LogIndex)
}

func TestBuildCanonicalEvents_BothEVMAndTransactionLogs(t *testing.T) {
	r := require.New(t)

	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx_combined")),
	}
	rcpt.AddLogs(&action.Log{
		Address: identityset.Address(1).String(),
		Topics:  action.Topics{hash.Hash256b([]byte("evm-topic"))},
	})
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
		Amount:    big.NewInt(500),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	txID := "0x" + strings.Repeat("cc", 32)
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID})
	r.Len(out, 2, "1 EVM log + 1 synthetic log")
	r.EqualValues(0, out[0].LogIndex, "EVM log first")
	r.EqualValues(1, out[1].LogIndex, "synthetic log second")
	expectedProto := strings.ToLower(common.BytesToAddress(account.ProtocolAddr().Bytes()).Hex())
	r.Equal(expectedProto, out[1].Address)
	r.NotEqual(expectedProto, out[0].Address)
}

func TestBuildCanonicalEvents_LogIndexAcrossReceipts(t *testing.T) {
	r := require.New(t)

	mkRcpt := func(seed string, n int) *action.Receipt {
		rcpt := &action.Receipt{
			Status:      uint64(iotextypes.ReceiptStatus_Success),
			BlockHeight: 100,
			ActionHash:  hash.Hash256b([]byte(seed)),
		}
		for i := 0; i < n; i++ {
			rcpt.AddLogs(&action.Log{
				Address: identityset.Address(1).String(),
				Topics:  action.Topics{hash.Hash256b([]byte(seed + "topic"))},
			})
		}
		return rcpt
	}

	r1 := mkRcpt("tx1", 3) // 3 logs
	r2 := mkRcpt("tx2", 0) // 0 logs (empty)
	r3 := mkRcpt("tx3", 2) // 2 logs

	txIDs := []string{
		"0x" + strings.Repeat("01", 32),
		"0x" + strings.Repeat("02", 32),
		"0x" + strings.Repeat("03", 32),
	}
	out := buildCanonicalEvents([]*action.Receipt{r1, r2, r3}, txIDs)
	r.Len(out, 5)
	for i, ev := range out {
		r.EqualValues(i, ev.LogIndex, "log idx must be globally monotonic across receipts")
	}
}

func TestBuildCanonicalEvents_NilReceiptSkipped(t *testing.T) {
	r := require.New(t)
	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx-real")),
	}
	rcpt.AddLogs(&action.Log{
		Address: identityset.Address(1).String(),
		Topics:  action.Topics{hash.Hash256b([]byte("t"))},
	})
	out := buildCanonicalEvents(
		[]*action.Receipt{nil, rcpt, nil},
		[]string{"0xaa", "0xbb", "0xcc"},
	)
	r.Len(out, 1, "nil receipts skipped")
}

func TestBuildCanonicalEvents_LengthMismatchReturnsEmpty(t *testing.T) {
	r := require.New(t)
	rcpt := &action.Receipt{}
	rcpt.AddLogs(&action.Log{
		Address: identityset.Address(1).String(),
		Topics:  action.Topics{hash.Hash256b([]byte("t"))},
	})
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{"0x1", "0x2"})
	r.Empty(out, "length mismatch is a caller bug; return empty rather than crash")
}
