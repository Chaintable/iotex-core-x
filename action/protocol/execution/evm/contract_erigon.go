package evm

import (
	"math/big"

	libcommon "github.com/erigontech/erigon-lib/common"
	erigonstate "github.com/erigontech/erigon/core/state"
	"github.com/holiman/uint256"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/db/trie"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-core/v2/state"
)

type contractErigon struct {
	*state.Account
	intra *erigonstate.IntraBlockState
	sr    protocol.StateReader
	addr  hash.Hash160
	// committed tracks slots modified during this action for pipeline state_diff collection.
	// Populated by SetState, consumed by StateDBAdapter.StateDiff().
	committed map[hash.Hash256]struct{}
	// dirtyCode is true if SetCode was called (new contract deployment).
	dirtyCode bool
	// code caches the bytecode set by SetCode in this action. Mirrors the
	// behavior of the regular *contract and serves as a backstop: GetCode
	// prefers this cache so state_diff capture does not depend on erigon
	// IntraBlockState correctly reflecting SetCode on every historical
	// height (a pre-Sumatra CREATE was observed losing code through the
	// intra-only read path).
	code []byte
}

func newContractErigon(addr hash.Hash160, account *state.Account, intra *erigonstate.IntraBlockState, sr protocol.StateReader) (Contract, error) {
	c := &contractErigon{
		Account:   account,
		intra:     intra,
		addr:      addr,
		sr:        sr,
		committed: make(map[hash.Hash256]struct{}),
	}
	return c, nil
}

func (c *contractErigon) GetCommittedState(key hash.Hash256) ([]byte, error) {
	k := libcommon.Hash(key)
	v := uint256.NewInt(0)
	c.intra.GetCommittedState(libcommon.Address(c.addr), &k, v)
	h := hash.BytesToHash256(v.Bytes())
	return h[:], nil
}

func (c *contractErigon) GetState(key hash.Hash256) ([]byte, error) {
	k := libcommon.Hash(key)
	v := uint256.NewInt(0)
	c.intra.GetState(libcommon.Address(c.addr), &k, v)
	h := hash.BytesToHash256(v.Bytes())
	return h[:], nil
}

func (c *contractErigon) SetState(key hash.Hash256, value []byte) error {
	k := libcommon.Hash(key)
	c.intra.SetState(libcommon.Address(c.addr), &k, *uint256.MustFromBig(big.NewInt(0).SetBytes(value)))
	c.committed[key] = struct{}{}
	return nil
}

func (c *contractErigon) GetCode() ([]byte, error) {
	// Prefer the locally-cached bytecode from SetCode over a re-read from
	// erigon IntraBlockState. The intra read has been observed to return
	// empty bytes on pre-Sumatra CREATE flows during trace_debankBlock
	// replay, even though intra.SetCode was called — evidently the dryrun
	// intra does not always reflect the in-action SetCode back through
	// GetCode. With the local cache, pipeline state_diff collection via
	// both StateDiff() and collectPreCommitDiff now sees the freshly-set
	// bytecode regardless of intra's read path.
	if c.dirtyCode && len(c.code) > 0 {
		return c.code, nil
	}
	return c.intra.GetCode(libcommon.Address(c.addr)), nil
}

func (c *contractErigon) SetCode(h hash.Hash256, code []byte) {
	log.S().Infof("[DEBANK_DBG_CREATE] contractErigon.SetCode addr=%x codeLen=%d hash=%x", c.addr[:], len(code), h[:8])
	// Mirror *contract.SetCode (contract.go:117-121): update the embedded
	// Account.CodeHash so SelfState-derived SlimAccount encoding gets the
	// real hash. Without this line, collector.Accounts[addr].CodeHash
	// ends up as the empty-code default and leafage stores the contract
	// as an EOA.
	c.Account.CodeHash = h[:]
	// Cache the code bytes locally so GetCode / collectPreCommitDiff can
	// return them without depending on erigon intra re-reads.
	c.code = append(c.code[:0], code...)
	c.intra.SetCode(libcommon.Address(c.addr), code)
	c.dirtyCode = true
}

func (c *contractErigon) SelfState() *state.Account {
	acc := &state.Account{}
	_, err := c.sr.State(acc, protocol.LegacyKeyOption(c.addr), protocol.ErigonStoreOnlyOption())
	if err != nil {
		log.S().Panicf("failed to load account %x: %v", c.addr, err)
	}
	// Overlay the locally-set CodeHash on top of the erigon read. For a
	// freshly-CREATEd contract the erigon store does not yet reflect the
	// in-action SetCode — without this overlay the SlimAccount returned
	// here has CodeHash = EmptyCodeHash, which survives through
	// collectAccountState / collectAccountDiffOnPut to state_diff.
	if c.dirtyCode && len(c.Account.CodeHash) > 0 {
		acc.CodeHash = append(acc.CodeHash[:0], c.Account.CodeHash...)
	}
	return acc
}

func (c *contractErigon) Commit() error {
	return nil
}

func (c *contractErigon) LoadRoot() error {
	return nil
}

// Iterator is only for debug
func (c *contractErigon) Iterator() (trie.Iterator, error) {
	return nil, errors.New("not supported")
}

func (c *contractErigon) Snapshot() Contract {
	// Copy committed entries so state_diff tracking survives revert/snapshot.
	committed := make(map[hash.Hash256]struct{}, len(c.committed))
	for k := range c.committed {
		committed[k] = struct{}{}
	}
	var code []byte
	if c.code != nil {
		code = append([]byte(nil), c.code...)
	}
	return &contractErigon{
		Account:   c.Account.Clone(),
		intra:     c.intra,
		addr:      c.addr,
		sr:        c.sr,
		committed: committed,
		dirtyCode: c.dirtyCode,
		code:      code,
	}
}
