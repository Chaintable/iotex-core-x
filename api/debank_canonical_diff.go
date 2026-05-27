package api

import (
	"bytes"
	"context"
	"sort"
	"strings"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/v2/blockchain"
	"github.com/iotexproject/iotex-core/v2/blockchain/blockdao"
	"github.com/iotexproject/iotex-core/v2/state/factory/erigonstore"
)

// Sentinel errors raised by canonical state_diff path.
//
// Callers (RPC layer / ETL) inspect these via errors.Is to decide retry / skip behavior.
var (
	// ErrCanonicalNotFinalized indicates that block N+1's changeset has not been
	// committed yet, so canonical "as-of end of block N" reads are not available.
	// ETL should sleep and retry.
	ErrCanonicalNotFinalized = errors.New("block not finalized for canonical state_diff (need tip >= height+1)")

	// ErrHistoryUnavailable indicates the requested height is below the archive's
	// available history range (changesets pruned). Caller cannot recover.
	ErrHistoryUnavailable = errors.New("block height below archive history availability")

	// ErrCanonicalCodeMissing indicates that an account in NewAccounts has a non-empty
	// codeHash but the bytecode was not found in kv.Code. Indicates writer commit gap.
	ErrCanonicalCodeMissing = errors.New("canonical state_diff missing bytecode in kv.Code")
)

// buildCanonicalStateDiff constructs the full BlockStorageDiff for `height` from
// Erigon's canonical changesets and post-block plain state.
//
// Returns:
//   - diff: the BlockStorageDiff with all 5 buckets + Hash + ParentHash populated.
//   - storageContracts: lowercase 0x-prefixed addresses of contracts whose storage
//     changed at this block (from kv.StorageChangeSet). Used by debankBlockImpl to
//     populate BlockFile.StorageContracts canonically.
//
// Preconditions enforced:
//   - height > 0  (genesis is special-cased by caller)
//   - tipHeight >= height + 1  (else ErrCanonicalNotFinalized)
//   - erigon AvailableFrom <= height  (else ErrHistoryUnavailable)
//
// Output schema matches what leafage consumes today (pipeline/types BlockStorageDiff).
// Replay path is NOT consulted at any point — this is pure read.
func buildCanonicalStateDiff(
	ctx context.Context,
	dao blockdao.BlockDAO,
	erigonDB *erigonstore.ErigonDB,
	height uint64,
	tipHeight uint64,
) (*ptypes.BlockStorageDiff, []string, error) {
	if height == 0 {
		return nil, nil, errors.New("buildCanonicalStateDiff: height=0 should be handled via genesis path")
	}
	if tipHeight < height+1 {
		return nil, nil, errors.Wrapf(ErrCanonicalNotFinalized, "height=%d tip=%d", height, tipHeight)
	}

	rd, err := erigonDB.NewCanonicalBlockReader(ctx)
	if err != nil {
		return nil, nil, errors.Wrap(err, "open canonical reader")
	}
	defer rd.Close()

	availFrom, err := rd.AvailableFrom()
	if err != nil {
		return nil, nil, errors.Wrap(err, "query AvailableFrom")
	}
	if height < availFrom {
		return nil, nil, errors.Wrapf(ErrHistoryUnavailable, "height=%d available_from=%d", height, availFrom)
	}

	diff, err := buildCanonicalDiffBuckets(ctx, rd, height)
	if err != nil {
		return nil, nil, err
	}

	// Storage contracts (raw 0x-prefixed lowercase addrs); same set as ChangedStorages.
	storageContracts, err := canonicalStorageContracts(ctx, rd, height)
	if err != nil {
		return nil, nil, errors.Wrap(err, "canonicalStorageContracts")
	}

	// Block hash chaining: Hash = blk(N).DeltaStateDigest, ParentHash = blk(N-1).DeltaStateDigest.
	// For block 1, parent root falls back to blockchain.GenesisStateRoot (matches existing
	// debankBlockImpl convention at coreservice.go).
	blk, err := dao.GetBlockByHeight(height)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "GetBlockByHeight(%d)", height)
	}
	d := blk.DeltaStateDigest()
	diff.Hash = common.BytesToHash(d[:])
	if height == 1 {
		diff.ParentHash = blockchain.GenesisStateRoot
	} else {
		parent, err := dao.GetBlockByHeight(height - 1)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "GetBlockByHeight(%d) for parent root", height-1)
		}
		pd := parent.DeltaStateDigest()
		diff.ParentHash = common.BytesToHash(pd[:])
	}

	return diff, storageContracts, nil
}

// canonicalStorageContracts returns the deduplicated, lowercased, 0x-prefixed list of
// contract addresses whose storage changed at `height`. This is the canonical equivalent
// of replay's BlockFile.StorageContracts field.
func canonicalStorageContracts(ctx context.Context, rd *erigonstore.CanonicalBlockReader, height uint64) ([]string, error) {
	storages, err := rd.ChangedStorages(height)
	if err != nil {
		return nil, err
	}
	seen := make(map[common.Address]struct{})
	out := make([]string, 0, len(storages))
	for _, sk := range storages {
		if _, ok := seen[sk.Addr]; ok {
			continue
		}
		seen[sk.Addr] = struct{}{}
		out = append(out, strings.ToLower(sk.Addr.Hex()))
	}
	return out, nil
}

// buildCanonicalDiffBuckets is the pure data-mapping core: given a CanonicalBlockReader
// already opened at (or below) height, produce the four content buckets (NewAccounts,
// DeletedAccounts, StorageDiff, NewCodes). Hash / ParentHash are NOT set here.
//
// Split out from buildCanonicalStateDiff so the bucket logic is unit-testable without
// needing a real blockdao.
func buildCanonicalDiffBuckets(
	ctx context.Context,
	rd *erigonstore.CanonicalBlockReader,
	height uint64,
) (*ptypes.BlockStorageDiff, error) {
	out := &ptypes.BlockStorageDiff{
		NewAccounts:     []ptypes.NewAccount{},
		DeletedAccounts: []common.Hash{},
		StorageDiff:     []ptypes.AccountStorageDiff{},
		NewCodes:        []ptypes.NewCode{},
	}

	changedAccs, err := rd.ChangedAccounts(height)
	if err != nil {
		return nil, errors.Wrap(err, "ChangedAccounts")
	}
	changedStorages, err := rd.ChangedStorages(height)
	if err != nil {
		return nil, errors.Wrap(err, "ChangedStorages")
	}

	// Track distinct codeHashes that need to be fetched (avoids duplicate kv.Code lookups
	// when multiple addresses deploy bytecode-identical contracts in the same block).
	newCodeHashes := make(map[common.Hash]struct{})

	// ─── Accounts → NewAccounts / DeletedAccounts ────────────────────────────
	for _, addr := range changedAccs {
		addrHash := crypto.Keccak256Hash(addr[:])
		acc, err := rd.AccountAt(height, addr)
		if err != nil {
			return nil, errors.Wrapf(err, "AccountAt(%d, %s)", height, addr.Hex())
		}
		if acc == nil {
			// Selfdestructed at this block; addr no longer in PlainState as-of N+1.
			out.DeletedAccounts = append(out.DeletedAccounts, addrHash)
			continue
		}
		codeHash := common.Hash(acc.CodeHash)
		out.NewAccounts = append(out.NewAccounts, ptypes.NewAccount{
			Address:  addrHash,
			Balance:  &acc.Balance,
			Nonce:    acc.Nonce,
			CodeHash: codeHash,
		})

		// Detect newly-introduced bytecode for this addr at this block.
		// Two signals together: codeHash is non-empty, AND prior block did not have
		// the same codeHash for this addr. For brand-new contracts, prior account is nil.
		if !isEmptyCodeHash(codeHash) {
			prior, err := rd.AccountAt(height-1, addr)
			if err != nil {
				return nil, errors.Wrapf(err, "AccountAt(%d, %s) for prior code check", height-1, addr.Hex())
			}
			priorCodeHash := common.Hash{}
			if prior != nil {
				priorCodeHash = common.Hash(prior.CodeHash)
			}
			if priorCodeHash != codeHash {
				newCodeHashes[codeHash] = struct{}{}
			}
		}
	}

	// ─── Storages → StorageDiff ─────────────────────────────────────────────
	// Group ChangedStorages by addr. Each post-block slot value goes through
	// uint256.SetBytes (handles Erigon's stripped-zero encoding).
	type slotEntry struct {
		slot  common.Hash
		value *uint256.Int
	}
	storageByAddr := make(map[common.Address][]slotEntry)
	for _, sk := range changedStorages {
		val, err := rd.StorageAt(height, sk.Addr, sk.Incarnation, sk.Slot)
		if err != nil {
			return nil, errors.Wrapf(err, "StorageAt(%d, %s, slot=%s)", height, sk.Addr.Hex(), sk.Slot.Hex())
		}
		v := uint256.NewInt(0)
		if len(val) > 0 {
			v.SetBytes(val)
		}
		storageByAddr[sk.Addr] = append(storageByAddr[sk.Addr], slotEntry{slot: sk.Slot, value: v})
	}
	// Iterate storageByAddr in deterministic order. Go map iteration is
	// unspecified; relying on it here makes StorageDiff entry order vary
	// between writer binaries even when the underlying chain state is
	// identical, which breaks byte-level state_diff RLP reconciliation.
	sortedAddrs := make([]common.Address, 0, len(storageByAddr))
	for addr := range storageByAddr {
		sortedAddrs = append(sortedAddrs, addr)
	}
	sort.Slice(sortedAddrs, func(i, j int) bool {
		return bytes.Compare(sortedAddrs[i][:], sortedAddrs[j][:]) < 0
	})
	for _, addr := range sortedAddrs {
		entries := storageByAddr[addr]
		addrHash := crypto.Keccak256Hash(addr[:])
		values := make([]ptypes.IndexValuePair, 0, len(entries))
		for _, e := range entries {
			values = append(values, ptypes.IndexValuePair{
				Index: crypto.Keccak256Hash(e.slot[:]),
				Value: e.value,
			})
		}
		out.StorageDiff = append(out.StorageDiff, ptypes.AccountStorageDiff{
			Address: addrHash,
			Values:  values,
		})
	}

	// ─── Codes → NewCodes ────────────────────────────────────────────────────
	// Same deterministic-order requirement as StorageDiff above.
	sortedCodeHashes := make([]common.Hash, 0, len(newCodeHashes))
	for codeHash := range newCodeHashes {
		sortedCodeHashes = append(sortedCodeHashes, codeHash)
	}
	sort.Slice(sortedCodeHashes, func(i, j int) bool {
		return bytes.Compare(sortedCodeHashes[i][:], sortedCodeHashes[j][:]) < 0
	})
	for _, codeHash := range sortedCodeHashes {
		code, err := rd.CodeByHash(codeHash)
		if err != nil {
			return nil, errors.Wrapf(err, "CodeByHash(%s)", codeHash.Hex())
		}
		if len(code) == 0 {
			return nil, errors.Wrapf(ErrCanonicalCodeMissing, "codeHash=%s referenced at height=%d", codeHash.Hex(), height)
		}
		out.NewCodes = append(out.NewCodes, ptypes.NewCode{
			CodeHash: codeHash,
			Code:     code,
		})
	}

	return out, nil
}

// emptyKeccak is keccak256("") — Erigon EOA marker.
var emptyKeccak = crypto.Keccak256Hash(nil)

func isEmptyCodeHash(h common.Hash) bool {
	return h == (common.Hash{}) || h == emptyKeccak
}
