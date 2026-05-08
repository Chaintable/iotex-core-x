package erigonstore

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"
)

// withTempErigonDB starts a fresh ErigonDB rooted at a temp dir; cleanup is automatic.
func withTempErigonDB(t *testing.T) *ErigonDB {
	t.Helper()
	dir := t.TempDir()
	db := NewErigonDB(dir)
	require.NoError(t, db.Start(context.Background()))
	t.Cleanup(func() { db.Stop(context.Background()) })
	return db
}

// commit is a thin wrapper around CommitSyntheticBlock that fails the test on error.
func commit(t *testing.T, db *ErigonDB, height uint64, accs []SyntheticAccountWrite, storages []SyntheticStorageWrite) {
	t.Helper()
	require.NoError(t, CommitSyntheticBlock(context.Background(), db, height, accs, storages))
}

func TestCanonicalReader_ChangedAccounts_EmptyBlock(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	commit(t, db, 1, nil, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	addrs, err := rd.ChangedAccounts(1)
	r.NoError(err)
	r.Empty(addrs)

	storages, err := rd.ChangedStorages(1)
	r.NoError(err)
	r.Empty(storages)
}

func TestCanonicalReader_ChangedAccounts_SingleEOA(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")

	commit(t, db, 1,
		[]SyntheticAccountWrite{{Addr: addr, Old: nil, New: MakeSyntheticAccount(100, 0, common.Hash{}, 0)}},
		nil,
	)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	addrs, err := rd.ChangedAccounts(1)
	r.NoError(err)
	r.Len(addrs, 1)
	r.Equal(addr, addrs[0])

	acc, err := rd.AccountAt(1, addr)
	r.NoError(err)
	r.NotNil(acc)
	r.EqualValues(100, acc.Balance.Uint64())
	r.EqualValues(0, acc.Nonce)
}

func TestCanonicalReader_AccountAt_OffByOne(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x2222222222222222222222222222222222222222")

	commit(t, db, 1, []SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: MakeSyntheticAccount(100, 0, common.Hash{}, 0)},
	}, nil)
	commit(t, db, 2, []SyntheticAccountWrite{
		{Addr: addr, Old: MakeSyntheticAccount(100, 0, common.Hash{}, 0), New: MakeSyntheticAccount(200, 0, common.Hash{}, 0)},
	}, nil)
	commit(t, db, 3, []SyntheticAccountWrite{
		{Addr: addr, Old: MakeSyntheticAccount(200, 0, common.Hash{}, 0), New: MakeSyntheticAccount(300, 0, common.Hash{}, 0)},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	acc1, err := rd.AccountAt(1, addr)
	r.NoError(err)
	r.NotNil(acc1)
	r.EqualValues(100, acc1.Balance.Uint64(), "after block 1")

	acc2, err := rd.AccountAt(2, addr)
	r.NoError(err)
	r.NotNil(acc2)
	r.EqualValues(200, acc2.Balance.Uint64(), "after block 2")

	acc3, err := rd.AccountAt(3, addr)
	r.NoError(err)
	r.NotNil(acc3)
	r.EqualValues(300, acc3.Balance.Uint64(), "after block 3")
}

func TestCanonicalReader_AccountAt_DeletedReturnsNil(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x3333333333333333333333333333333333333333")

	original := MakeSyntheticAccount(50, 0, common.Hash{}, 0)
	commit(t, db, 1, []SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: original},
	}, nil)
	commit(t, db, 2, []SyntheticAccountWrite{
		{Addr: addr, Old: original, New: nil},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	acc1, err := rd.AccountAt(1, addr)
	r.NoError(err)
	r.NotNil(acc1)
	r.EqualValues(50, acc1.Balance.Uint64())

	acc2, err := rd.AccountAt(2, addr)
	r.NoError(err)
	r.Nil(acc2, "should be nil after deletion")

	addrs, err := rd.ChangedAccounts(2)
	r.NoError(err)
	r.Contains(addrs, addr)
}

func TestCanonicalReader_StorageAt_BasicReadWrite(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x4444444444444444444444444444444444444444")
	slot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")

	contractAcc := MakeSyntheticAccount(0, 1, common.HexToHash("0xabcdef"), 1)
	commit(t, db, 1,
		[]SyntheticAccountWrite{{Addr: addr, Old: nil, New: contractAcc, Code: []byte{0xab, 0xcd, 0xef}}},
		[]SyntheticStorageWrite{
			{Addr: addr, Incarnation: 1, Slot: slot, Old: uint256.NewInt(0), New: uint256.NewInt(0xdead)},
		},
	)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	val, err := rd.StorageAt(1, addr, 1, slot)
	r.NoError(err)
	r.NotNil(val)
	got := uint256.NewInt(0).SetBytes(val).Uint64()
	r.EqualValues(0xdead, got)

	storages, err := rd.ChangedStorages(1)
	r.NoError(err)
	r.Len(storages, 1)
	r.Equal(addr, storages[0].Addr)
	r.EqualValues(1, storages[0].Incarnation)
	r.Equal(slot, storages[0].Slot)
}

func TestCanonicalReader_CodeByHash_Roundtrip(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x5555555555555555555555555555555555555555")
	code := []byte{0x60, 0x80, 0x60, 0x40, 0x52}
	codeHash := common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

	commit(t, db, 1,
		[]SyntheticAccountWrite{{Addr: addr, Old: nil, New: MakeSyntheticAccount(0, 1, codeHash, 1), Code: code}},
		nil,
	)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	got, err := rd.CodeByHash(codeHash)
	r.NoError(err)
	r.Equal(code, got)

	emptyResult, err := rd.CodeByHash(emptyCodeHashCommon)
	r.NoError(err)
	r.Nil(emptyResult)

	notFound, err := rd.CodeByHash(common.HexToHash("0xff00"))
	r.NoError(err)
	r.Nil(notFound)
}

func TestCanonicalReader_AvailableFrom(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	addr := common.HexToAddress("0x6666666666666666666666666666666666666666")

	commit(t, db, 5, []SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: MakeSyntheticAccount(1, 0, common.Hash{}, 0)},
	}, nil)
	commit(t, db, 7, []SyntheticAccountWrite{
		{Addr: addr, Old: MakeSyntheticAccount(1, 0, common.Hash{}, 0), New: MakeSyntheticAccount(2, 0, common.Hash{}, 0)},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	from, err := rd.AvailableFrom()
	r.NoError(err)
	r.EqualValues(5, from, "earliest changeset block")
}

func TestCanonicalReader_MultipleAccountsInOneBlock(t *testing.T) {
	r := require.New(t)
	db := withTempErigonDB(t)
	a1 := common.HexToAddress("0x7777777777777777777777777777777777777777")
	a2 := common.HexToAddress("0x8888888888888888888888888888888888888888")
	a3 := common.HexToAddress("0x9999999999999999999999999999999999999999")

	commit(t, db, 1, []SyntheticAccountWrite{
		{Addr: a1, Old: nil, New: MakeSyntheticAccount(10, 0, common.Hash{}, 0)},
		{Addr: a2, Old: nil, New: MakeSyntheticAccount(20, 1, common.Hash{}, 0)},
		{Addr: a3, Old: nil, New: MakeSyntheticAccount(30, 5, common.Hash{}, 0)},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	addrs, err := rd.ChangedAccounts(1)
	r.NoError(err)
	r.Len(addrs, 3)
	r.ElementsMatch([]common.Address{a1, a2, a3}, addrs)

	for _, want := range []struct {
		addr common.Address
		bal  uint64
		non  uint64
	}{
		{a1, 10, 0}, {a2, 20, 1}, {a3, 30, 5},
	} {
		acc, err := rd.AccountAt(1, want.addr)
		r.NoError(err)
		r.NotNil(acc)
		r.EqualValues(want.bal, acc.Balance.Uint64(), "addr %x balance", want.addr)
		r.EqualValues(want.non, acc.Nonce, "addr %x nonce", want.addr)
	}
}
