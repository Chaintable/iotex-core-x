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
		Index:   7, // iotex-side log.Index propagates straight through to ev.LogIndex
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
	r.EqualValues(7, ev.LogIndex, "ev.LogIndex must reflect the iotex receipt's log.Index, not a builder counter")
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
		Index:   0,
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
	expectedProto := strings.ToLower(common.BytesToAddress(account.ProtocolAddr().Bytes()).Hex())
	r.Equal(expectedProto, out[1].Address)
	r.NotEqual(expectedProto, out[0].Address)
	// LogIndex values are receipt-side: EVM log carries its assigned Index;
	// transferLog (via r.TransferLogs(_, 0)) starts at 0 locally — Step 5 will
	// align this with eth_getTransactionReceipt's per-tx allocation. For now
	// just verify both values were populated from their respective source.
	r.EqualValues(0, out[0].LogIndex)
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
		Index:   4,
	})
	out := buildCanonicalEvents(
		[]*action.Receipt{nil, rcpt, nil},
		[]string{"0xaa", "0xbb", "0xcc"},
		nil, nil, 0,
	)
	r.Len(out, 1, "nil receipts skipped")
	r.EqualValues(4, out[0].LogIndex)
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

// ---- trace-binding tests (new key: (txID, inTxIdx)) ----

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
	// Per-tx ordinal 0 -> binding (replay-side callTracer saw the same log
	// as the 0-th OnLog within this tx, so InTxLogIdx == 0).
	bindingMap := map[bindingKey]eventBinding{{txID: txID, inTxIdx: 0}: binding}
	rootMap := map[string]string{txID: "root-of-tx"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, bindingMap, rootMap, 100)
	r.Len(out, 1)
	r.Equal("frame-1", out[0].ParentTraceID, "precise frame binding from (txID, inTxIdx) map")
	r.EqualValues(7, out[0].Position)
	r.Equal(binding.id, out[0].ID)
	r.EqualValues(0, out[0].LogIndex, "ev.LogIndex copies iotex receipt's log.Index")
}

func TestBuildCanonicalEvents_DriftFallback(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("bb", 32)
	rcpt := mkEVMRcpt("tx-drift", 0, 2) // log Indices 0 and 1

	// replay only captured the 0-th log within this tx (drift case)
	bindingMap := map[bindingKey]eventBinding{
		{txID: txID, inTxIdx: 0}: {
			parentTraceID: "frame-A",
			position:      2,
			id:            util.ToHash([]string{"frame-A", "2"}),
		},
	}
	rootMap := map[string]string{txID: "root-tx-drift"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, bindingMap, rootMap, 100)
	r.Len(out, 2)

	// log[0]: precise
	r.Equal("frame-A", out[0].ParentTraceID)
	r.EqualValues(2, out[0].Position)
	r.EqualValues(0, out[0].LogIndex)

	// log[1]: drift, fallback to root
	r.Equal("root-tx-drift", out[1].ParentTraceID)
	r.EqualValues(1, out[1].Position, "fallback uses block-global logIdx counter for uniqueness")
	r.Equal(util.ToHash([]string{"root-tx-drift", "1"}), out[1].ID)
	r.EqualValues(1, out[1].LogIndex, "still copies iotex Index even on fallback")
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

func TestBuildCanonicalEvents_Synthetic_FallbackWhenUnboundReplay(t *testing.T) {
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

	// Even though replay had a binding for a DIFFERENT tx's 0-th log,
	// this synthetic doesn't match by (txID, inTxIdx) so it falls back.
	otherTxID := "0x" + strings.Repeat("99", 32)
	bindingMap := map[bindingKey]eventBinding{
		{txID: otherTxID, inTxIdx: 0}: {parentTraceID: "should-not-be-used"},
	}
	rootMap := map[string]string{txID: "root-synth"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, bindingMap, rootMap, 100)
	r.Len(out, 1)
	r.Equal("root-synth", out[0].ParentTraceID, "no (txID, inTxIdx) match -> fallback to root")
	r.EqualValues(0, out[0].Position)
	r.Equal(util.ToHash([]string{"root-synth", "0"}), out[0].ID)
}

func TestBuildCanonicalEvents_Synthetic_PreciseBindingWhenReplayMatched(t *testing.T) {
	r := require.New(t)
	txID := "0x" + strings.Repeat("ab", 32)

	rcpt := &action.Receipt{
		Status:      uint64(iotextypes.ReceiptStatus_Success),
		BlockHeight: 100,
		ActionHash:  hash.Hash256b([]byte("tx-synth-bound")),
	}
	rcpt.AddTransactionLogs(&action.TransactionLog{
		Type:      iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
		Amount:    big.NewInt(1),
		Sender:    identityset.Address(2).String(),
		Recipient: identityset.Address(3).String(),
	})

	// Non-Execution actions emit their receipt logs through EmitTransferLogs ->
	// callTracer.OnLog as well, so a precise binding CAN exist for synthetic
	// logs. Verify we use it instead of falling back to root.
	bindingMap := map[bindingKey]eventBinding{
		{txID: txID, inTxIdx: 0}: {
			parentTraceID: "synthetic-frame",
			position:      11,
			id:            util.ToHash([]string{"synthetic-frame", "11"}),
		},
	}
	rootMap := map[string]string{txID: "root-synth-bound"}

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, bindingMap, rootMap, 100)
	r.Len(out, 1)
	r.Equal("synthetic-frame", out[0].ParentTraceID, "synthetic log uses precise binding when present")
	r.EqualValues(11, out[0].Position)
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
	bindingMap := map[bindingKey]eventBinding{}
	rootMap := map[string]string{} // tx not present

	out := buildCanonicalEvents([]*action.Receipt{rcpt}, []string{txID}, bindingMap, rootMap, 100)
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
		Type:   iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
		Amount: big.NewInt(1), Sender: identityset.Address(2).String(), Recipient: identityset.Address(3).String(),
	})

	r2 := &action.Receipt{Status: uint64(iotextypes.ReceiptStatus_Success), ActionHash: hash.Hash256b([]byte("rcpt2"))}
	r2.AddTransactionLogs(&action.TransactionLog{
		Type:   iotextypes.TransactionLogType_CLAIM_FROM_REWARDING_FUND,
		Amount: big.NewInt(2), Sender: identityset.Address(4).String(), Recipient: identityset.Address(5).String(),
	})

	bindingMap := map[bindingKey]eventBinding{
		{txID: tx1, inTxIdx: 0}: {parentTraceID: "f1", position: 0, id: util.ToHash([]string{"f1", "0"})},
	}
	rootMap := map[string]string{
		tx1: "root-tx1",
		// tx2 missing on purpose (non-EVM)
	}

	out := buildCanonicalEvents([]*action.Receipt{r1, r2}, []string{tx1, tx2}, bindingMap, rootMap, 100)
	r.Len(out, 3, "1 EVM + 1 synthetic (tx1) + 1 synthetic (tx2)")

	// tx1 EVM log: precise
	r.Equal("f1", out[0].ParentTraceID)
	// tx1 synthetic at inTxIdx=1 (no binding) -> fallback to root-tx1
	r.Equal("root-tx1", out[1].ParentTraceID)
	// tx2 synthetic: no root -> empty
	r.Empty(out[2].ParentTraceID)
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

// TestBuildCanonicalEvents_TransferLogIdxMatchesEthAPI mirrors block 48125704:
// tx-A (TxIndex=0) has 5 EVM logs (Index 0..4) + 4 native TransactionLogs;
// tx-B (TxIndex=1) has 1 EVM log (Index 5). eth_getTransactionReceipt allocates
// tx-A's synthetic transferLog indices starting at 6 (== block_total_EVM_logs
// = 5+1, since tx-A has no earlier tx's txLogs). Our buildCanonicalEvents must
// produce the same allocation.
func TestBuildCanonicalEvents_TransferLogIdxMatchesEthAPI(t *testing.T) {
	r := require.New(t)
	txA := "0x" + strings.Repeat("aa", 32)
	txB := "0x" + strings.Repeat("bb", 32)

	// tx-A: 5 EVM logs at iotex Index 0..4
	rA := mkEVMRcpt("tx-A", 0, 5)
	rA.TxIndex = 0
	// 4 native TransactionLogs (would convert to synthetic transferLogs in eth)
	for i := 0; i < 4; i++ {
		rA.AddTransactionLogs(&action.TransactionLog{
			Type:      iotextypes.TransactionLogType_IN_CONTRACT_TRANSFER,
			Amount:    big.NewInt(int64(i + 1)),
			Sender:    identityset.Address(2).String(),
			Recipient: identityset.Address(3).String(),
		})
	}

	// tx-B: 1 EVM log at iotex Index 5
	rB := mkEVMRcpt("tx-B", 5, 1)
	rB.TxIndex = 1

	out := buildCanonicalEvents([]*action.Receipt{rA, rB}, []string{txA, txB}, nil, nil, 100)
	r.Len(out, 5+4+1, "5 EVM (tx-A) + 4 synthetic (tx-A) + 1 EVM (tx-B)")

	// Events are sorted by LogIndex ascending (Step 6). After sort:
	//   out[0..4]: tx-A's 5 EVM logs (Index 0..4)
	//   out[5]:    tx-B's 1 EVM log  (Index 5, slotted between A's EVM and synthetic)
	//   out[6..9]: tx-A's 4 synthetic transferLogs (Index 6..9, start = totalEvmLogs(6) + priorTxLogs(0))
	for i := 0; i < 5; i++ {
		r.EqualValues(int64(i), out[i].LogIndex, "tx-A EVM log %d", i)
	}
	r.EqualValues(5, out[5].LogIndex, "tx-B EVM log Index=5 sorted between A's EVM and synthetic logs")
	for i := 0; i < 4; i++ {
		r.EqualValues(int64(6+i), out[6+i].LogIndex, "tx-A synthetic transferLog %d should be 6..9 not 5..8", i)
	}

	// Verify the array is sorted by LogIndex (ascending).
	for i := 1; i < len(out); i++ {
		r.LessOrEqual(out[i-1].LogIndex, out[i].LogIndex, "events must be sorted by LogIndex")
	}
}

func TestExtractBindingByTxPos(t *testing.T) {
	r := require.New(t)
	// Two txs, replay events stamped with InTxLogIdx 0..N per tx.
	traces := []ptypes.Trace{
		{TxID: "tx-A", ID: "root-A", TraceAddress: []int64{}},
		{TxID: "tx-A", ID: "sub-A1", TraceAddress: []int64{0}},
		{TxID: "tx-B", ID: "root-B", TraceAddress: []int64{}},
	}
	errTraces := []ptypes.Trace{
		{TxID: "tx-C", ID: "rootErr-C", TraceAddress: []int64{}},
	}
	events := []ptypes.Event{
		{ParentTraceID: "root-A", InTxLogIdx: 0, Position: 3, ID: "ev-A0"},
		{ParentTraceID: "sub-A1", InTxLogIdx: 1, Position: 4, ID: "ev-A1"},
		{ParentTraceID: "root-B", InTxLogIdx: 0, Position: 0, ID: "ev-B0"},
	}
	errEvents := []ptypes.Event{
		{ParentTraceID: "rootErr-C", InTxLogIdx: 0, Position: 0, ID: "ev-C0"},
	}

	m := extractBindingByTxPos(events, errEvents, traces, errTraces)
	r.Len(m, 4)

	r.Equal("root-A", m[bindingKey{txID: "tx-A", inTxIdx: 0}].parentTraceID)
	r.EqualValues(3, m[bindingKey{txID: "tx-A", inTxIdx: 0}].position)
	r.Equal("ev-A0", m[bindingKey{txID: "tx-A", inTxIdx: 0}].id)

	r.Equal("sub-A1", m[bindingKey{txID: "tx-A", inTxIdx: 1}].parentTraceID)
	r.Equal("root-B", m[bindingKey{txID: "tx-B", inTxIdx: 0}].parentTraceID)
	r.Equal("rootErr-C", m[bindingKey{txID: "tx-C", inTxIdx: 0}].parentTraceID)
}

func TestExtractBindingByTxPos_OrphanEventSkipped(t *testing.T) {
	r := require.New(t)
	// Event references an unknown ParentTraceID -> we have no way to find its
	// tx, so it's skipped (canonical side will fall back to root).
	traces := []ptypes.Trace{
		{TxID: "tx-X", ID: "root-X", TraceAddress: []int64{}},
	}
	events := []ptypes.Event{
		{ParentTraceID: "root-X", InTxLogIdx: 0, ID: "kept"},
		{ParentTraceID: "unknown-frame", InTxLogIdx: 0, ID: "dropped"},
	}
	m := extractBindingByTxPos(events, nil, traces, nil)
	r.Len(m, 1)
	r.Equal("kept", m[bindingKey{txID: "tx-X", inTxIdx: 0}].id)
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
