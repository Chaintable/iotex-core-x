package api

import (
	"context"
	"math/big"

	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/iotexproject/iotex-proto/golang/iotexapi"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"

	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/action/protocol/rewarding"
	"github.com/iotexproject/iotex-core/v2/action/protocol/staking"
	"github.com/iotexproject/iotex-core/v2/blockchain"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-core/v2/state/factory"
)

// EVM-style protocol pool addresses.
//
// IoTeX-native `eth_getBalance(addr, h)` special-cases these two addresses (see
// coreservice.go:getProtocolAccount + web3 routing), unconditionally routing them to
// `staking.ReadState(TOTAL_STAKING_AMOUNT)` and `rewarding.TotalBalance` respectively.
// They are derived as `keccak256(<name>)[12:]` — the standard EVM convention for deriving
// a 20-byte address from an arbitrary string. NOT to be confused with `hash160(<name>)`
// (= `address.RewardingProtocolAddrHash` / `address.StakingProtocolAddrHash`), which are
// the iotex-native protocol IDs but return 0 from eth_getBalance.
var (
	rewardingPoolEthAddr = common.BytesToAddress(crypto.Keccak256(nil)) // placeholder; overwritten in init
	stakingPoolEthAddr   = common.BytesToAddress(crypto.Keccak256(nil))
)

func init() {
	r := crypto.Keccak256([]byte("rewarding"))
	s := crypto.Keccak256([]byte("staking"))
	copy(rewardingPoolEthAddr[:], r[12:]) // 0xa576c141e5659137ddda4223d209d4744b2106be
	copy(stakingPoolEthAddr[:], s[12:])   // 0x04c22afae6a03438b8fed74cb1cf441168df3f12
}

// appendProtocolPoolSyntheticAccounts appends rewarding-pool and staking-pool synthetic
// account entries to the canonical state_diff's NewAccounts list, preserving parity
// with native iotex `eth_getBalance(<pool>, h)` semantics (always returns LATEST value
// regardless of h).
//
// Pool balances live in custom protocol namespaces (`RewardingNamespace`,
// `StakingNamespace`) that bypass kv.PlainState entirely. The canonical reader cannot
// see them. We synthesize them here at TIP context and override any pre-existing
// entry for the same addr_hash (relevant for rewarding: GrantReward writes
// `0xa576c141...` to PlainState every block via accountstorage.Store, so canonical
// already produces a historical-accurate entry — we override with tip-snapshot to
// match native eth_getBalance which is tip-only).
//
// Failures (panic, missing protocol, pre-Greenland init issues) are recovered and
// logged. They leave the pool entry absent from this block, which degrades to
// "leafage shows older value at this height" rather than failing the whole RPC.
func appendProtocolPoolSyntheticAccounts(
	ctx context.Context,
	bc blockchain.Blockchain,
	registry *protocol.Registry,
	sf factory.Factory,
	diff *ptypes.BlockStorageDiff,
) {
	if diff == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.L().Warn("recovered from panic in appendProtocolPoolSyntheticAccounts", zap.Any("panic", r))
		}
	}()

	// Build a tip-context: matches legacy semantics where pool balance is always
	// read at TipHeight with current FeatureCtx (rewarding fund migrated v1→v2
	// at Greenland; per-block ctx with archived FeatureCtx would look in v1, now empty).
	poolCtx, err := bc.Context(context.Background())
	if err != nil {
		log.L().Debug("failed to build pool ctx", zap.Error(err))
		return
	}
	tipHeight := bc.TipHeight()
	poolCtx = protocol.WithBlockCtx(poolCtx, protocol.BlockCtx{BlockHeight: tipHeight})
	poolCtx = protocol.WithRegistry(poolCtx, registry)
	poolCtx = protocol.WithFeatureCtx(protocol.WithFeatureWithHeightCtx(poolCtx))

	// Rewarding pool — emitted at 0xa576c141... (keccak256("rewarding")[12:]).
	if balance := readRewardingPoolBalance(poolCtx, registry, sf); balance != nil {
		entry := makeSyntheticAccountEntry(rewardingPoolEthAddr, balance)
		upsertNewAccount(diff, entry)
	}

	// Staking pool — emitted at 0x04c22afa... (keccak256("staking")[12:]).
	if balance := readStakingPoolBalance(poolCtx, registry, sf); balance != nil {
		entry := makeSyntheticAccountEntry(stakingPoolEthAddr, balance)
		upsertNewAccount(diff, entry)
	}
}

// upsertNewAccount appends `entry` to diff.NewAccounts, replacing any pre-existing
// entry for the same Address (addr_hash). Also scrubs the addr from DeletedAccounts.
//
// This handles the rewarding-pool case where canonical reader already produced an entry
// (because GrantReward writes 0xa576c141 to PlainState every block) — we want the tip-
// snapshot synthetic value to override the historical-accurate canonical value, since
// the goal is parity with native eth_getBalance (tip-only).
func upsertNewAccount(diff *ptypes.BlockStorageDiff, entry ptypes.NewAccount) {
	for i, e := range diff.NewAccounts {
		if e.Address == entry.Address {
			diff.NewAccounts[i] = entry
			removeFromDeleted(diff, entry.Address)
			return
		}
	}
	diff.NewAccounts = append(diff.NewAccounts, entry)
	removeFromDeleted(diff, entry.Address)
}

// readRewardingPoolBalance returns the rewarding pool's TotalBalance at the given (tip)
// context, or nil if the protocol is unavailable / errored / panicked.
func readRewardingPoolBalance(ctx context.Context, registry *protocol.Registry, sr protocol.StateReader) (bal *big.Int) {
	defer func() {
		if r := recover(); r != nil {
			log.L().Info("rewarding TotalBalance panicked", zap.Any("panic", r))
			bal = nil
		}
	}()
	p, ok := registry.Find("rewarding")
	if !ok {
		return nil
	}
	rp, ok := p.(*rewarding.Protocol)
	if !ok {
		return nil
	}
	balance, _, err := rp.TotalBalance(ctx, sr)
	if err != nil {
		log.L().Info("rewarding TotalBalance error", zap.Error(err))
		return nil
	}
	return balance
}

// readStakingPoolBalance returns the staking pool's total bucket amount at (tip) context.
func readStakingPoolBalance(ctx context.Context, registry *protocol.Registry, sr protocol.StateReader) (bal *big.Int) {
	defer func() {
		if r := recover(); r != nil {
			log.L().Info("staking readTotalAmount panicked", zap.Any("panic", r))
			bal = nil
		}
	}()
	p, ok := registry.Find("staking")
	if !ok {
		return nil
	}
	sp, ok := p.(*staking.Protocol)
	if !ok {
		return nil
	}
	methodBytes, err := proto.Marshal(&iotexapi.ReadStakingDataMethod{
		Method: iotexapi.ReadStakingDataMethod_TOTAL_STAKING_AMOUNT,
	})
	if err != nil {
		return nil
	}
	argBytes, err := proto.Marshal(&iotexapi.ReadStakingDataRequest{
		Request: &iotexapi.ReadStakingDataRequest_TotalStakingAmount_{
			TotalStakingAmount: &iotexapi.ReadStakingDataRequest_TotalStakingAmount{},
		},
	})
	if err != nil {
		return nil
	}
	data, _, err := sp.ReadState(ctx, sr, methodBytes, argBytes)
	if err != nil {
		log.L().Info("staking ReadState error", zap.Error(err))
		return nil
	}
	var meta iotextypes.AccountMeta
	if err := proto.Unmarshal(data, &meta); err != nil {
		return nil
	}
	balance, ok := new(big.Int).SetString(meta.GetBalance(), 10)
	if !ok {
		log.L().Info("staking total amount unparsable", zap.String("raw", meta.GetBalance()))
		return nil
	}
	return balance
}

// makeSyntheticAccountEntry produces a NewAccount entry with addrhash=keccak256(addr),
// the given balance, nonce=0, codeHash=keccak256("") (EOA marker).
func makeSyntheticAccountEntry(ethAddr common.Address, balance *big.Int) ptypes.NewAccount {
	addrHash := crypto.Keccak256Hash(ethAddr[:])
	return ptypes.NewAccount{
		Address:  addrHash,
		Balance:  uint256.MustFromBig(balance),
		Nonce:    0,
		CodeHash: crypto.Keccak256Hash(nil),
	}
}

// removeFromDeleted scrubs `addrHash` from diff.DeletedAccounts (defensive — the
// canonical reader should never produce a deletion for protocol pool addrs since they're
// not in PlainState, but if it ever did, we'd want the synthetic to take precedence).
func removeFromDeleted(diff *ptypes.BlockStorageDiff, addrHash common.Hash) {
	if len(diff.DeletedAccounts) == 0 {
		return
	}
	out := diff.DeletedAccounts[:0]
	for _, h := range diff.DeletedAccounts {
		if h != addrHash {
			out = append(out, h)
		}
	}
	diff.DeletedAccounts = out
}
