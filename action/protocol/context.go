// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package protocol

import (
	"context"
	"math/big"
	"time"

	ptracer "github.com/Chaintable/pipeline/tracer"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/v2/action"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
)

type (
	blockchainContextKey struct{}

	blockContextKey struct{}

	actionContextKey struct{}

	registryContextKey struct{}

	featureContextKey struct{}

	featureWithHeightContextKey struct{}

	vmConfigContextKey struct{}

	// TipInfo contains the tip block information
	TipInfo struct {
		Height        uint64
		GasUsed       uint64
		Hash          hash.Hash256
		StateDigest   hash.Hash256
		Timestamp     time.Time
		BaseFee       *big.Int
		BlobGasUsed   uint64
		ExcessBlobGas uint64
	}

	// BlockchainCtx provides blockchain auxiliary information.
	BlockchainCtx struct {
		// Tip is the information of tip block
		Tip TipInfo
		//ChainID is the native chain ID
		ChainID uint32
		// EvmNetworkID is the EVM network ID
		EvmNetworkID uint32
		// GetBlockHash is the function to get block hash by height
		GetBlockHash func(uint64) (hash.Hash256, error)
		// GetBlockTime is the function to get block time by height
		GetBlockTime func(uint64) (time.Time, error)
	}

	// BlockCtx provides block auxiliary information.
	BlockCtx struct {
		// height of block containing those actions
		BlockHeight uint64
		// timestamp of block containing those actions
		BlockTimeStamp time.Time
		// gas Limit for perform those actions
		GasLimit uint64
		// Producer is the address of whom composes the block containing this action
		Producer address.Address
		// AccumTips is the accumulated tips of the block
		AccumulatedTips big.Int
		// BaseFee is the base fee of the block
		BaseFee *big.Int
		// ExcessBlobGas is the excess blob gas of the block
		ExcessBlobGas uint64
		// SkipSidecarValidation dictates to validate sidecar (for blob tx) or not
		SkipSidecarValidation bool
		// Simulate is used for read-only APIs
		Simulate bool
	}

	// ActionCtx provides action auxiliary information.
	ActionCtx struct {
		// Caller is the address of whom issues this action
		Caller address.Address
		// ActionHash is the hash of the action with the sealed envelope
		ActionHash hash.Hash256
		// GasPrice is the action gas price
		GasPrice *big.Int
		// IntrinsicGas is the action intrinsic gas
		IntrinsicGas uint64
		// Nonce is the nonce of the action
		Nonce uint64
		// ReadOnly indicates two scenarios: eth_estimateGas and eth_call
		ReadOnly bool
	}

	// CheckFunc is function type to check by height.
	CheckFunc func(height uint64) bool

	// FeatureCtx provides features information.
	FeatureCtx struct {
		FixDoubleChargeGas                      bool
		SystemWideActionGasLimit                bool
		NotFixTopicCopyBug                      bool
		SetRevertMessageToReceipt               bool
		FixGetHashFnHeight                      bool
		FixSortCacheContractsAndUsePendingNonce bool
		AsyncContractTrie                       bool
		AddOutOfGasToTransactionLog             bool
		AddChainIDToConfig                      bool
		UseV2Storage                            bool
		CannotUnstakeAgain                      bool
		SkipStakingIndexer                      bool
		ReturnFetchError                        bool
		CannotTranferToSelf                     bool
		NewStakingReceiptFormat                 bool
		UpdateBlockMeta                         bool
		CurrentEpochProductivity                bool
		FixSnapshotOrder                        bool
		AllowCorrectDefaultChainID              bool
		CorrectGetHashFn                        bool
		CorrectTxLogIndex                       bool
		RevertLog                               bool
		TolerateLegacyAddress                   bool
		CreateLegacyNonceAccount                bool
		FixGasAndNonceUpdate                    bool
		FixUnproductiveDelegates                bool
		CorrectGasRefund                        bool
		SufficentBalanceGuarantee               bool
		TolerateEmptyCandidateName              bool
		SkipSystemActionNonce                   bool
		ValidateSystemAction                    bool
		AllowCorrectChainIDOnly                 bool
		AddContractStakingVotes                 bool
		FixContractStakingWeightedVotes         bool
		ExecutionSizeLimit32KB                  bool
		UseZeroNonceForFreshAccount             bool
		CandidateRegisterMustWithStake          bool
		DisableDelegateEndorsement              bool
		RefactorFreshAccountConversion          bool
		SuicideTxLogMismatchPanic               bool
		PanicUnrecoverableError                 bool
		CandidateIdentifiedByOwner              bool
		LimitedStakingContract                  bool
		MigrateNativeStake                      bool
		AddClaimRewardAddress                   bool
		EnforceLegacyEndorsement                bool
		EnableDynamicFeeTx                      bool
		EnableBlobTransaction                   bool
		EnableCancunEVM                         bool
		CorrectValidationOrder                  bool
		UnstakedButNotClearSelfStakeAmount      bool
		CheckStakingDurationUpperLimit          bool
		FixRevertSnapshot                       bool
		TimestampedStakingContract              bool
		PreStateSystemAction                    bool
		CreatePostActionStates                  bool
		NotSlashUnproductiveDelegates           bool
		CandidateBLSPublicKey                   bool
		NotUseMinSelfStakeToBeActive            bool
		StoreVoteOfNFTBucketIntoView            bool
		CandidateSlashByOwner                   bool
		CandidateBLSPublicKeyNotCopied          bool
		OnlyOwnerCanUpdateBLSPublicKey          bool
		PrePectraEVM                            bool
		// AlwaysWriteCachedContract if true, CommitContracts writes back all cached
		// contracts regardless of whether they were modified; if false, only dirty
		// contracts are committed and written back
		AlwaysWriteCachedContract bool
		NoCandidateExitQueue      bool
	}

	// FeatureWithHeightCtx provides feature check functions.
	FeatureWithHeightCtx struct {
		GetUnproductiveDelegates        CheckFunc
		ReadStateFromDB                 CheckFunc
		UseV2Staking                    CheckFunc
		EnableNativeStaking             CheckFunc
		StakingCorrectGas               CheckFunc
		CalculateProbationList          CheckFunc
		LoadCandidatesLegacy            CheckFunc
		CandCenterHasAlias              CheckFunc
		CandidateWithoutIdentity        CheckFunc
		CandidateWithoutIdentityStorage CheckFunc
	}
)

// WithRegistry adds registry to context
func WithRegistry(ctx context.Context, reg *Registry) context.Context {
	return context.WithValue(ctx, registryContextKey{}, reg)
}

// GetRegistry returns the registry from context
func GetRegistry(ctx context.Context) (*Registry, bool) {
	reg, ok := ctx.Value(registryContextKey{}).(*Registry)
	return reg, ok
}

// MustGetRegistry returns the registry from context
func MustGetRegistry(ctx context.Context) *Registry {
	reg, ok := ctx.Value(registryContextKey{}).(*Registry)
	if !ok {
		log.S().Panic("Miss registry context")
	}
	return reg
}

// WithBlockchainCtx add BlockchainCtx into context.
func WithBlockchainCtx(ctx context.Context, bc BlockchainCtx) context.Context {
	return context.WithValue(ctx, blockchainContextKey{}, bc)
}

// GetBlockchainCtx gets BlockchainCtx
func GetBlockchainCtx(ctx context.Context) (BlockchainCtx, bool) {
	bc, ok := ctx.Value(blockchainContextKey{}).(BlockchainCtx)
	return bc, ok
}

// MustGetBlockchainCtx must get BlockchainCtx.
// If context doesn't exist, this function panic.
func MustGetBlockchainCtx(ctx context.Context) BlockchainCtx {
	bc, ok := ctx.Value(blockchainContextKey{}).(BlockchainCtx)
	if !ok {
		log.S().Panic("Miss blockchain context")
	}
	return bc
}

// WithBlockCtx add BlockCtx into context.
func WithBlockCtx(ctx context.Context, blk BlockCtx) context.Context {
	return context.WithValue(ctx, blockContextKey{}, blk)
}

// GetBlockCtx gets BlockCtx
func GetBlockCtx(ctx context.Context) (BlockCtx, bool) {
	blk, ok := ctx.Value(blockContextKey{}).(BlockCtx)
	return blk, ok
}

// MustGetBlockCtx must get BlockCtx .
// If context doesn't exist, this function panic.
func MustGetBlockCtx(ctx context.Context) BlockCtx {
	blk, ok := ctx.Value(blockContextKey{}).(BlockCtx)
	if !ok {
		log.S().Panic("Miss block context")
	}
	return blk
}

// WithActionCtx add ActionCtx into context.
func WithActionCtx(ctx context.Context, ac ActionCtx) context.Context {
	return context.WithValue(ctx, actionContextKey{}, ac)
}

// GetActionCtx gets ActionCtx
func GetActionCtx(ctx context.Context) (ActionCtx, bool) {
	ac, ok := ctx.Value(actionContextKey{}).(ActionCtx)
	return ac, ok
}

// MustGetActionCtx must get ActionCtx .
// If context doesn't exist, this function panic.
func MustGetActionCtx(ctx context.Context) ActionCtx {
	ac, ok := ctx.Value(actionContextKey{}).(ActionCtx)
	if !ok {
		log.S().Panic("Miss action context")
	}
	return ac
}

// WithFeatureCtx add FeatureCtx into context.
func WithFeatureCtx(ctx context.Context) context.Context {
	g := genesis.MustExtractGenesisContext(ctx)
	height := MustGetBlockCtx(ctx).BlockHeight
	return context.WithValue(
		ctx,
		featureContextKey{},
		FeatureCtx{
			FixDoubleChargeGas:                      g.IsPacific(height),
			SystemWideActionGasLimit:                !g.IsAleutian(height),
			NotFixTopicCopyBug:                      !g.IsAleutian(height),
			SetRevertMessageToReceipt:               g.IsHawaii(height),
			FixGetHashFnHeight:                      g.IsHawaii(height),
			FixSortCacheContractsAndUsePendingNonce: g.IsHawaii(height),
			AsyncContractTrie:                       g.IsGreenland(height),
			AddOutOfGasToTransactionLog:             !g.IsGreenland(height),
			AddChainIDToConfig:                      g.IsIceland(height),
			UseV2Storage:                            g.IsGreenland(height),
			CannotUnstakeAgain:                      g.IsGreenland(height),
			SkipStakingIndexer:                      !g.IsFairbank(height),
			ReturnFetchError:                        !g.IsGreenland(height),
			CannotTranferToSelf:                     g.IsHawaii(height),
			NewStakingReceiptFormat:                 g.IsFbkMigration(height),
			UpdateBlockMeta:                         g.IsGreenland(height),
			CurrentEpochProductivity:                g.IsGreenland(height),
			FixSnapshotOrder:                        g.IsKamchatka(height),
			AllowCorrectDefaultChainID:              g.IsMidway(height),
			CorrectGetHashFn:                        g.IsMidway(height),
			CorrectTxLogIndex:                       g.IsMidway(height),
			RevertLog:                               g.IsMidway(height),
			TolerateLegacyAddress:                   !g.IsNewfoundland(height),
			CreateLegacyNonceAccount:                !g.IsOkhotsk(height),
			FixGasAndNonceUpdate:                    g.IsOkhotsk(height),
			FixUnproductiveDelegates:                g.IsOkhotsk(height),
			CorrectGasRefund:                        g.IsOkhotsk(height),
			SufficentBalanceGuarantee:               g.IsOkhotsk(height),
			TolerateEmptyCandidateName:              !g.IsPalau(height),
			SkipSystemActionNonce:                   g.IsPalau(height),
			ValidateSystemAction:                    g.IsQuebec(height),
			AllowCorrectChainIDOnly:                 g.IsQuebec(height),
			AddContractStakingVotes:                 g.IsQuebec(height),
			FixContractStakingWeightedVotes:         g.IsRedsea(height),
			ExecutionSizeLimit32KB:                  !g.IsSumatra(height),
			UseZeroNonceForFreshAccount:             g.IsSumatra(height),
			CandidateRegisterMustWithStake:          !g.IsTsunami(height),
			DisableDelegateEndorsement:              !g.IsTsunami(height),
			RefactorFreshAccountConversion:          g.IsTsunami(height),
			SuicideTxLogMismatchPanic:               g.IsUpernavik(height),
			PanicUnrecoverableError:                 g.IsUpernavik(height),
			CandidateIdentifiedByOwner:              !g.IsUpernavik(height),
			LimitedStakingContract:                  !g.IsUpernavik(height),
			MigrateNativeStake:                      g.IsUpernavik(height),
			AddClaimRewardAddress:                   g.IsUpernavik(height),
			EnforceLegacyEndorsement:                !g.IsUpernavik(height),
			EnableDynamicFeeTx:                      g.IsVanuatu(height),
			EnableBlobTransaction:                   g.IsVanuatu(height),
			EnableCancunEVM:                         g.IsVanuatu(height),
			CorrectValidationOrder:                  g.IsVanuatu(height),
			UnstakedButNotClearSelfStakeAmount:      !g.IsVanuatu(height),
			CheckStakingDurationUpperLimit:          g.IsVanuatu(height),
			FixRevertSnapshot:                       g.IsVanuatu(height),
			TimestampedStakingContract:              g.IsWake(height),
			PreStateSystemAction:                    !g.IsWake(height),
			CreatePostActionStates:                  g.IsWake(height),
			NotSlashUnproductiveDelegates:           !g.IsXingu(height),
			CandidateBLSPublicKey:                   g.IsXingu(height),
			NotUseMinSelfStakeToBeActive:            !g.IsXingu(height),
			StoreVoteOfNFTBucketIntoView:            !g.IsXingu(height),
			CandidateSlashByOwner:                   !g.IsXinguBeta(height),
			CandidateBLSPublicKeyNotCopied:          !g.IsXinguBeta(height),
			OnlyOwnerCanUpdateBLSPublicKey:          !g.IsYap(height),
			PrePectraEVM:                            !g.IsYap(height),
			AlwaysWriteCachedContract:               !g.IsYap(height),
			NoCandidateExitQueue:                    !g.IsYap(height),
		},
	)
}

func (fCtx *FeatureCtx) Tolerate(err error) bool {
	if fCtx.TolerateEmptyCandidateName && errors.Cause(err) == action.ErrInvalidCanName {
		return true
	}
	return false
}

// GetFeatureCtx gets FeatureCtx.
func GetFeatureCtx(ctx context.Context) (FeatureCtx, bool) {
	fc, ok := ctx.Value(featureContextKey{}).(FeatureCtx)
	return fc, ok
}

// MustGetFeatureCtx must get FeatureCtx.
// If context doesn't exist, this function panic.
func MustGetFeatureCtx(ctx context.Context) FeatureCtx {
	fc, ok := ctx.Value(featureContextKey{}).(FeatureCtx)
	if !ok {
		log.L().Panic("Miss feature context")
	}
	return fc
}

// WithFeatureWithHeightCtx add FeatureWithHeightCtx into context.
func WithFeatureWithHeightCtx(ctx context.Context) context.Context {
	g := genesis.MustExtractGenesisContext(ctx)
	return context.WithValue(
		ctx,
		featureWithHeightContextKey{},
		FeatureWithHeightCtx{
			GetUnproductiveDelegates: func(height uint64) bool {
				return !g.IsEaster(height)
			},
			ReadStateFromDB: func(height uint64) bool {
				return g.IsGreenland(height)
			},
			UseV2Staking: func(height uint64) bool {
				return g.IsFairbank(height)
			},
			EnableNativeStaking: func(height uint64) bool {
				return g.IsCook(height)
			},
			StakingCorrectGas: func(height uint64) bool {
				return g.IsDaytona(height)
			},
			CalculateProbationList: func(height uint64) bool {
				return g.IsEaster(height)
			},
			LoadCandidatesLegacy: func(height uint64) bool {
				return !g.IsEaster(height)
			},
			CandCenterHasAlias: func(height uint64) bool {
				return !g.IsOkhotsk(height)
			},
			CandidateWithoutIdentity: func(height uint64) bool {
				return !g.IsYapBeta(height)
			},
			CandidateWithoutIdentityStorage: func(height uint64) bool {
				return !g.IsYap(height)
			},
		},
	)
}

// GetFeatureWithHeightCtx gets FeatureWithHeightCtx.
func GetFeatureWithHeightCtx(ctx context.Context) (FeatureWithHeightCtx, bool) {
	fc, ok := ctx.Value(featureWithHeightContextKey{}).(FeatureWithHeightCtx)
	return fc, ok
}

// MustGetFeatureWithHeightCtx must get FeatureWithHeightCtx.
// If context doesn't exist, this function panic.
func MustGetFeatureWithHeightCtx(ctx context.Context) FeatureWithHeightCtx {
	fc, ok := ctx.Value(featureWithHeightContextKey{}).(FeatureWithHeightCtx)
	if !ok {
		log.S().Panic("Miss feature context")
	}
	return fc
}

// WithVMConfigCtx adds vm config to context
func WithVMConfigCtx(ctx context.Context, vmConfig vm.Config) context.Context {
	return context.WithValue(ctx, vmConfigContextKey{}, vmConfig)
}

// GetVMConfigCtx returns the vm config from context
func GetVMConfigCtx(ctx context.Context) (vm.Config, bool) {
	cfg, ok := ctx.Value(vmConfigContextKey{}).(vm.Config)
	return cfg, ok
}

type pipelineHooksContextKey struct{}

// WithPipelineHooksCtx adds pipeline tracing hooks to context
func WithPipelineHooksCtx(ctx context.Context, hooks *tracing.Hooks) context.Context {
	return context.WithValue(ctx, pipelineHooksContextKey{}, hooks)
}

// GetPipelineHooksCtx returns the pipeline hooks from context, nil if not set
func GetPipelineHooksCtx(ctx context.Context) *tracing.Hooks {
	hooks, _ := ctx.Value(pipelineHooksContextKey{}).(*tracing.Hooks)
	return hooks
}

// Pipeline tracer ctx — drives OnCommit via direct *PipelineTracer call (R1).
// v1.15.11 tracing.Hooks struct has no OnCommit field (chaintable's geth fork extension);
// iotex-core merge v2.4.1 onward keeps the *PipelineTracer reference in ctx and dispatches
// OnCommit through it instead of through hooks.OnCommit.
type pipelineTracerContextKey struct{}

// WithPipelineTracerCtx attaches a *PipelineTracer to ctx for OnCommit dispatch.
func WithPipelineTracerCtx(ctx context.Context, pt *ptracer.PipelineTracer) context.Context {
	return context.WithValue(ctx, pipelineTracerContextKey{}, pt)
}

// GetPipelineTracerCtx returns the *PipelineTracer from ctx, nil if not set.
func GetPipelineTracerCtx(ctx context.Context) *ptracer.PipelineTracer {
	pt, _ := ctx.Value(pipelineTracerContextKey{}).(*ptracer.PipelineTracer)
	return pt
}

// PipelineCommitter is the minimal interface for *tracer.PipelineTracer.OnCommit.
// Field/value types match pipeline OnCommit signature exactly (common.Hash keys +
// []byte values), not the conceptual *types.StateAccount form used elsewhere.
// Production wires WithPipelineCommitterCtx(ctx, bc.pipelineTracer) — *PipelineTracer
// satisfies this interface via its public OnCommit. Tests can wire a mock implementing
// the same signature so OnCommit dispatch is asserted without constructing a full
// PipelineTracer (which would require etcd / Kafka init).
type PipelineCommitter interface {
	OnCommit(
		originRoot, root common.Hash,
		destructs map[common.Hash]struct{},
		accounts map[common.Hash][]byte,
		accountsOrigin map[common.Address][]byte,
		storages map[common.Hash]map[common.Hash][]byte,
		storagesOrigin map[common.Address]map[common.Hash][]byte,
		codes map[common.Hash][]byte,
	)
}

type pipelineCommitterContextKey struct{}

// WithPipelineCommitterCtx attaches a PipelineCommitter to ctx.
func WithPipelineCommitterCtx(ctx context.Context, c PipelineCommitter) context.Context {
	return context.WithValue(ctx, pipelineCommitterContextKey{}, c)
}

// GetPipelineCommitterCtx returns the PipelineCommitter from ctx, nil if not set.
func GetPipelineCommitterCtx(ctx context.Context) PipelineCommitter {
	c, _ := ctx.Value(pipelineCommitterContextKey{}).(PipelineCommitter)
	return c
}

// PipelineStateDiffCollector accumulates state diffs across transactions within a block
type PipelineStateDiffCollector struct {
	Destructs map[common.Hash]struct{}
	Accounts  map[common.Hash][]byte
	Storages  map[common.Hash]map[common.Hash][]byte
	Codes     map[common.Hash][]byte
	// Debug enables trace_debankBlock debug logging for sender-balance bug investigation.
	// Emits [DEBANK_DBG] log lines at every sm.PutState(Account) and at the CommitContracts
	// EOA overwrite path.
	Debug bool

	// snapshots is a stack of map copies used for per-action transaction semantics
	// in Simulate mode. Snapshot before running an action; Revert on failure so the
	// collector does not retain "ghost" writes from actions that were skipped.
	snapshots []collectorSnapshot
}

// collectorSnapshot is a deep copy of the four diff maps at a point in time.
type collectorSnapshot struct {
	destructs map[common.Hash]struct{}
	accounts  map[common.Hash][]byte
	storages  map[common.Hash]map[common.Hash][]byte
	codes     map[common.Hash][]byte
}

// NewPipelineStateDiffCollector creates a new collector with initialized maps
func NewPipelineStateDiffCollector() *PipelineStateDiffCollector {
	return &PipelineStateDiffCollector{
		Destructs: make(map[common.Hash]struct{}),
		Accounts:  make(map[common.Hash][]byte),
		Storages:  make(map[common.Hash]map[common.Hash][]byte),
		Codes:     make(map[common.Hash][]byte),
	}
}

// Snapshot deep-copies the four diff maps and pushes the copy onto a stack.
// Returns an id (stack depth) to pass back to Revert.
// Values in Accounts/Codes are []byte produced fresh per write (RLP encode),
// and Storages slot values are fresh byte slices from the EVM statedb, so
// shallow references are safe to share between the live map and the snapshot.
func (c *PipelineStateDiffCollector) Snapshot() int {
	if c == nil {
		return -1
	}
	snap := collectorSnapshot{
		destructs: make(map[common.Hash]struct{}, len(c.Destructs)),
		accounts:  make(map[common.Hash][]byte, len(c.Accounts)),
		storages:  make(map[common.Hash]map[common.Hash][]byte, len(c.Storages)),
		codes:     make(map[common.Hash][]byte, len(c.Codes)),
	}
	for k := range c.Destructs {
		snap.destructs[k] = struct{}{}
	}
	for k, v := range c.Accounts {
		snap.accounts[k] = v
	}
	for k, slots := range c.Storages {
		clone := make(map[common.Hash][]byte, len(slots))
		for sk, sv := range slots {
			clone[sk] = sv
		}
		snap.storages[k] = clone
	}
	for k, v := range c.Codes {
		snap.codes[k] = v
	}
	c.snapshots = append(c.snapshots, snap)
	return len(c.snapshots) - 1
}

// Revert restores the collector maps to the state captured by Snapshot with
// the given id, and pops any snapshots taken after it. Safe no-op on a nil
// collector or an id that is out of range (typical when Snapshot was never
// called because the context had no collector).
func (c *PipelineStateDiffCollector) Revert(id int) {
	if c == nil || id < 0 || id >= len(c.snapshots) {
		return
	}
	snap := c.snapshots[id]
	c.Destructs = snap.destructs
	c.Accounts = snap.accounts
	c.Storages = snap.storages
	c.Codes = snap.codes
	c.snapshots = c.snapshots[:id]
}

// DiscardSnapshot pops the snapshot with the given id without reverting the
// live maps. Used on the success path: the snapshot is no longer needed.
// Pops snapshots taken after id as well, for symmetry with Revert.
func (c *PipelineStateDiffCollector) DiscardSnapshot(id int) {
	if c == nil || id < 0 || id >= len(c.snapshots) {
		return
	}
	c.snapshots = c.snapshots[:id]
}

type stateDiffCollectorContextKey struct{}

// WithStateDiffCollectorCtx adds a state diff collector to context
func WithStateDiffCollectorCtx(ctx context.Context, c *PipelineStateDiffCollector) context.Context {
	return context.WithValue(ctx, stateDiffCollectorContextKey{}, c)
}

// GetStateDiffCollectorCtx returns the state diff collector from context, nil if not set
func GetStateDiffCollectorCtx(ctx context.Context) *PipelineStateDiffCollector {
	c, _ := ctx.Value(stateDiffCollectorContextKey{}).(*PipelineStateDiffCollector)
	return c
}
