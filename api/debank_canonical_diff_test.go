package api

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/state/factory/erigonstore"
)

// withTempCanonicalDB starts a fresh ErigonDB at a temp dir for canonical-diff tests.
func withTempCanonicalDB(t *testing.T) *erigonstore.ErigonDB {
	t.Helper()
	dir := t.TempDir()
	db := erigonstore.NewErigonDB(dir)
	require.NoError(t, db.Start(context.Background()))
	t.Cleanup(func() { db.Stop(context.Background()) })
	return db
}

func commitDiff(t *testing.T, db *erigonstore.ErigonDB, height uint64, accs []erigonstore.SyntheticAccountWrite, storages []erigonstore.SyntheticStorageWrite) {
	t.Helper()
	require.NoError(t, erigonstore.CommitSyntheticBlock(context.Background(), db, height, accs, storages))
}

// helper: open a CanonicalBlockReader and run the bucket builder
func runBuckets(t *testing.T, db *erigonstore.ErigonDB, height uint64) (newAccs int, deleted int, storageGroups int, codes int) {
	t.Helper()
	rd, err := db.NewCanonicalBlockReader(context.Background())
	require.NoError(t, err)
	defer rd.Close()
	d, err := buildCanonicalDiffBuckets(context.Background(), rd, height)
	require.NoError(t, err)
	return len(d.NewAccounts), len(d.DeletedAccounts), len(d.StorageDiff), len(d.NewCodes)
}

func TestBuildCanonical_NewEOA(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xaaaa000000000000000000000000000000000001")

	commitDiff(t, db, 1,
		[]erigonstore.SyntheticAccountWrite{{Addr: addr, Old: nil, New: erigonstore.MakeSyntheticAccount(100, 0, common.Hash{}, 0)}},
		nil,
	)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 1)
	r.NoError(err)
	r.Len(d.NewAccounts, 1)
	r.Empty(d.DeletedAccounts)
	r.Empty(d.StorageDiff)
	r.Empty(d.NewCodes)

	got := d.NewAccounts[0]
	r.Equal(crypto.Keccak256Hash(addr[:]), got.Address)
	r.EqualValues(100, got.Balance.Uint64())
	r.EqualValues(0, got.Nonce)
	r.Equal(common.Hash(emptyKeccak), got.CodeHash)
}

func TestBuildCanonical_ContractDeploy(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xbbbb000000000000000000000000000000000001")
	code := []byte{0x60, 0x80, 0x60, 0x40, 0x52, 0x60, 0x00, 0x80, 0xfd}
	codeHash := crypto.Keccak256Hash(code)
	slot0 := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000000")
	slot7 := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000007")

	commitDiff(t, db, 1,
		[]erigonstore.SyntheticAccountWrite{{
			Addr: addr,
			Old:  nil,
			New:  erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1),
			Code: code,
		}},
		[]erigonstore.SyntheticStorageWrite{
			{Addr: addr, Incarnation: 1, Slot: slot0, Old: uint256.NewInt(0), New: uint256.NewInt(0xcafe)},
			{Addr: addr, Incarnation: 1, Slot: slot7, Old: uint256.NewInt(0), New: uint256.NewInt(42)},
		},
	)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 1)
	r.NoError(err)

	r.Len(d.NewAccounts, 1)
	r.Equal(crypto.Keccak256Hash(addr[:]), d.NewAccounts[0].Address)
	r.Equal(codeHash, d.NewAccounts[0].CodeHash)
	r.EqualValues(1, d.NewAccounts[0].Nonce)

	r.Len(d.NewCodes, 1)
	r.Equal(codeHash, d.NewCodes[0].CodeHash)
	r.Equal(code, d.NewCodes[0].Code)

	r.Len(d.StorageDiff, 1, "one contract changed storage")
	r.Equal(crypto.Keccak256Hash(addr[:]), d.StorageDiff[0].Address)
	r.Len(d.StorageDiff[0].Values, 2)

	// Validate slot encoding: Index = keccak256(slot)
	indexes := map[common.Hash]uint64{}
	for _, v := range d.StorageDiff[0].Values {
		indexes[v.Index] = v.Value.Uint64()
	}
	r.EqualValues(0xcafe, indexes[crypto.Keccak256Hash(slot0[:])])
	r.EqualValues(42, indexes[crypto.Keccak256Hash(slot7[:])])
}

func TestBuildCanonical_Selfdestruct(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xcccc000000000000000000000000000000000001")
	code := []byte{0xfe}
	codeHash := crypto.Keccak256Hash(code)

	original := erigonstore.MakeSyntheticAccount(50, 1, codeHash, 1)
	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: original, Code: code},
	}, nil)
	commitDiff(t, db, 2, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: original, New: nil},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 2)
	r.NoError(err)
	r.Len(d.DeletedAccounts, 1)
	r.Equal(crypto.Keccak256Hash(addr[:]), d.DeletedAccounts[0])
	r.Empty(d.NewAccounts)
	r.Empty(d.NewCodes)
}

func TestBuildCanonical_StorageMutationNoNewCode(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xdddd000000000000000000000000000000000001")
	code := []byte{0xfe, 0xfe}
	codeHash := crypto.Keccak256Hash(code)
	slot := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000000005")

	// Block 1: deploy contract with init storage
	deployed := erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1)
	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: deployed, Code: code},
	}, []erigonstore.SyntheticStorageWrite{
		{Addr: addr, Incarnation: 1, Slot: slot, Old: uint256.NewInt(0), New: uint256.NewInt(1)},
	})

	// Block 2: modify storage only (codeHash unchanged, balance/nonce same)
	commitDiff(t, db, 2, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: deployed, New: deployed},
	}, []erigonstore.SyntheticStorageWrite{
		{Addr: addr, Incarnation: 1, Slot: slot, Old: uint256.NewInt(1), New: uint256.NewInt(2)},
	})

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 2)
	r.NoError(err)
	r.Empty(d.NewCodes, "no new code should be reported when codeHash unchanged across blocks")
	r.Len(d.StorageDiff, 1)
	r.Len(d.StorageDiff[0].Values, 1)
	r.EqualValues(2, d.StorageDiff[0].Values[0].Value.Uint64())
}

func TestBuildCanonical_EmptyBlock(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	commitDiff(t, db, 1, nil, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 1)
	r.NoError(err)
	r.NotNil(d)
	r.Empty(d.NewAccounts)
	r.Empty(d.DeletedAccounts)
	r.Empty(d.StorageDiff)
	r.Empty(d.NewCodes)
}

func TestBuildCanonical_DedupCodes(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	a1 := common.HexToAddress("0xeeee000000000000000000000000000000000001")
	a2 := common.HexToAddress("0xeeee000000000000000000000000000000000002")
	code := []byte{0xab}
	codeHash := crypto.Keccak256Hash(code)

	// Two contracts with bytecode-identical code in one block → expect 1 NewCode
	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{Addr: a1, Old: nil, New: erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1), Code: code},
		{Addr: a2, Old: nil, New: erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1), Code: code},
	}, nil)

	a, b, c, codes := runBuckets(t, db, 1)
	r.Equal(2, a, "two contract accounts")
	r.Equal(0, b)
	r.Equal(0, c)
	r.Equal(1, codes, "duplicate codeHash should be deduped")
}

func TestBuildCanonical_TipNotFinalized(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)

	commitDiff(t, db, 1, nil, nil)

	// Simulate tip < height+1 → caller side returns ErrCanonicalNotFinalized.
	// Direct buildCanonicalStateDiff requires *blockdao.BlockDAO; we exercise
	// the precondition only.
	_, _, err := buildCanonicalStateDiff(context.Background(), nil, db, 5, 5)
	r.Error(err)
	r.ErrorIs(err, ErrCanonicalNotFinalized)
}

// TestBuildCanonical_KvCodeMissing simulates a writer-data-integrity bug where
// AccountChangeSet records a contract deploy (account.CodeHash != empty) but the
// corresponding bytecode is absent from kv.Code. This SHOULD NEVER happen on a
// healthy archive (Erigon's UpdateAccountCode writes both atomically) — but if it
// does, buildCanonicalDiffBuckets must error out with ErrCanonicalCodeMissing
// rather than silently emit an empty NewCode entry.
//
// Construction: use SyntheticAccountWrite WITHOUT Code field. CommitSyntheticBlock
// will call UpdateAccountData (writes PlainState entry with CodeHash) but skip
// UpdateAccountCode, leaving kv.Code without the bytecode.
func TestBuildCanonical_KvCodeMissing(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xfeed000000000000000000000000000000000001")
	fakeCodeHash := common.HexToHash("0xdeadbeef00112233445566778899aabbccddeeff00112233445566778899aabb")

	// Account written with non-empty CodeHash but Code: nil — kv.Code stays empty
	// for this codeHash, while PlainState carries the dangling reference.
	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{
			Addr: addr,
			Old:  nil,
			New:  erigonstore.MakeSyntheticAccount(0, 1, fakeCodeHash, 1),
			Code: nil, // intentional: skip UpdateAccountCode → kv.Code missing this hash
		},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	// Sanity: the dangling reference is observable.
	acc, err := rd.AccountAt(1, addr)
	r.NoError(err)
	r.NotNil(acc)
	r.Equal(fakeCodeHash, common.Hash(acc.CodeHash), "account.CodeHash points at the missing hash")
	missing, err := rd.CodeByHash(fakeCodeHash)
	r.NoError(err)
	r.Nil(missing, "kv.Code has no bytecode for this hash")

	// Critical: builder must hard-error, NOT silently emit empty Code or skip the entry.
	_, err = buildCanonicalDiffBuckets(context.Background(), rd, 1)
	r.Error(err)
	r.ErrorIs(err, ErrCanonicalCodeMissing, "must surface ErrCanonicalCodeMissing sentinel")
	r.Contains(err.Error(), fakeCodeHash.Hex()[:18],
		"error message must reference the missing codeHash for ops triage")
	r.Contains(err.Error(), "height=1", "error message must reference the block height")
}

// TestBuildCanonical_KvCodePresent is the happy-path counterpoint: when kv.Code IS
// populated, buildCanonicalDiffBuckets should succeed and emit the bytecode in NewCodes.
// This ensures the missing-code check doesn't produce false positives on legitimate deploys.
func TestBuildCanonical_KvCodePresent(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xfeed000000000000000000000000000000000002")
	code := []byte{0x60, 0x80, 0x60, 0x40, 0x52} // synthetic bytecode prefix
	codeHash := crypto.Keccak256Hash(code)

	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{
			Addr: addr,
			Old:  nil,
			New:  erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1),
			Code: code, // kv.Code populated
		},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 1)
	r.NoError(err)
	r.Len(d.NewCodes, 1)
	r.Equal(codeHash, d.NewCodes[0].CodeHash)
	r.Equal(code, d.NewCodes[0].Code)
}

// TestBuildCanonical_CodeUnchangedNotFetched is a targeted regression: when an
// existing-and-still-deployed contract appears in AccountChangeSet (e.g., its balance
// or nonce changed but code did not), the builder must NOT trigger CodeByHash for
// that addr — otherwise a transient codeHash-without-Code on an unrelated codeHash
// could falsely fail. We simulate this by:
//   - block 1: deploy a contract WITH bytecode (kv.Code populated for codeHash A)
//   - block 2: modify same contract's account (e.g., simulated balance change) keeping
//             the same codeHash A. The diff at block 2 must succeed without re-fetching
//             code (which would still pass, but we verify NewCodes is empty for block 2).
func TestBuildCanonical_CodeUnchangedNotInNewCodes(t *testing.T) {
	r := require.New(t)
	db := withTempCanonicalDB(t)
	addr := common.HexToAddress("0xfeed000000000000000000000000000000000003")
	code := []byte{0xfe}
	codeHash := crypto.Keccak256Hash(code)

	deployed := erigonstore.MakeSyntheticAccount(0, 1, codeHash, 1)
	commitDiff(t, db, 1, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: nil, New: deployed, Code: code},
	}, nil)

	// Block 2: same codeHash, balance changed
	updated := erigonstore.MakeSyntheticAccount(100, 1, codeHash, 1)
	commitDiff(t, db, 2, []erigonstore.SyntheticAccountWrite{
		{Addr: addr, Old: deployed, New: updated},
	}, nil)

	rd, err := db.NewCanonicalBlockReader(context.Background())
	r.NoError(err)
	defer rd.Close()

	d, err := buildCanonicalDiffBuckets(context.Background(), rd, 2)
	r.NoError(err)
	r.Empty(d.NewCodes, "codeHash unchanged across blocks → no NewCode entry at block 2")
	r.Len(d.NewAccounts, 1, "balance change still produces NewAccount entry")
}
