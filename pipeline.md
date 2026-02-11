# Chaintable Pipeline Tracer 集成技术方案

## 1. 背景

将 Chaintable pipeline tracer 集成到 iotex-core 中，实现区块和 EVM 数据的实时采集。参考 bitlayer-l2 的实现，适配 iotex-core 的架构差异。

### 1.1 架构差异

iotex-core 与 geth 的执行流程存在显著差异：

```
bitlayer (标准 geth):
  eth/backend.go → blockchain.insertChain() → state_processor.Process() → statedb.Commit()

iotex-core:
  blockchain.commitBlock() → dao.PutBlock() → stateDB.PutBlock() → ws.process() → ws.Commit()
```

关键区别：
- iotex 没有 `state_processor`，action 执行发生在 `workingSet.process()` / `pickAndRunActions()` 中
- 状态管理由 `state/factory` 包负责，而非 geth 的 `core/state`
- iotex 使用 `action.SealedEnvelope` 而非 `types.Transaction`，需要类型转换
- EVM 日志在 `StateDBAdapter.AddLog()` 中处理，而非 geth 的 `statedb.AddLog()`

### 1.2 前提条件

- iotex go-ethereum fork (`github.com/Chaintable/go-ethereum-iotex`) 已有 `core/tracing/hooks.go`，定义了 `Hooks` 结构和 `BuildHooks()` 函数
- `blockchain.Config` 已有 `VMTraceConfig string` 字段
- `blockchain` struct 已有 `logger *tracing.Hooks` 字段

## 2. Hook 列表与挂载点

pipeline tracer 共 9 个 hook，在 iotex-core 中的挂载点如下：

| Hook | 挂载位置 | 时机 |
|------|---------|------|
| `OnBlockchainInit` | `blockchain.NewBlockchain()` | 节点启动，tracer 创建后 |
| `OnClose` | `blockchain.Stop()` | 节点关闭 |
| `OnBlockStart` | `statedb.PutBlock()` | 区块处理开始前 |
| `OnBlockEnd` | `statedb.PutBlock()` | 区块处理完成后（含错误路径） |
| `OnTxStart` | `workingset.process()` / `pickAndRunActions()` / `runActionsLegacy()` | 每个 action 执行前 |
| `OnTxEnd` | 同上 | 每个 action 执行后 |
| `OnLog` | `StateDBAdapter.AddLog()` | EVM 合约事件产生时 |
| `OnCommit` | `workingset.Commit()` | 状态提交后 |
| `OnGenesisBlock` | `statedb.createGenesisStates()` | 创世状态初始化后 |

## 3. 数据流

```
blockchain.NewBlockchain()
  │
  ├── tracer.NewPipelineTracer(cfg.VMTraceConfig)
  ├── tracing.BuildHooks(tracer)  →  chain.logger
  └── OnBlockchainInit(chainConfig)

blockchain.Start(ctx)
  │
  └── ctx = WithPipelineHooksCtx(ctx, logger)  →  传递给 statedb
      │
      └── statedb.createGenesisStates(ctx)
            └── OnGenesisBlock(genesisBlock, genesisAlloc)

blockchain.commitBlock(blk) / MintNewBlock()
  │
  └── ctx = WithPipelineHooksCtx(ctx, logger)
      │
      └── statedb.PutBlock(ctx, blk)
            │
            ├── OnBlockStart(gethBlock)
            │
            ├── workingset.process(ctx, actions)
            │     │
            │     ├── OnTxStart(ethTx, sender)    ─┐
            │     ├── ws.runAction()                │  每个 action 重复
            │     │     └── StateDBAdapter.AddLog() │
            │     │           └── OnLog(evmLog)     │
            │     └── OnTxEnd(ethReceipt, err)     ─┘
            │
            ├── workingset.Commit()
            │     └── OnCommit(hash{}, stateRoot, nil...)
            │
            └── OnBlockEnd(err)

blockchain.Stop()
  └── OnClose()
```

## 4. 实现详情

### 4.1 Context 传递机制

**文件**: `action/protocol/context.go`

通过 context 传递 pipeline hooks，避免在所有中间层添加参数：

```go
type pipelineHooksContextKey struct{}

func WithPipelineHooksCtx(ctx context.Context, hooks *tracing.Hooks) context.Context
func GetPipelineHooksCtx(ctx context.Context) *tracing.Hooks  // 返回 nil 如果不存在
```

设计考虑：
- `protocol` 包已经是所有模块共享的基础包，适合放置 context key
- 所有 hook 调用前均有 nil check，未启用 tracer 时零开销

### 4.2 类型转换

**文件**: `blockchain/pipeline_convert.go`（新建）

iotex 使用自有类型（`block.Block`、`action.Receipt`），pipeline tracer 需要 geth 类型（`types.Block`、`types.Receipt`），因此需要转换函数：

| 函数 | 输入 | 输出 | 说明 |
|------|------|------|------|
| `ConvertToGethBlock` | `*block.Block`, `genesis.Genesis` | `*types.Block` | 映射 header 字段 + 转换 actions 为 txs |
| `ConvertToGethReceipt` | `*action.Receipt` | `*types.Receipt` | 映射 receipt 字段 + 转换 logs |
| `BuildGenesisGethBlock` | `genesis.Genesis` | `*types.Block` | height=0 的空块 |
| `BuildGenesisAlloc` | `genesis.Genesis` | `types.GenesisAlloc` | InitBalanceMap → address:balance |

Header 字段映射：

| iotex Header | geth Header | 说明 |
|-------------|-------------|------|
| `Height()` | `Number` | 区块高度 |
| `Timestamp()` | `Time` | Unix 时间戳 |
| `GasUsed()` | `GasUsed` | 实际 gas 消耗 |
| `genesis.BlockGasLimit` | `GasLimit` | 区块 gas 上限（来自 genesis 配置） |
| `PrevHash()` | `ParentHash` | 父块哈希 |
| `DeltaStateDigest()` | `Root` | 状态根（对应 stateRoot） |
| `TxRoot()` | `TxHash` | 交易根 |
| `ReceiptRoot()` | `ReceiptHash` | 回执根 |
| `BaseFee()` | `BaseFee` | EIP-1559 基础费用 |
| `ProducerAddress()` | `Coinbase` | 出块者地址（iotex bech32 → eth address） |
| `LogsBloomfilter()` | `Bloom` | 日志布隆过滤器 |

注意事项：
- `selp.ToEthTx()` 对 system actions（如 reward distribution）会失败，转换时跳过
- iotex 地址使用 bech32 编码，需要通过 `address.FromString()` → `.Bytes()` → `common.BytesToAddress()` 转换

### 4.3 blockchain.go 修改

**文件**: `blockchain/blockchain.go`

#### OnBlockchainInit

在 `NewBlockchain()` 中，创建 tracer 后调用。需要构建最小 context 来生成 `*params.ChainConfig`：

```go
if chain.logger != nil && chain.logger.OnBlockchainInit != nil {
    initCtx := genesis.WithGenesisContext(
        protocol.WithBlockchainCtx(context.Background(), protocol.BlockchainCtx{
            ChainID: cfg.ID, EvmNetworkID: cfg.EVMNetworkID,
            GetBlockTime: chain.getBlockTime,
        }), g)
    initCtx = protocol.WithBlockCtx(initCtx, protocol.BlockCtx{
        BlockHeight: 0, BlockTimeStamp: time.Unix(g.Timestamp, 0),
    })
    chainConfig, _ := evm.NewChainConfig(initCtx)
    chain.logger.OnBlockchainInit(chainConfig)
}
```

#### OnClose

在 `Stop()` 中，`lifecycle.OnStop()` 之前调用。

#### Context 注入

在三个入口点将 `bc.logger` 注入 context：

1. `Start()` — `lifecycle.OnStart(ctx)` 之前，用于 genesis 初始化
2. `commitBlock()` — `dao.PutBlock(ctx, blk)` 之前，用于同步区块
3. `MintNewBlock()` — `bbf.Mint(ctx, ...)` 之前，用于出块

### 4.4 OnBlockStart / OnBlockEnd

**文件**: `state/factory/statedb.go` — `PutBlock()` 方法

`PutBlock()` 是唯一持有完整 `blk *block.Block` 的入口，是调用 block 级 hook 的最佳位置：

```
PutBlock(ctx, blk)
  ├── OnBlockStart(ConvertToGethBlock(blk))   ← 最前面
  ├── ws.ValidateBlock / ws.Process
  ├── ws.Commit
  ├── indexer.PutBlock (各 indexer)
  └── OnBlockEnd(nil)                          ← 最后面
```

错误处理：`OnBlockEnd(err)` 在所有 error return 路径上调用，确保 block 生命周期完整。

注意：Mint 路径（`stateDB.Mint()`）产出块但不 commit，最终通过 `CommitBlock` → `PutBlock` 处理，不需要重复调用。

### 4.5 OnTxStart / OnTxEnd

**文件**: `state/factory/workingset.go`

在三个执行路径中添加 hook：

| 方法 | 路径类型 | 说明 |
|------|---------|------|
| `process()` | 正常路径 | 用户 actions 循环 + 系统 actions 循环 |
| `pickAndRunActions()` | Mint 路径 | 从 actpool 拣选 actions + 系统 actions |
| `runActionsLegacy()` | 遗留路径 | 旧版本兼容 |

每个循环中 `ws.runAction()` 前后分别调用：

```go
// OnTxStart: runAction 之前
if hooks != nil && hooks.OnTxStart != nil {
    if ethTx, err := act.ToEthTx(); err == nil {
        hooks.OnTxStart(ethTx, common.BytesToAddress(act.SenderAddress().Bytes()))
    }
}

receipt, err := ws.runAction(actionCtx, act)

// OnTxEnd: runAction 之后（无论成功或失败）
if hooks != nil && hooks.OnTxEnd != nil {
    hooks.OnTxEnd(blockchain.ConvertToGethReceipt(receipt), err)
}
```

注意：`ToEthTx()` 对 system actions 可能失败（非 EthCompatibleAction），此时跳过 OnTxStart。

### 4.6 OnLog

**文件**: `action/protocol/execution/evm/evmstatedbadapter.go` — `AddLog()` 方法

`evmLog` 已经是 `*types.Log`，直接传递，在函数最前面调用以确保捕获所有日志：

```go
func (stateDB *StateDBAdapter) AddLog(evmLog *types.Log) {
    if hooks := protocol.GetPipelineHooksCtx(stateDB.ctx); hooks != nil && hooks.OnLog != nil {
        hooks.OnLog(evmLog)
    }
    // ... existing logic ...
}
```

### 4.7 OnCommit

涉及三个文件协同：

#### 4.7.1 Block 级状态差异收集器

**文件**: `action/protocol/context.go`

通过 context 传递的 block 级别收集器，在 PutBlock 中创建，跨 tx 累积 EVM 状态差异：

```go
type PipelineStateDiffCollector struct {
    Destructs map[common.Hash]struct{}                     // keccak(addr) → 被销毁的账户
    Accounts  map[common.Hash][]byte                       // keccak(addr) → SlimAccountRLP
    Storages  map[common.Hash]map[common.Hash][]byte       // keccak(addr) → keccak(slot) → RLP(value)
    Codes     map[common.Hash][]byte                       // codeHash → bytecode
}
```

#### 4.7.2 Diff 收集时机

**文件**: `action/protocol/execution/evm/evmstatedbadapter.go` — `CommitContracts()` 方法

在 `CommitContracts()` 内部分两阶段收集，解决 per-tx clear 问题：

```
for each contract (非 selfDestructed):
  ① collectPreCommitDiff   — Commit() 前：从 committed map 获取变更的 slot，从 trie 读取新值；收集 dirtyCode
  ② contract.Commit()      — 清除 committed, dirtyCode
  ③ collectAccountState    — Commit() 后：收集 account state（含更新后的 Root）
收集 selfDestructed → destructs
```

关键映射：
- iotex `state.Account` → geth `types.StateAccount` → `types.SlimAccountRLP()`
- 存储值：iotex trie 存原始字节 → RLP 编码 `rlp.EncodeToBytes(common.TrimLeftZeroes(val))`
- 地址/slot key：`crypto.Keccak256Hash()` 哈希

通过 `getInnerContract()` 类型断言（`*contract` 或 `*contractAdapter`）访问未导出字段。

#### 4.7.3 传递给 OnCommit

**文件**: `state/factory/workingset.go` — `Commit()` 方法

```go
if collector := protocol.GetStateDiffCollectorCtx(ctx); collector != nil {
    hooks.OnCommit(common.Hash{}, root,
        collector.Destructs, collector.Accounts, nil,
        collector.Storages, nil, collector.Codes)
}
```

`accountsOrigin` 和 `storagesOrigin` 传 nil（与 bitlayer 一致）。

限制：仅捕获 EVM 相关变更（合约 storage/code/destruct），非 EVM 变更（原生转账、staking 等）暂不采集。

### 4.8 OnGenesisBlock

**文件**: `state/factory/statedb.go` — `createGenesisStates()` 方法末尾

在 genesis 状态写入完成后调用：

```go
if hooks := protocol.GetPipelineHooksCtx(ctx); hooks != nil && hooks.OnGenesisBlock != nil {
    gethBlock := blockchain.BuildGenesisGethBlock(sdb.cfg.Genesis)
    alloc := blockchain.BuildGenesisAlloc(sdb.cfg.Genesis)
    hooks.OnGenesisBlock(gethBlock, alloc)
}
```

## 5. 依赖变更

### 5.1 新增依赖

```
github.com/Chaintable/pipeline  — pipeline tracer 核心库
```

### 5.2 pipeline 模块适配

pipeline 模块原始依赖 bitlayer-l2 的 go-ethereum fork（`StateAccount.Balance` 为 `*big.Int`），而 iotex 的 go-ethereum fork 中 `StateAccount.Balance` 为 `*uint256.Int`。

修改点（`pipeline/tracer/pipeline.go`）：
```go
// 修改前（适配 bitlayer）
Balance: uint256.MustFromBig(account.Balance)

// 修改后（适配 iotex）
Balance: new(uint256.Int).Set(account.Balance)
```

同时更新 pipeline 的 `go.mod` replace 指向 iotex fork：
```
replace github.com/ethereum/go-ethereum => github.com/Chaintable/go-ethereum-iotex v0.0.0-20260210121649-5e2059cfbb3a
```

当前 iotex-core 使用 local replace 指向本地修改后的 pipeline：
```
replace github.com/Chaintable/pipeline => /Users/lihe/ghorg/chaintable/pipeline
```

**TODO**: push pipeline 修改后替换为远程版本。

## 6. 文件清单

| 文件 | 变更类型 | 说明 |
|------|---------|------|
| `action/protocol/context.go` | 修改 | pipeline hooks context key + StateDiffCollector |
| `blockchain/pipeline_convert.go` | 新建 | 类型转换工具函数 |
| `blockchain/blockchain.go` | 修改 | OnBlockchainInit、OnClose、context 注入 |
| `state/factory/statedb.go` | 修改 | OnBlockStart/OnBlockEnd、OnGenesisBlock、创建 collector |
| `state/factory/workingset.go` | 修改 | OnTxStart/OnTxEnd、OnCommit（含 state diff） |
| `action/protocol/execution/evm/evmstatedbadapter.go` | 修改 | OnLog、CommitContracts diff 收集 |
| `go.mod` / `go.sum` | 修改 | 添加 pipeline 依赖 |

外部仓库：

| 文件 | 仓库 | 说明 |
|------|------|------|
| `tracer/pipeline.go` | Chaintable/pipeline | 修复 uint256 兼容性 |
| `go.mod` | Chaintable/pipeline | replace 指向 go-ethereum-iotex |

## 7. 验证方式

1. **编译验证**: `go build -tags nosilkworm -o /dev/null ./server` 通过
2. **go vet**: `go vet -tags nosilkworm ./blockchain/... ./action/protocol/... ./state/factory/...` 无错误
3. **空配置测试**: 不配置 `VMTraceConfig` 时，所有 hooks 为 nil，零开销运行
4. **集成测试**: 配置 `VMTraceConfig` 为有效的 pipeline JSON 配置，启动 standalone 节点，验证 hooks 被正确调用

## 8. 后续优化

1. **非 EVM 状态差异**: 当前仅采集 EVM 合约相关变更，非 EVM 变更（原生转账、staking、rewarding）需拦截 StateManager 层
2. **pipeline replace**: push pipeline 修改后替换 local replace 为远程版本
3. **性能监控**: 添加 prometheus metrics 监控 hook 调用延迟
