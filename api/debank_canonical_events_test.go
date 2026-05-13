package api

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/Chaintable/pipeline/util"
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
	out := buildCanonicalEvents(nil, nil, nil, nil, 0)
	r.Empty(out)

	out = buildCanonicalEvents([]*action.Receipt{}, []string{}, nil, nil, 0)
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
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, nil, nil, 0)
	r.Len(out, 1)

	ev := out[0]
	expectedAddr := strings.ToLower(common.BytesToAddress(contractAddr.Bytes()).Hex())
	r.Equal(expectedAddr, ev.Address)
	r.Equal(strings.ToLower(common.BytesToHash(topic0[:]).Hex()), ev.Selector)
	r.Len(ev.Topics, 1)
	r.Equal(strings.ToLower(common.BytesToHash(topic1[:]).Hex()), ev.Topics[0])
	r.Equal([]byte{0xab, 0xcd}, []byte(ev.Data))
	r.EqualValues(0, ev.LogIndex)
	// nil maps -> three trace-binding fields stay at zero value
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
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_GAS_FEE,
		Amount:    big.NewInt(1000),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	txID := "0x" + strings.Repeat("bb", 32)
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, nil, nil, 0)
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
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, nil, nil, 0)
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

	r1 := mkRcpt("tx1", 3)
	r2 := mkRcpt("tx2", 0)
	r3 := mkRcpt("tx3", 2)

	txIDs := []string{
		"0x" + strings.Repeat("01", 32),
		"0x" + strings.Repeat("02", 32),
		"0x" + strings.Repeat("03", 32),
	}
	out := buildCanonicalEvents([]*action.Receipt{r1, r2, r3}, txIDs, nil, nil, 0)
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
		nil, nil, 0,
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
	out := buildCanonicalEvents(
		[]*action.Receipt{rcpt}, []string{"0x1", "0x2"},
		nil, nil, 0,
	)
	r.Empty(out, "length mismatch is a caller bug; return empty rather than crash")
}

// ---- New trace-binding tests below ----

// mkEVMRcpt builds a receipt with `n` EVM logs whose Index runs from `startIdx`.
func mkEVMRcpt(seed string, startIdx uint32, n int) *action.Receipt {
	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte(seed)),
	}
	for i := 0; i < n; i++ {
		rcpt.AddLogs(&action.Log{
			Address: identityset.Address(1).String(),
			Topics:  action.Topics{hash.Hash256b([]byte(seed + "t"))},
			Index:   startIdx + uint32(i),
		})
	}
	return rcpt
}

func TestBuildCanonicalEvents_HappyPath_PreciseBinding(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("aa", 32)
	rcpt := mkEVMRcpt("tx-happy", 0, 1)

	binding := eventBinding{
		parentTraceID: "frame-1",
		position:      7,
		id:            util.ToHash([]string{"frame-1", "7"}),
	}
	logMap := map[uint32]eventBinding{0: binding}
	rootMap := map[string]string{txID: "root-of-tx"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, logMap, rootMap, 100)
	r.Len(out, 1)
	r.Equal("frame-1", out[0].ParentTraceID, "precise frame binding from map")
	r.EqualValues(7, out[0].Position)
	r.Equal(binding.id, out[0].ID)
}

func TestBuildCanonicalEvents_DriftFallback(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("bb", 32)
	rcpt := mkEVMRcpt("tx-drift", 0, 2) // log Indices 0 and 1

	// replay only captured log 0 (drift case)
	logMap := map[uint32]eventBinding{
		0: {parentTraceID: "frame-A", position: 2, id: util.ToHash([]string{"frame-A", "2"})},
	}
	rootMap := map[string]string{txID: "root-tx-drift"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, logMap, rootMap, 100)
	r.Len(out, 2)

	// log[0]: precise
	r.Equal("frame-A", out[0].ParentTraceID)
	r.EqualValues(2, out[0].Position)

	// log[1]: drift, fallback to root
	r.Equal("root-tx-drift", out[1].ParentTraceID)
	r.EqualValues(1, out[1].Position, "fallback uses block-global logIdx")
	r.Equal(util.ToHash([]string{"root-tx-drift", "1"}), out[1].ID)
}

func TestBuildCanonicalEvents_NilMapsLeavesBindingEmpty(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("cc", 32)
	rcpt := mkEVMRcpt("tx-nil", 0, 1)

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, nil, nil, 100)
	r.Len(out, 1)
	r.Empty(out[0].ParentTraceID)
	r.EqualValues(0, out[0].Position)
	r.Empty(out[0].ID)
	r.EqualValues(0, out[0].LogIndex, "LogIndex still populated even when maps are nil")
}

func TestBuildCanonicalEvents_Synthetic_AlwaysFallback(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("dd", 32)

	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx-synth")),
	}
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_GAS_FEE,
		Amount:    big.NewInt(1),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	// even if logMap has entry, synthetic events never look it up
	logMap := map[uint32]eventBinding{0: {parentTraceID: "should-not-be-used"}}
	rootMap := map[string]string{txID: "root-synth"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, logMap, rootMap, 100)
	r.Len(out, 1)
	r.Equal("root-synth", out[0].ParentTraceID, "synthetic always falls back to root, never queries logMap")
	r.EqualValues(0, out[0].Position)
	r.Equal(util.ToHash([]string{"root-synth", "0"}), out[0].ID)
}

func TestBuildCanonicalEvents_Synthetic_NoRootTrace(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("ee", 32)

	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx-nonevm")),
	}
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_CLAIM_FROM_REWARDING_FUND,
		Amount:    big.NewInt(42),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	// non-EVM action: rootTraceByTx doesn't contain this tx
	logMap := map[uint32]eventBinding{}
	rootMap := map[string]string{} // tx not present

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, logMap, rootMap, 100)
	r.Len(out, 1)
	r.Empty(out[0].ParentTraceID, "no root trace -> binding left empty + warn (not crash)")
	r.EqualValues(0, out[0].Position)
	r.Empty(out[0].ID)
}

func TestBuildCanonicalEvents_MixedTx(t *testing.T) {
	r := require.New(t)
	tx1 := "0x" + strings.Repeat("01", 32)
	tx2 := "0x" + strings.Repeat("02", 32)

	r1 := mkEVMRcpt("rcpt1", 0, 1) // 1 EVM log @ Index=0
	r1.AddTransactionLogs(&action.TransactionLog{
		Type: iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
		Amount: big.NewInt(1), Sender: identityset.Address(2).String(), Recipient: identityset.Address(3).String(),
	})

	r2 := &action.Receipt{Status: uint64(iotextypes.ReceiptStatus_Success), ActionHash: hash.Hash256b([]byte("rcpt2"))}
	r2.AddTransactionLogs(&action.TransactionLog{
		Type: iotextypes.TransactionLogType_CLAIM_FROM_REWARDING_FUND,
		Amount: big.NewInt(2), Sender: identityset.Address(4).String(), Recipient: identityset.Address(5).String(),
	})

	logMap := map[uint32]eventBinding{
		0: {parentTraceID: "f1", position: 0, id: util.ToHash([]string{"f1", "0"})},
	}
	rootMap := map[string]string{
		tx1: "root-tx1",
		// tx2 missing on purpose (non-EVM)
	}

	out := buildCanonicalEvents([]*action.Receipt{r1, r2}, []string{tx1, tx2}, logMap, rootMap, 100)
	r.Len(out, 3, "1 EVM + 1 synthetic (tx1) + 1 synthetic (tx2)")

	// tx1 EVM log: precise
	r.Equal("f1", out[0].ParentTraceID)
	// tx1 synthetic: fallback to root-tx1
	r.Equal("root-tx1", out[1].ParentTraceID)
	// tx2 synthetic: no root -> empty
	r.Empty(out[2].ParentTraceID)

	// global LogIndex monotonic
	for i, ev := range out {
		r.EqualValues(i, ev.LogIndex)
	}
}

func TestBuildCanonicalEvents_IDNoCollisionAcrossFallbacks(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("ff", 32)
	rcpt := &action.Receipt{Status: uint64(iotextypes.ReceiptStatus_Success), ActionHash: hash.Hash256b([]byte("tx-many"))}
	// 3 synthetic logs in same tx
	for i := 0; i < 3; i++ {
		rcpt.AddTransactionLogs(&action.TransactionLog{
			Type:   iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
			Amount: big.NewInt(int64(i + 1)),
			Sender: identityset.Address(2).String(), Recipient: identityset.Address(3).String(),
		})
	}

	rootMap := map[string]string{txID: "root-many"}
	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, nil, rootMap, 100)
	r.Len(out, 3)

	seen := make(map[string]struct{})
	for i, ev := range out {
		r.Equal("root-many", ev.ParentTraceID)
		r.EqualValues(i, ev.Position, "Position uses block-global logIdx so each fallback is unique")
		r.Equal(util.ToHash([]string{"root-many", fmt.Sprintf("%d", i)}), ev.ID)
		_, dup := seen[ev.ID]
		r.False(dup, "ID must be unique across synthetic fallbacks in same tx")
		seen[ev.ID] = struct{}{}
	}
}

func TestExtractLogIndexToBinding(t *testing.T) {
	r := require.New(t)
	events := []ptypes.Event{
		{LogIndex: 0, ParentTraceID: "f0", Position: 1, ID: "id0"},
		{LogIndex: 3, ParentTraceID: "f3", Position: 0, ID: "id3"},
	}
	errEvents := []ptypes.Event{
		{LogIndex: 5, ParentTraceID: "fE", Position: 0, ID: "idE"},
	}
	m := extractLogIndexToBinding(events, errEvents)
	r.Len(m, 3)
	r.Equal("f0", m[0].parentTraceID)
	r.EqualValues(1, m[0].position)
	r.Equal("id0", m[0].id)
	r.Equal("f3", m[3].parentTraceID)
	r.Equal("fE", m[5].parentTraceID)
}

func TestExtractRootTraceByTx(t *testing.T) {
	r := require.New(t)
	traces := []ptypes.Trace{
		{TxID: "tx1", ID: "root1", TraceAddress: []int64{}},
		{TxID: "tx1", ID: "sub1", TraceAddress: []int64{0}},
		{TxID: "tx2", ID: "root2", TraceAddress: []int64{}},
	}
	errTraces := []ptypes.Trace{
		{TxID: "tx3", ID: "rootErr", TraceAddress: []int64{}},
	}
	m := extractRootTraceByTx(traces, errTraces)
	r.Len(m, 3)
	r.Equal("root1", m["tx1"])
	r.Equal("root2", m["tx2"])
	r.Equal("rootErr", m["tx3"])
}
