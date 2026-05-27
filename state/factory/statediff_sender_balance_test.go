// Copyright (c) 2026 IoTeX Foundation
// Licensed under Apache License 2.0.

package factory

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/stretchr/testify/require"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/action/protocol/account"
	accountutil "github.com/iotexproject/iotex-core/v2/action/protocol/account/util"
	"github.com/iotexproject/iotex-core/v2/action/protocol/execution"
	"github.com/iotexproject/iotex-core/v2/action/protocol/execution/evm"
	"github.com/iotexproject/iotex-core/v2/action/protocol/rewarding"
	"github.com/iotexproject/iotex-core/v2/action/protocol/rolldpos"
	"github.com/iotexproject/iotex-core/v2/blockchain"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
	"github.com/iotexproject/iotex-core/v2/db"
	"github.com/iotexproject/iotex-core/v2/test/identityset"
)

// TestStateDiffCollectorSenderBalanceBug reproduces the ~0.126 IOTX / tx
// discrepancy observed in trace_debankBlock's state_diff on IoTeX mainnet
// where sender balance captured by PipelineStateDiffCollector does not match
// the true post-block balance in the working set.
//
// Hypothesis: collector.Accounts[sender] is overwritten at a point where the
// in-memory sm snapshot does not reflect the full gas-fee deduction
// (base_fee + priority_fee). See conversation for full analysis.
//
// Expected (if no bug): collector.Accounts[sender].Balance == ws sender balance
func TestStateDiffCollectorSenderBalanceBug(t *testing.T) {
	require := require.New(t)

	ctx := context.Background()

	// Enable FixDoubleChargeGas (Pacific) and EnableDynamicFeeTx (Vanuatu) from block 0.
	g := genesis.TestDefault()
	// Activate all forks from genesis so feature flags line up.
	g.AleutianBlockHeight = 0
	g.BeringBlockHeight = 0
	g.CookBlockHeight = 0
	g.DardanellesBlockHeight = 0
	g.DaytonaBlockHeight = 0
	g.EasterBlockHeight = 0
	g.FairbankBlockHeight = 0
	g.GreenlandBlockHeight = 0
	g.HawaiiBlockHeight = 0
	g.IcelandBlockHeight = 0
	g.JutlandBlockHeight = 0
	g.KamchatkaBlockHeight = 0
	g.MidwayBlockHeight = 0
	g.NewfoundlandBlockHeight = 0
	g.OkhotskBlockHeight = 0
	g.PalauBlockHeight = 0
	g.QuebecBlockHeight = 0
	g.RedseaBlockHeight = 0
	g.SumatraBlockHeight = 0
	g.TsunamiBlockHeight = 0
	g.UpernavikBlockHeight = 0
	g.VanuatuBlockHeight = 0
	g.WakeBlockHeight = 0
	g.XinguBlockHeight = 0
	g.BlockGasLimit = 100_000_000

	// Protocols.
	registry := protocol.NewRegistry()
	require.NoError(account.NewProtocol(rewarding.DepositGas).Register(registry))
	require.NoError(rolldpos.NewProtocol(g.NumCandidateDelegates, g.NumDelegates, g.NumSubEpochs).Register(registry))
	getBlockHash := func(uint64) (hash.Hash256, error) { return hash.ZeroHash256, nil }
	getBlockTime := func(uint64) (time.Time, error) { return time.Time{}, nil }
	// v2.4.1 added IsBlackListedFunc 4th param; tests don't blacklist anyone.
	require.NoError(execution.NewProtocol(getBlockHash, rewarding.DepositGas, getBlockTime, nil).Register(registry))
	require.NoError(rewarding.NewProtocol(g.Rewarding).Register(registry))

	chainCfg := blockchain.DefaultConfig
	factoryCfg := GenerateConfig(chainCfg, g)
	sf, err := NewStateDB(factoryCfg, db.NewMemKVStore(), RegistryStateDBOption(registry))
	require.NoError(err)

	startCtx := genesis.WithGenesisContext(ctx, g)
	startCtx = protocol.WithBlockchainCtx(startCtx, protocol.BlockchainCtx{
		ChainID:      chainCfg.ID,
		EvmNetworkID: chainCfg.EVMNetworkID,
	})
	require.NoError(sf.Start(startCtx))
	defer sf.Stop(startCtx)

	sender := identityset.Address(0)
	senderKey := identityset.PrivateKey(0)

	preBal, err := accountutil.AccountState(startCtx, sf, sender)
	require.NoError(err)
	t.Logf("pre balance of sender=%s  balance=%s", sender, preBal.Balance)

	// tx: plain value=0 call to identityset.Address(1) (EOA), no calldata.
	baseFee := big.NewInt(1_000_000_000_000) // 10^12
	priority := int64(1)
	gasPrice := new(big.Int).Add(baseFee, big.NewInt(priority)) // 10^12 + 1
	gasLimit := uint64(100_000)

	txValue := new(big.Int).Mul(big.NewInt(1e6), big.NewInt(1e12)) // 1e18 wei = 1 IOTX
	exec := action.NewExecution(identityset.Address(1).String(), txValue, nil)
	elp := (&action.EnvelopeBuilder{}).
		SetChainID(chainCfg.ID).
		SetNonce(1).
		SetGasLimit(gasLimit).
		SetGasPrice(gasPrice).
		SetAction(exec).
		Build()
	selp, err := action.Sign(elp, senderKey)
	require.NoError(err)

	// system action: GrantReward to producer (block-last action).
	grant := action.NewGrantReward(action.BlockReward, 1)
	grantElp := (&action.EnvelopeBuilder{}).
		SetNonce(0).
		SetGasPrice(big.NewInt(0)).
		SetAction(grant).
		Build()
	grantSelp, err := action.Sign(grantElp, identityset.PrivateKey(0))
	require.NoError(err)

	// Replay context mirroring what DebankBlock sets up.
	replayCtx := startCtx
	replayCtx = protocol.WithBlockCtx(replayCtx, protocol.BlockCtx{
		BlockHeight:    1,
		BlockTimeStamp: time.Unix(g.Timestamp+1, 0),
		Producer:       identityset.Address(0),
		GasLimit:       g.BlockGasLimit,
		BaseFee:        baseFee,
	})
	replayCtx = protocol.WithFeatureCtx(protocol.WithFeatureWithHeightCtx(replayCtx))
	replayCtx = protocol.WithRegistry(replayCtx, registry)
	replayCtx = evm.WithHelperCtx(replayCtx, evm.HelperContext{
		GetBlockHash:   getBlockHash,
		GetBlockTime:   getBlockTime,
		DepositGasFunc: rewarding.DepositGas,
	})

	collector := protocol.NewPipelineStateDiffCollector()
	replayCtx = protocol.WithStateDiffCollectorCtx(replayCtx, collector)

	// Run replay (what trace_debankBlock does).
	ws, err := sf.WorkingSetAtTransaction(replayCtx, 1, selp, grantSelp)
	require.NoError(err)
	defer ws.Close()

	wsInner, ok := ws.(*workingSet)
	require.True(ok, "ws should be *workingSet")
	receipts, err := wsInner.Receipts()
	require.NoError(err)
	require.GreaterOrEqual(len(receipts), 1)
	rcpt := receipts[0]
	t.Logf("replay receipt: status=%d gas_consumed=%d", rcpt.Status, rcpt.GasConsumed)

	// Truth: post-replay balance read from ws.
	postAcc, err := accountutil.LoadAccount(ws, sender)
	require.NoError(err)
	t.Logf("ws post-replay sender balance (truth) = %s", postAcc.Balance)

	// Observed: collector's captured balance.
	senderEvmAddr := common.BytesToAddress(sender.Bytes())
	senderHash := crypto.Keccak256Hash(senderEvmAddr.Bytes())
	accBytes, ok := collector.Accounts[senderHash]
	require.True(ok, "sender hash %s not in collector.Accounts (keys=%d)", senderHash.Hex(), len(collector.Accounts))

	var slim types.SlimAccount
	require.NoError(rlp.DecodeBytes(accBytes, &slim))
	dbkBalance := slim.Balance.ToBig()
	t.Logf("collector (state_diff) sender balance = %s", dbkBalance)

	diff := new(big.Int).Sub(postAcc.Balance, dbkBalance)
	t.Logf("diff (truth - state_diff) = %s wei", diff)

	if diff.Sign() != 0 {
		gasUsed := new(big.Int).SetUint64(rcpt.GasConsumed)
		expectedDiff := new(big.Int).Mul(gasUsed, new(big.Int).Sub(baseFee, big.NewInt(priority)))
		t.Logf("bug hypothesis: gas_used × (base_fee - priority) = %s", expectedDiff)
		if diff.Cmp(expectedDiff) == 0 {
			t.Log("BUG CONFIRMED (diff matches gas_used × (base_fee - priority))")
		}
	}

	require.Zero(diff.Sign(),
		"state_diff sender balance should equal ws sender balance, diff=%s", diff)
}
