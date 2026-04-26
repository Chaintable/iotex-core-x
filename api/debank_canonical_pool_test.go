package api

import (
	"context"
	"math/big"
	"testing"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/action/protocol"
)

// Sanity: the keccak[12:]-derived pool addresses match what iotex-native eth_getBalance
// routes to pool ReadState. These are the EVM-style addresses that downstream leafage
// must reflect with tip-snapshot balance to preserve native parity.
//
// 0xa576c141e5659137ddda4223d209d4744b2106be == keccak256("rewarding")[12:]
// 0x04c22afae6a03438b8fed74cb1cf441168df3f12 == keccak256("staking")[12:]
func TestPoolEthAddrs(t *testing.T) {
	r := require.New(t)
	r.Equal(common.HexToAddress("0xa576c141e5659137ddda4223d209d4744b2106be"), rewardingPoolEthAddr)
	r.Equal(common.HexToAddress("0x04c22afae6a03438b8fed74cb1cf441168df3f12"), stakingPoolEthAddr)
}

func TestMakeSyntheticAccountEntry(t *testing.T) {
	r := require.New(t)
	balance := big.NewInt(123456789)

	entry := makeSyntheticAccountEntry(rewardingPoolEthAddr, balance)
	r.Equal(crypto.Keccak256Hash(rewardingPoolEthAddr[:]), entry.Address, "addr_hash = keccak256(20-byte addr)")
	r.Equal(uint256.MustFromBig(balance), entry.Balance)
	r.EqualValues(0, entry.Nonce, "synthetic account always has nonce=0")
	r.Equal(crypto.Keccak256Hash(nil), entry.CodeHash, "synthetic account always has empty codeHash")
}

func TestRemoveFromDeleted(t *testing.T) {
	r := require.New(t)
	a := common.HexToHash("0x01")
	b := common.HexToHash("0x02")
	c := common.HexToHash("0x03")

	d := &ptypes.BlockStorageDiff{}
	removeFromDeleted(d, a)
	r.Empty(d.DeletedAccounts)

	d.DeletedAccounts = []common.Hash{a, b, c}
	removeFromDeleted(d, b)
	r.Equal([]common.Hash{a, c}, d.DeletedAccounts)

	removeFromDeleted(d, common.HexToHash("0xff"))
	r.Equal([]common.Hash{a, c}, d.DeletedAccounts)
}

func TestUpsertNewAccount_AppendsWhenAbsent(t *testing.T) {
	r := require.New(t)
	d := &ptypes.BlockStorageDiff{}

	e1 := makeSyntheticAccountEntry(rewardingPoolEthAddr, big.NewInt(100))
	upsertNewAccount(d, e1)
	r.Len(d.NewAccounts, 1)
	r.Equal(e1.Address, d.NewAccounts[0].Address)
	r.EqualValues(100, d.NewAccounts[0].Balance.Uint64())

	e2 := makeSyntheticAccountEntry(stakingPoolEthAddr, big.NewInt(200))
	upsertNewAccount(d, e2)
	r.Len(d.NewAccounts, 2)
}

// Critical: upsert must REPLACE an existing entry with the same addr_hash. This is
// the rewarding-pool case where canonical reader produced an entry from GrantReward's
// accountstorage write, and the synthesis overrides it with tip-snapshot balance.
func TestUpsertNewAccount_OverridesWhenPresent(t *testing.T) {
	r := require.New(t)
	d := &ptypes.BlockStorageDiff{}

	// Pretend canonical reader already added a historical-accurate entry for rewarding
	historical := makeSyntheticAccountEntry(rewardingPoolEthAddr, big.NewInt(40))
	d.NewAccounts = append(d.NewAccounts, historical)

	// Synthesis injects tip-snapshot value
	tipSnapshot := makeSyntheticAccountEntry(rewardingPoolEthAddr, big.NewInt(50))
	upsertNewAccount(d, tipSnapshot)

	r.Len(d.NewAccounts, 1, "must NOT duplicate same-addr entries")
	r.EqualValues(50, d.NewAccounts[0].Balance.Uint64(), "tip-snapshot value wins")
}

func TestUpsertNewAccount_ScrubsDeletedAddr(t *testing.T) {
	r := require.New(t)
	rewardingHash := crypto.Keccak256Hash(rewardingPoolEthAddr[:])
	other := common.HexToHash("0xff")

	d := &ptypes.BlockStorageDiff{
		DeletedAccounts: []common.Hash{rewardingHash, other},
	}
	entry := makeSyntheticAccountEntry(rewardingPoolEthAddr, big.NewInt(1))
	upsertNewAccount(d, entry)

	r.Len(d.NewAccounts, 1)
	r.Equal([]common.Hash{other}, d.DeletedAccounts, "addr removed from DeletedAccounts")
}

func TestAppendProtocolPoolSyntheticAccounts_NilDiffIsSafe(t *testing.T) {
	appendProtocolPoolSyntheticAccounts(context.Background(), nil, nil, nil, nil)
	// Should not panic / error.
}

func TestAppendProtocolPoolSyntheticAccounts_NilBcRecovers(t *testing.T) {
	r := require.New(t)
	emptyReg := protocol.NewRegistry()
	d := &ptypes.BlockStorageDiff{NewAccounts: []ptypes.NewAccount{}}
	// Nil bc → method call panics on nil receiver → function recovers.
	appendProtocolPoolSyntheticAccounts(context.Background(), nil, emptyReg, nil, d)
	// Diff should not contain pool entries since nothing succeeded.
	rewardingHash := crypto.Keccak256Hash(rewardingPoolEthAddr[:])
	stakingHash := crypto.Keccak256Hash(stakingPoolEthAddr[:])
	for _, e := range d.NewAccounts {
		r.NotEqual(rewardingHash, e.Address)
		r.NotEqual(stakingHash, e.Address)
	}
}
