package erigonstore

import (
	"bytes"
	"context"
	"sort"

	erigonChangeset "github.com/erigontech/erigon/common/changeset"
	erigonComm "github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/length"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/kv/temporal/historyv2"
	erigonstate "github.com/erigontech/erigon/core/state"
	erigonAcc "github.com/erigontech/erigon/core/types/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/pkg/errors"
)

// StorageKey identifies a specific storage slot on a specific incarnation of a contract.
//
// Erigon's storage is keyed by (addr, incarnation, slot) so that selfdestruct + redeploy
// at the same address gets a fresh storage tree. For most contracts incarnation == 1.
type StorageKey struct {
	Addr        common.Address
	Incarnation uint64
	Slot        common.Hash
}

// CanonicalBlockReader provides read-only access to Erigon's canonical state at any height.
//
// All methods share a single read-only mdbx tx. Caller MUST call Close when done.
//
// Height semantics:
//   - height N in input means "state at end of block N" (i.e., after block N's actions are applied)
//   - Internally maps to Erigon's NewPlainState(tx, N+1) per Erigon's as-of convention
//     (see erigon/core/state/plain_readonly.go)
type CanonicalBlockReader struct {
	db    *ErigonDB
	tx    kv.Tx
	cache map[uint64]*erigonstate.PlainState
}

// NewCanonicalBlockReader opens a read-only transaction and returns a reader for canonical state queries.
//
// The reader is single-threaded. Caller MUST call Close exactly once.
func (db *ErigonDB) NewCanonicalBlockReader(ctx context.Context) (*CanonicalBlockReader, error) {
	if db.rw == nil {
		return nil, ErrErigonStoreClosed
	}
	tx, err := db.rw.BeginRo(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "begin canonical reader ro tx")
	}
	return &CanonicalBlockReader{
		db:    db,
		tx:    tx,
		cache: make(map[uint64]*erigonstate.PlainState),
	}, nil
}

// Close releases the underlying read-only transaction. Safe to call multiple times.
func (r *CanonicalBlockReader) Close() {
	if r.tx != nil {
		r.tx.Rollback()
		r.tx = nil
	}
}

// plainStateAt returns the PlainState reader rooted "as-of" the given Erigon block number.
//
// IMPORTANT: caller passes the *Erigon* blockNr (= N+1 to mean "end of iotex block N").
// This method does NOT add 1 — that's done at the public API layer.
func (r *CanonicalBlockReader) plainStateAt(erigonBlockNr uint64) *erigonstate.PlainState {
	if ps, ok := r.cache[erigonBlockNr]; ok {
		return ps
	}
	ps := erigonstate.NewPlainState(r.tx, erigonBlockNr, nil)
	r.cache[erigonBlockNr] = ps
	return ps
}

// ChangedAccounts returns the set of account addresses whose state was modified during block N.
//
// This is read directly from kv.AccountChangeSet[N]. The set may be empty for blocks that
// only contain non-state-mutating actions.
func (r *CanonicalBlockReader) ChangedAccounts(height uint64) ([]common.Address, error) {
	seen := make(map[common.Address]struct{})
	if err := erigonChangeset.ForRange(r.tx, kv.AccountChangeSet, height, height+1,
		func(blockN uint64, k, v []byte) error {
			if len(k) != length.Addr {
				return errors.Errorf("unexpected account changeset key length %d (expect %d) at block %d",
					len(k), length.Addr, blockN)
			}
			seen[common.BytesToAddress(k)] = struct{}{}
			return nil
		}); err != nil {
		return nil, errors.Wrapf(err, "iterate AccountChangeSet at height %d", height)
	}
	out := make([]common.Address, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	// Map iteration order in Go is unspecified; downstream consumers
	// (buildCanonicalDiffBuckets) append entries to BlockStorageDiff in the
	// returned order, and the resulting RLP encoding is order-sensitive. Two
	// writers running the same build but different binary instances will
	// otherwise produce byte-divergent state_diff hex for identical chain
	// state. Sort by raw address bytes to make the output deterministic.
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i][:], out[j][:]) < 0
	})
	return out, nil
}

// ChangedStorages returns the set of (addr, incarnation, slot) tuples whose storage value
// was modified during block N.
//
// This is read directly from kv.StorageChangeSet[N]. After historyv2.FromDBFormat decoding,
// the key is layout addr(20) || incarnation(8) || slot(32) = 60 bytes.
func (r *CanonicalBlockReader) ChangedStorages(height uint64) ([]StorageKey, error) {
	const wantKeyLen = length.Addr + length.Incarnation + length.Hash // 20+8+32 = 60
	seen := make(map[StorageKey]struct{})
	if err := erigonChangeset.ForRange(r.tx, kv.StorageChangeSet, height, height+1,
		func(blockN uint64, k, v []byte) error {
			if len(k) != wantKeyLen {
				return errors.Errorf("unexpected storage changeset key length %d (expect %d) at block %d",
					len(k), wantKeyLen, blockN)
			}
			var sk StorageKey
			copy(sk.Addr[:], k[:length.Addr])
			sk.Incarnation = decUint64BE(k[length.Addr : length.Addr+length.Incarnation])
			copy(sk.Slot[:], k[length.Addr+length.Incarnation:])
			seen[sk] = struct{}{}
			return nil
		}); err != nil {
		return nil, errors.Wrapf(err, "iterate StorageChangeSet at height %d", height)
	}
	out := make([]StorageKey, 0, len(seen))
	for sk := range seen {
		out = append(out, sk)
	}
	// See ChangedAccounts for the rationale on deterministic ordering. Sort key
	// is (Addr, Incarnation, Slot) — Addr first so all slots of a given
	// contract group together (matches downstream storageByAddr grouping in
	// buildCanonicalDiffBuckets), Slot last as the per-contract tiebreak.
	sort.Slice(out, func(i, j int) bool {
		if c := bytes.Compare(out[i].Addr[:], out[j].Addr[:]); c != 0 {
			return c < 0
		}
		if out[i].Incarnation != out[j].Incarnation {
			return out[i].Incarnation < out[j].Incarnation
		}
		return bytes.Compare(out[i].Slot[:], out[j].Slot[:]) < 0
	})
	return out, nil
}

// AccountAt returns the account state at the END of block `height`.
//
// Returns (nil, nil) if the address has no entry in PlainState as-of height+1
// (which means: either never existed up to that height, or was deleted/selfdestructed
// at exactly this block). Caller distinguishes "deleted at this block" from
// "never existed" by also checking ChangedAccounts(height).
func (r *CanonicalBlockReader) AccountAt(height uint64, addr common.Address) (*erigonAcc.Account, error) {
	return r.plainStateAt(height + 1).ReadAccountData(toLibAddr(addr))
}

// StorageAt returns the storage value at the END of block `height` for (addr, incarnation, slot).
//
// Returns nil if the slot has zero value or no record (Erigon does not distinguish).
// The returned bytes are the raw stored bytes (zero-prefix stripped); caller does
// 32-byte left-padding if a fixed-width representation is needed.
func (r *CanonicalBlockReader) StorageAt(height uint64, addr common.Address, incarnation uint64, slot common.Hash) ([]byte, error) {
	libAddr := toLibAddr(addr)
	libSlot := toLibHash(slot)
	return r.plainStateAt(height + 1).ReadAccountStorage(libAddr, incarnation, &libSlot)
}

// CodeByHash returns the bytecode stored at the given code hash, or nil if not present.
//
// kv.Code is keyed by keccak256(code). Empty code hash returns nil with no error.
func (r *CanonicalBlockReader) CodeByHash(codeHash common.Hash) ([]byte, error) {
	if codeHash == (common.Hash{}) || codeHash == emptyCodeHashCommon {
		return nil, nil
	}
	code, err := r.tx.GetOne(kv.Code, codeHash[:])
	if err != nil {
		return nil, errors.Wrapf(err, "lookup code by hash %x", codeHash)
	}
	if len(code) == 0 {
		return nil, nil
	}
	return code, nil
}

// AvailableFrom returns the earliest block number for which AccountChangeSet has entries.
//
// On a full archive this is typically 0 or 1. On a pruned node, queries below this height
// will fall back to current PlainState (i.e., return latest values, not historical).
func (r *CanonicalBlockReader) AvailableFrom() (uint64, error) {
	return historyv2.AvailableFrom(r.tx)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// emptyCodeHashCommon is keccak256("") cast to go-ethereum common.Hash.
// Matches Erigon's accounts.Account.IsEmptyCodeHash() check via fixed value.
var emptyCodeHashCommon = common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")

func toLibAddr(a common.Address) erigonComm.Address {
	var out erigonComm.Address
	copy(out[:], a[:])
	return out
}

func toLibHash(h common.Hash) erigonComm.Hash {
	var out erigonComm.Hash
	copy(out[:], h[:])
	return out
}

func decUint64BE(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
