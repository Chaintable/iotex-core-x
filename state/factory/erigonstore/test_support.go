package erigonstore

// Test support helpers for cross-package tests of canonical state_diff & events.
//
// These functions write directly to Erigon's PlainState + ChangeSet via PlainStateWriter,
// bypassing iotex's commit pipeline. They exist as exported (non-`_test.go`) symbols so
// that tests in other packages (e.g. `api`) can construct synthetic ErigonDB state.
//
// Production code MUST NOT call these — but the package compiles them unconditionally
// because they have no testing.T dependency (errors returned instead of t.Fatal).

import (
	"context"

	erigonComm "github.com/erigontech/erigon-lib/common"
	erigonstate "github.com/erigontech/erigon/core/state"
	erigonAcc "github.com/erigontech/erigon/core/types/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
	"github.com/pkg/errors"
)

// SyntheticAccountWrite describes a synthetic account state transition for testing.
//
// Old=nil signals creating a fresh account (Erigon original.Initialised=false).
// New=nil signals selfdestruct/deletion. Code is the bytecode for new contracts.
type SyntheticAccountWrite struct {
	Addr common.Address
	Old  *erigonAcc.Account
	New  *erigonAcc.Account
	Code []byte
}

// SyntheticStorageWrite describes a synthetic storage slot mutation for testing.
type SyntheticStorageWrite struct {
	Addr        common.Address
	Incarnation uint64
	Slot        common.Hash
	Old         *uint256.Int
	New         *uint256.Int
}

// CommitSyntheticBlock writes one block's worth of state changes directly via Erigon's
// PlainStateWriter. After this returns successfully, kv.PlainState / kv.AccountChangeSet
// / kv.StorageChangeSet / kv.Code / kv.E2*History tables are populated as if the writer
// had committed a block at `height`.
func CommitSyntheticBlock(ctx context.Context, db *ErigonDB, height uint64, accs []SyntheticAccountWrite, storages []SyntheticStorageWrite) error {
	if db.rw == nil {
		return ErrErigonStoreClosed
	}
	tx, err := db.rw.BeginRw(ctx)
	if err != nil {
		return errors.Wrap(err, "begin rw tx")
	}
	defer tx.Rollback()

	psw := erigonstate.NewPlainStateWriter(tx, tx, height)

	for _, a := range accs {
		la := toLibAddr(a.Addr)
		oldAcc := a.Old
		if oldAcc == nil {
			oldAcc = &erigonAcc.Account{} // !Initialised → "did not exist" semantics
		}
		switch {
		case a.New == nil:
			if err := psw.DeleteAccount(la, oldAcc); err != nil {
				return errors.Wrapf(err, "DeleteAccount %s", a.Addr.Hex())
			}
		default:
			if err := psw.UpdateAccountData(la, oldAcc, a.New); err != nil {
				return errors.Wrapf(err, "UpdateAccountData %s", a.Addr.Hex())
			}
			if len(a.Code) > 0 {
				ch := erigonComm.Hash(a.New.CodeHash)
				if err := psw.UpdateAccountCode(la, a.New.Incarnation, ch, a.Code); err != nil {
					return errors.Wrapf(err, "UpdateAccountCode %s", a.Addr.Hex())
				}
			}
		}
	}

	for _, s := range storages {
		la := toLibAddr(s.Addr)
		ls := toLibHash(s.Slot)
		old := s.Old
		if old == nil {
			old = uint256.NewInt(0)
		}
		if err := psw.WriteAccountStorage(la, s.Incarnation, &ls, old, s.New); err != nil {
			return errors.Wrapf(err, "WriteAccountStorage %s slot=%s", s.Addr.Hex(), s.Slot.Hex())
		}
	}

	if err := psw.WriteChangeSets(); err != nil {
		return errors.Wrap(err, "WriteChangeSets")
	}
	if err := psw.WriteHistory(); err != nil {
		return errors.Wrap(err, "WriteHistory")
	}
	if err := tx.Commit(); err != nil {
		return errors.Wrap(err, "commit synthetic block tx")
	}
	return nil
}

// MakeSyntheticAccount builds a fully-initialized erigon Account suitable for use in
// SyntheticAccountWrite{Old, New}.
func MakeSyntheticAccount(balance uint64, nonce uint64, codeHash common.Hash, incarnation uint64) *erigonAcc.Account {
	a := &erigonAcc.Account{
		Initialised: true,
		Nonce:       nonce,
		Balance:     *uint256.NewInt(balance),
		Incarnation: incarnation,
	}
	if codeHash != (common.Hash{}) {
		a.CodeHash = erigonComm.Hash(codeHash)
	} else {
		a.CodeHash = erigonComm.Hash(emptyCodeHashCommon)
	}
	return a
}
