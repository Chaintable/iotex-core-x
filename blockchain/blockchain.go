// Copyright (c) 2024 IoTeX Foundation
// This source code is provided 'as is' and no warranties are given as to title or non-infringement, merchantability
// or fitness for purpose and, to the extent permitted by law, all liability for your use of the code is disclaimed.
// This source code is governed by Apache License 2.0 that can be found in the LICENSE file.

package blockchain

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Chaintable/pipeline/tracer"
	ptypes "github.com/Chaintable/pipeline/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/facebookgo/clock"
	"github.com/iotexproject/go-pkgs/crypto"
	"github.com/iotexproject/go-pkgs/hash"
	"github.com/iotexproject/iotex-address/address"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"github.com/iotexproject/iotex-core/v2/action/protocol"
	"github.com/iotexproject/iotex-core/v2/action/protocol/execution/evm"
	"github.com/iotexproject/iotex-core/v2/blockchain/block"
	"github.com/iotexproject/iotex-core/v2/blockchain/blockdao"
	"github.com/iotexproject/iotex-core/v2/blockchain/filedao"
	"github.com/iotexproject/iotex-core/v2/blockchain/genesis"
	"github.com/iotexproject/iotex-core/v2/pkg/lifecycle"
	"github.com/iotexproject/iotex-core/v2/pkg/log"
	"github.com/iotexproject/iotex-core/v2/pkg/prometheustimer"
	"github.com/iotexproject/iotex-core/v2/pkg/unit"
)

// const
const (
	SigP256k1  = "secp256k1"
	SigP256sm2 = "p256sm2"
)

var (
	_blockMtc = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "iotex_block_metrics",

			Help: "Block metrics.",
		},
		[]string{"type"},
	)
	// ErrInvalidTipHeight is the error returned when the block height is not valid
	ErrInvalidTipHeight = errors.New("invalid tip height")
	// ErrInvalidBlock is the error returned when the block is not valid
	ErrInvalidBlock = errors.New("failed to validate the block")
	// ErrActionNonce is the error when the nonce of the action is wrong
	ErrActionNonce = errors.New("invalid action nonce")
	// ErrInsufficientGas indicates the error of insufficient gas value for data storage
	ErrInsufficientGas = errors.New("insufficient intrinsic gas value")
	// ErrBalance indicates the error of balance
	ErrBalance = errors.New("invalid balance")
	// ErrPaused indicates the error of blockchain is paused
	ErrPaused = errors.New("blockchain is paused")
)

func init() {
	prometheus.MustRegister(_blockMtc)
}

type (
	// MintOptions is the options to mint a new block
	MintOptions struct {
		ProducerPrivateKey crypto.PrivateKey
	}
	// MintOption sets the mint options
	MintOption func(*MintOptions)
	// Blockchain represents the blockchain data structure and hosts the APIs to access it
	Blockchain interface {
		lifecycle.StartStopper

		// For exposing blockchain states
		// BlockHeaderByHeight return block header by height
		BlockHeaderByHeight(height uint64) (*block.Header, error)
		// BlockHeader return block header by hash
		BlockHeader(hash hash.Hash256) (*block.Header, error)
		// BlockFooterByHeight return block footer by height
		BlockFooterByHeight(height uint64) (*block.Footer, error)
		// ChainID returns the chain ID
		ChainID() uint32
		// EvmNetworkID returns the evm network ID
		EvmNetworkID() uint32
		// ChainAddress returns chain address on parent chain, the root chain return empty.
		ChainAddress() string
		// TipHash returns tip block's hash
		TipHash() hash.Hash256
		// TipHeight returns tip block's height
		TipHeight() uint64
		// Genesis returns the genesis
		Genesis() genesis.Genesis
		// Context returns current context
		Context(context.Context) (context.Context, error)
		// ContextAtHeight returns context at given height
		ContextAtHeight(context.Context, uint64) (context.Context, error)

		// For block operations
		// MintNewBlock creates a new block with given actions
		// Note: the coinbase transfer will be added to the given transfers when minting a new block
		MintNewBlock(time.Time, ...MintOption) (*block.Block, error)
		// CommitBlock validates and appends a block to the chain
		CommitBlock(blk *block.Block) error
		// ValidateBlock validates a new block before adding it to the blockchain
		ValidateBlock(*block.Block, ...BlockValidationOption) error

		// AddSubscriber make you listen to every single produced block
		AddSubscriber(BlockCreationSubscriber) error

		// RemoveSubscriber make you listen to every single produced block
		RemoveSubscriber(BlockCreationSubscriber) error
		//  Pause pauses the blockchain
		Pause(bool)
	}

	// BlockMinter is the block minter interface
	BlockMinter interface {
		// Mint creates a new block
		Mint(context.Context, crypto.PrivateKey) (*block.Block, error)
	}

	// blockchain implements the Blockchain interface
	blockchain struct {
		mu             sync.RWMutex // mutex to protect utk, tipHeight and tipHash
		dao            blockdao.BlockDAO
		config         Config
		genesis        genesis.Genesis
		blockValidator block.Validator
		lifecycle      lifecycle.Lifecycle
		clk            clock.Clock
		pubSubManager  PubSubManager
		timerFactory   *prometheustimer.TimerFactory

		// used by account-based model
		bbf   BlockMinter
		pause bool

		logger         *tracing.Hooks
		pipelineTracer *tracer.PipelineTracer
	}
)

// WithProducerPrivateKey sets the producer private key
func WithProducerPrivateKey(pk crypto.PrivateKey) MintOption {
	return func(options *MintOptions) {
		options.ProducerPrivateKey = pk
	}
}

// Productivity returns the map of the number of blocks produced per delegate in given epoch
func Productivity(bc Blockchain, startHeight uint64, endHeight uint64) (map[string]uint64, error) {
	stats := make(map[string]uint64)
	for i := startHeight; i <= endHeight; i++ {
		header, err := bc.BlockHeaderByHeight(i)
		if err != nil {
			return nil, err
		}
		producer := header.ProducerAddress()
		stats[producer]++
	}

	return stats, nil
}

// Option sets blockchain construction parameter
type Option func(*blockchain) error

// BlockValidatorOption sets block validator
func BlockValidatorOption(blockValidator block.Validator) Option {
	return func(bc *blockchain) error {
		bc.blockValidator = blockValidator
		return nil
	}
}

// ClockOption overrides the default clock
func ClockOption(clk clock.Clock) Option {
	return func(bc *blockchain) error {
		bc.clk = clk
		return nil
	}
}

type (
	BlockValidationCfg struct {
		skipSidecarValidation bool
	}

	BlockValidationOption func(*BlockValidationCfg)
)

func SkipSidecarValidationOption() BlockValidationOption {
	return func(opts *BlockValidationCfg) {
		opts.skipSidecarValidation = true
	}
}

// NewBlockchain creates a new blockchain and DB instance
func NewBlockchain(cfg Config, g genesis.Genesis, dao blockdao.BlockDAO, bbf BlockMinter, opts ...Option) Blockchain {
	// create the Blockchain
	chain := &blockchain{
		config:        cfg,
		genesis:       g,
		dao:           dao,
		bbf:           bbf,
		clk:           clock.New(),
		pubSubManager: NewPubSub(cfg.StreamingBlockBufferSize),
	}
	for _, opt := range opts {
		if err := opt(chain); err != nil {
			log.S().Panicf("Failed to execute blockchain creation option %p: %v", opt, err)
		}
	}
	timerFactory, err := prometheustimer.New(
		"iotex_blockchain_perf",
		"Performance of blockchain module",
		[]string{"topic", "chainID"},
		[]string{"default", strconv.FormatUint(uint64(cfg.ID), 10)},
	)
	if err != nil {
		log.L().Panic("Failed to generate prometheus timer factory.", zap.Error(err))
	}
	chain.timerFactory = timerFactory
	if chain.dao == nil {
		log.L().Panic("blockdao is nil")
	}
	chain.lifecycle.Add(chain.dao)
	chain.lifecycle.Add(chain.pubSubManager)

	if cfg.VMTraceConfig != "" {
		t, err := tracer.NewPipelineTracer(json.RawMessage(cfg.VMTraceConfig))
		if err != nil {
			log.L().Panic("failed to create tracer.", zap.Error(err))
		}
		chain.pipelineTracer = t
		chain.logger = tracing.BuildHooks(t)
		log.L().Info("pipeline tracer created", zap.String("config", cfg.VMTraceConfig))
	}

	if chain.logger != nil && chain.logger.OnBlockchainInit != nil {
		initCtx := genesis.WithGenesisContext(
			protocol.WithBlockchainCtx(context.Background(), protocol.BlockchainCtx{
				ChainID:      cfg.ID,
				EvmNetworkID: cfg.EVMNetworkID,
				GetBlockTime: chain.getBlockTime,
			}), g)
		initCtx = protocol.WithBlockCtx(initCtx, protocol.BlockCtx{BlockHeight: 0, BlockTimeStamp: time.Unix(g.Timestamp, 0)})
		chainConfig, err := evm.NewChainConfig(initCtx)
		if err != nil {
			log.L().Panic("failed to create chain config for pipeline tracer.", zap.Error(err))
		}
		// NewChainConfig only sets ChainID after Iceland fork (height 12289321),
		// but OnBlockchainInit is called with height=0, leaving ChainID nil.
		// Pipeline tracer uses ChainID.String() for S3 key paths — nil produces "<nil>".
		if chainConfig.ChainID == nil {
			chainConfig.ChainID = new(big.Int).SetUint64(uint64(cfg.EVMNetworkID))
		}
		chain.logger.OnBlockchainInit(chainConfig)
		// Initialize geth hash cache for ConvertToGethBlock.
		// On restart, LastPushedBlock() has the last block's geth hash from Kafka.
		// On fresh start, this is nil and ConvertToGethBlock handles block 1 specially.
		if tracer.NodeXPusher != nil {
			if lastPushed := tracer.NodeXPusher.LastPushedBlock(); lastPushed != nil {
				LastGethBlockHash = lastPushed.Hash
			}
		}
	}

	return chain
}

func (bc *blockchain) ChainID() uint32 {
	return atomic.LoadUint32(&bc.config.ID)
}

func (bc *blockchain) EvmNetworkID() uint32 {
	return atomic.LoadUint32(&bc.config.EVMNetworkID)
}

func (bc *blockchain) ChainAddress() string {
	return bc.config.Address
}

// Start starts the blockchain
func (bc *blockchain) Start(ctx context.Context) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.logger != nil {
		ctx = protocol.WithPipelineHooksCtx(ctx, bc.logger)
		ctx = protocol.WithPipelineEVMLoggerCtx(ctx, bc.pipelineTracer)
	}
	// pass registry to be used by state factory's initialization
	ctx = protocol.WithFeatureWithHeightCtx(genesis.WithGenesisContext(
		protocol.WithBlockchainCtx(
			ctx,
			protocol.BlockchainCtx{
				ChainID:      bc.ChainID(),
				EvmNetworkID: bc.EvmNetworkID(),
				GetBlockHash: bc.dao.GetBlockHash,
				GetBlockTime: bc.getBlockTime,
			},
		), bc.genesis))
	return bc.lifecycle.OnStart(ctx)
}

// Stop stops the blockchain.
func (bc *blockchain) Stop(ctx context.Context) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.logger != nil && bc.logger.OnClose != nil {
		bc.logger.OnClose()
	}
	return bc.lifecycle.OnStop(ctx)
}

func (bc *blockchain) Pause(pause bool) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	bc.pause = pause
}

func (bc *blockchain) BlockHeaderByHeight(height uint64) (*block.Header, error) {
	return bc.dao.HeaderByHeight(height)
}

func (bc *blockchain) BlockHeader(hash hash.Hash256) (*block.Header, error) {
	return bc.dao.Header(hash)
}

func (bc *blockchain) BlockFooterByHeight(height uint64) (*block.Footer, error) {
	return bc.dao.FooterByHeight(height)
}

// TipHash returns tip block's hash
func (bc *blockchain) TipHash() hash.Hash256 {
	tipHeight, err := bc.dao.Height()
	if err != nil {
		return hash.ZeroHash256
	}
	tipHash, err := bc.dao.GetBlockHash(tipHeight)
	if err != nil {
		return hash.ZeroHash256
	}
	return tipHash
}

// TipHeight returns tip block's height
func (bc *blockchain) TipHeight() uint64 {
	tipHeight, err := bc.dao.Height()
	if err != nil {
		log.L().Panic("failed to get tip height", zap.Error(err))
	}
	return tipHeight
}

// ValidateBlock validates a new block before adding it to the blockchain
func (bc *blockchain) ValidateBlock(blk *block.Block, opts ...BlockValidationOption) error {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	timer := bc.timerFactory.NewTimer("ValidateBlock")
	defer timer.End()
	if blk == nil {
		return ErrInvalidBlock
	}
	tipHeight, err := bc.dao.Height()
	if err != nil {
		return err
	}
	tip, err := bc.tipInfo(tipHeight)
	if err != nil {
		return err
	}
	// verify new block has height incremented by 1
	if blk.Height() != 0 && blk.Height() != tip.Height+1 {
		return errors.Wrapf(
			ErrInvalidTipHeight,
			"wrong block height %d, expecting %d",
			blk.Height(),
			tip.Height+1,
		)
	}
	// verify new block has correctly linked to current tip
	if blk.PrevHash() != tip.Hash {
		blk.HeaderLogger(log.L()).Error("Previous block hash doesn't match.",
			log.Hex("expectedBlockHash", tip.Hash[:]))
		return errors.Wrapf(
			ErrInvalidBlock,
			"wrong prev hash %x, expecting %x",
			blk.PrevHash(),
			tip.Hash,
		)
	}
	// verify EIP1559 header (baseFee adjustment)
	if blk.Header.BaseFee() != nil {
		if err = protocol.VerifyEIP1559Header(bc.genesis.Blockchain, tip, &blk.Header); err != nil {
			return errors.Wrap(err, "failed to verify EIP1559 header (baseFee adjustment)")
		}
	}
	if !blk.Header.VerifySignature() {
		return errors.Errorf("failed to verify block's signature with public key: %x", blk.PublicKey())
	}
	if err := blk.VerifyTxRoot(); err != nil {
		return err
	}

	producerAddr := blk.PublicKey().Address()
	if producerAddr == nil {
		return errors.New("failed to get address")
	}
	ctx, err := bc.context(context.Background(), tipHeight)
	if err != nil {
		return err
	}
	cfg := BlockValidationCfg{}
	for _, opt := range opts {
		opt(&cfg)
	}
	ctx = protocol.WithBlockCtx(ctx,
		protocol.BlockCtx{
			BlockHeight:           blk.Height(),
			BlockTimeStamp:        blk.Timestamp(),
			GasLimit:              bc.genesis.BlockGasLimitByHeight(blk.Height()),
			Producer:              producerAddr,
			BaseFee:               blk.BaseFee(),
			ExcessBlobGas:         blk.ExcessBlobGas(),
			SkipSidecarValidation: cfg.skipSidecarValidation,
		},
	)
	ctx = protocol.WithFeatureCtx(ctx)
	if bc.blockValidator == nil {
		return nil
	}
	return bc.blockValidator.Validate(ctx, blk)
}

func (bc *blockchain) Context(ctx context.Context) (context.Context, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	tipHeight, err := bc.dao.Height()
	if err != nil {
		return nil, err
	}
	return bc.context(ctx, tipHeight)
}

func (bc *blockchain) ContextAtHeight(ctx context.Context, height uint64) (context.Context, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	return bc.context(ctx, height)
}

func (bc *blockchain) contextWithBlock(ctx context.Context, producer address.Address, height uint64, timestamp time.Time, baseFee *big.Int, blobgas uint64) context.Context {
	return protocol.WithBlockCtx(
		ctx,
		protocol.BlockCtx{
			BlockHeight:    height,
			BlockTimeStamp: timestamp,
			Producer:       producer,
			GasLimit:       bc.genesis.BlockGasLimitByHeight(height),
			BaseFee:        baseFee,
			ExcessBlobGas:  blobgas,
		})
}

func (bc *blockchain) context(ctx context.Context, height uint64) (context.Context, error) {
	tip, err := bc.tipInfo(height)
	if err != nil {
		return nil, err
	}
	ctx = genesis.WithGenesisContext(
		protocol.WithBlockchainCtx(
			ctx,
			protocol.BlockchainCtx{
				Tip:          *tip,
				ChainID:      bc.ChainID(),
				EvmNetworkID: bc.EvmNetworkID(),
				GetBlockHash: bc.dao.GetBlockHash,
				GetBlockTime: bc.getBlockTime,
			},
		),
		bc.genesis,
	)
	return protocol.WithFeatureWithHeightCtx(ctx), nil
}

func (bc *blockchain) MintNewBlock(timestamp time.Time, opts ...MintOption) (*block.Block, error) {
	bc.mu.RLock()
	defer bc.mu.RUnlock()
	mintNewBlockTimer := bc.timerFactory.NewTimer("MintNewBlock")
	defer mintNewBlockTimer.End()
	var options MintOptions
	for _, opt := range opts {
		opt(&options)
	}
	tipHeight, err := bc.dao.Height()
	if err != nil {
		return nil, err
	}
	newblockHeight := tipHeight + 1
	ctx, err := bc.context(context.Background(), tipHeight)
	if err != nil {
		return nil, err
	}
	tip := protocol.MustGetBlockchainCtx(ctx).Tip
	producerPrivateKey := options.ProducerPrivateKey
	if producerPrivateKey == nil {
		privateKeys := bc.config.ProducerPrivateKeys()
		if len(privateKeys) == 0 {
			return nil, errors.New("no producer private key available")
		}
		producerPrivateKey = privateKeys[0]
	}
	minterAddress := producerPrivateKey.PublicKey().Address()
	log.L().Info("Minting a new block.", zap.Uint64("height", newblockHeight), zap.String("minter", minterAddress.String()))
	ctx = bc.contextWithBlock(ctx, minterAddress, newblockHeight, timestamp, protocol.CalcBaseFee(genesis.MustExtractGenesisContext(ctx).Blockchain, &tip), protocol.CalcExcessBlobGas(tip.ExcessBlobGas, tip.BlobGasUsed))
	ctx = protocol.WithFeatureCtx(ctx)
	if bc.logger != nil {
		ctx = protocol.WithPipelineHooksCtx(ctx, bc.logger)
		ctx = protocol.WithPipelineEVMLoggerCtx(ctx, bc.pipelineTracer)
	}
	// run execution and update state trie root hash
	blk, err := bc.bbf.Mint(ctx, producerPrivateKey)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create block")
	}
	_blockMtc.WithLabelValues("MintGas").Set(float64(blk.GasUsed()))
	_blockMtc.WithLabelValues("MintActions").Set(float64(len(blk.Actions)))
	return blk, nil
}

// CommitBlock validates and appends a block to the chain
func (bc *blockchain) CommitBlock(blk *block.Block) error {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.pause {
		return errors.Wrapf(ErrPaused, "blockchain is paused, cannot commit block %d, %x", blk.Height(), blk.HashBlock())
	}
	timer := bc.timerFactory.NewTimer("CommitBlock")
	defer timer.End()
	return bc.commitBlock(blk)
}

func (bc *blockchain) AddSubscriber(s BlockCreationSubscriber) error {
	log.L().Info("Add a subscriber.")
	if s == nil {
		return errors.New("subscriber could not be nil")
	}

	return bc.pubSubManager.AddBlockListener(s)
}

func (bc *blockchain) RemoveSubscriber(s BlockCreationSubscriber) error {
	return bc.pubSubManager.RemoveBlockListener(s)
}

//======================================
// internal functions
//=====================================

func (bc *blockchain) Genesis() genesis.Genesis {
	return bc.genesis
}

//======================================
// private functions
//=====================================

func (bc *blockchain) tipInfo(tipHeight uint64) (*protocol.TipInfo, error) {
	if tipHeight == 0 {
		return &protocol.TipInfo{
			Height:    0,
			Hash:      bc.genesis.Hash(),
			Timestamp: time.Unix(bc.genesis.Timestamp, 0),
		}, nil
	}
	header, err := bc.dao.HeaderByHeight(tipHeight)
	if err != nil {
		return nil, err
	}

	return &protocol.TipInfo{
		Height:        tipHeight,
		GasUsed:       header.GasUsed(),
		Hash:          header.HashBlock(),
		StateDigest:   header.DeltaStateDigest(),
		Timestamp:     header.Timestamp(),
		BaseFee:       header.BaseFee(),
		BlobGasUsed:   header.BlobGasUsed(),
		ExcessBlobGas: header.ExcessBlobGas(),
	}, nil
}

// commitBlock commits a block to the chain
func (bc *blockchain) commitBlock(blk *block.Block) error {
	tipHeight, err := bc.dao.Height()
	if err != nil {
		return err
	}
	ctx, err := bc.context(context.Background(), tipHeight)
	if err != nil {
		return err
	}
	ctx = bc.contextWithBlock(ctx, blk.PublicKey().Address(), blk.Height(), blk.Timestamp(), blk.BaseFee(), blk.ExcessBlobGas())
	ctx = protocol.WithFeatureCtx(ctx)
	if bc.logger != nil {
		ctx = protocol.WithPipelineHooksCtx(ctx, bc.logger)
		ctx = protocol.WithPipelineEVMLoggerCtx(ctx, bc.pipelineTracer)
	}
	// write block into DB
	putTimer := bc.timerFactory.NewTimer("putBlock")
	err = bc.dao.PutBlock(ctx, blk)
	putTimer.End()
	switch errors.Cause(err) {
	case filedao.ErrAlreadyExist, blockdao.ErrAlreadyExist:
		return nil
	case nil:
		bc.pushBlockChange(blk)
	default:
		return err
	}
	blkHash := blk.HashBlock()
	if blk.Height()%100 == 0 {
		blk.HeaderLogger(log.L()).Info("Committed a block.", log.Hex("tipHash", blkHash[:]))
	}
	_blockMtc.WithLabelValues("numActions").Set(float64(len(blk.Actions)))
	if blk.BaseFee() != nil {
		basefeeQev := new(big.Int).Div(blk.BaseFee(), big.NewInt(unit.Qev))
		_blockMtc.WithLabelValues("baseFee").Set(float64(basefeeQev.Int64()))
	}
	_blockMtc.WithLabelValues("excessBlobGas").Set(float64(blk.ExcessBlobGas()))
	_blockMtc.WithLabelValues("blobGasUsed").Set(float64(blk.BlobGasUsed()))
	_blockMtc.WithLabelValues("gasUsed").Set(float64(blk.GasUsed()))
	// emit block to all block subscribers
	bc.emitToSubscribers(blk)
	return nil
}

// getCommonAncestor finds the common ancestor between two block contexts and returns
// the ancestor, the chain from ancestor to blocka (dropBlocks), and the chain from ancestor to blockb (newBlocks).
// NOTE: The slow path (reorg walk-back) uses dao.GetBlockHash/Header which return IoTeX native hashes.
// Since pushBlockChange now uses geth hashes, the slow path would not work correctly.
// This is acceptable because IoTeX uses DPoS consensus with no reorgs — only the fast path
// (blockb.ParentHash == blocka.Hash) is ever reached.
func (bc *blockchain) getCommonAncestor(blocka ptypes.BlockContext, blockb ptypes.BlockContext) (ptypes.BlockContext, []ptypes.BlockContext, []ptypes.BlockContext) {
	var chainA, chainB []ptypes.BlockContext
	if blockb.ParentHash == blocka.Hash {
		return blocka, chainA, []ptypes.BlockContext{blockb}
	}
	for blockb.BlockNumber > blocka.BlockNumber {
		chainB = append(chainB, blockb)
		headerb, err := bc.dao.Header(hash.Hash256(blockb.ParentHash))
		if err != nil {
			log.L().Fatal("Failed to get header by hash", zap.String("hash", blockb.ParentHash.Hex()), zap.Error(err))
		}
		// Use dao.GetBlockHash instead of header.HashBlock() to stay consistent
		// with IoTeX's hash system where GetBlockHash(0) = GenesisHash() (config hash),
		// which differs from GenesisBlock().HashBlock() (header protobuf hash).
		blkHash, err := bc.dao.GetBlockHash(headerb.Height())
		if err != nil {
			log.L().Fatal("Failed to get block hash", zap.Uint64("height", headerb.Height()), zap.Error(err))
		}
		prevHash := headerb.PrevHash()
		blockb = ptypes.BlockContext{
			BlockNumber: headerb.Height(),
			Hash:        common.Hash(blkHash),
			ParentHash:  common.Hash(prevHash),
			Timestamp:   uint64(headerb.Timestamp().Unix()),
		}
	}
	for blocka.Hash != blockb.Hash {
		chainA = append(chainA, blocka)
		headera, err := bc.dao.Header(hash.Hash256(blocka.ParentHash))
		if err != nil {
			log.L().Fatal("Failed to get header by hash", zap.String("hash", blocka.ParentHash.Hex()), zap.Error(err))
		}
		blkHashA, err := bc.dao.GetBlockHash(headera.Height())
		if err != nil {
			log.L().Fatal("Failed to get block hash", zap.Uint64("height", headera.Height()), zap.Error(err))
		}
		prevHash := headera.PrevHash()
		blocka = ptypes.BlockContext{
			BlockNumber: headera.Height(),
			Hash:        common.Hash(blkHashA),
			ParentHash:  common.Hash(prevHash),
			Timestamp:   uint64(headera.Timestamp().Unix()),
		}

		chainB = append(chainB, blockb)
		headerb, err := bc.dao.Header(hash.Hash256(blockb.ParentHash))
		if err != nil {
			log.L().Fatal("Failed to get header by hash", zap.String("hash", blockb.ParentHash.Hex()), zap.Error(err))
		}
		blkHashB, err := bc.dao.GetBlockHash(headerb.Height())
		if err != nil {
			log.L().Fatal("Failed to get block hash", zap.Uint64("height", headerb.Height()), zap.Error(err))
		}
		prevHash = headerb.PrevHash()
		blockb = ptypes.BlockContext{
			BlockNumber: headerb.Height(),
			Hash:        common.Hash(blkHashB),
			ParentHash:  common.Hash(prevHash),
			Timestamp:   uint64(headerb.Timestamp().Unix()),
		}
	}
	slices.Reverse(chainA)
	slices.Reverse(chainB)
	return blocka, chainA, chainB
}

// pushBlockChange pushes block change notification to kafka
func (bc *blockchain) pushBlockChange(blk *block.Block) {
	if tracer.NodeXPusher == nil {
		return
	}
	lastPushed := tracer.NodeXPusher.LastPushedBlock()
	if lastPushed == nil || lastPushed.BlockNumber > blk.Height() {
		return
	}
	lastCtx := *lastPushed
	// Use geth hash from pipeline tracer (set in OnBlockStart via event.Block.Hash())
	// to match S3 keys which also use geth hashes (uploaded in OnCommit).
	// IoTeX DAO uses a different hash system (GenesisHash for block 0,
	// protobuf hash for others) — do NOT use dao.GetBlockHash() here.
	blkGethHash := tracer.BlockCtx.BlockHash

	// For sequential blocks (always the case with DPoS — no reorgs),
	// use lastCtx.Hash as ParentHash to maintain geth hash chain consistency
	// in Kafka. blk.PrevHash() returns IoTeX native hash which differs from
	// the geth hash stored in Kafka by OnGenesisBlock/OnCommit.
	var parentHash common.Hash
	if blk.Height() == lastCtx.BlockNumber+1 {
		parentHash = lastCtx.Hash
	} else {
		// Non-sequential: should not happen with DPoS consensus.
		log.L().Warn("pushBlockChange: non-sequential block, falling back to native PrevHash",
			zap.Uint64("blkHeight", blk.Height()),
			zap.Uint64("lastPushedHeight", lastCtx.BlockNumber))
		parentHash = common.Hash(blk.PrevHash())
	}

	_, dropBlocks, newBlocks := bc.getCommonAncestor(lastCtx, ptypes.BlockContext{
		BlockNumber: blk.Height(),
		Hash:        blkGethHash,
		ParentHash:  parentHash,
		Timestamp:   uint64(blk.Timestamp().Unix()),
	})
	var blockChange *ptypes.BlockChangeNotification
	if len(dropBlocks) > 0 {
		log.L().Info("pushBlockChange drop blocks", zap.String("hash", blkGethHash.Hex()))
		blockChange = &ptypes.BlockChangeNotification{
			ChangeType: 2,
			NewBlocks:  newBlocks,
			DropBlocks: dropBlocks,
		}
	} else if len(newBlocks) > 0 {
		log.L().Info("pushBlockChange new blocks", zap.String("hash", blkGethHash.Hex()))
		blockChange = &ptypes.BlockChangeNotification{
			ChangeType: 1,
			NewBlocks:  newBlocks,
		}
	}
	if blockChange != nil {
		if err := tracer.NodeXPusher.PushBlockChangeNotification(blockChange); err != nil {
			log.L().Error("PushBlockChangeNotification error", zap.Error(err))
		} else {
			log.L().Info("NodeXPusher PushBlockChangeNotification", zap.Uint64("height", blk.Height()))
		}
	}
}

func (bc *blockchain) emitToSubscribers(blk *block.Block) {
	if bc.pubSubManager == nil {
		return
	}
	bc.pubSubManager.SendBlockToSubscribers(blk)
}

func (bc *blockchain) getBlockTime(height uint64) (time.Time, error) {
	if height == 0 {
		return time.Unix(bc.genesis.Timestamp, 0), nil
	}
	header, err := bc.dao.HeaderByHeight(height)
	if err != nil {
		return time.Time{}, err
	}
	return header.Timestamp(), nil
}
