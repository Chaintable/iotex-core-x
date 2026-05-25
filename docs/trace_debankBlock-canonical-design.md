# trace_debankBlock 新方案设计文档（canonical state_diff + 双源 events/traces）

> 版本：v3-canonical-draft
> 日期：2026-04-26
> 取代：旧的「按需重放」单一真相源设计（state_diff/events 切到 Erigon canonical 来源）
> 动机：见同目录 `replay-vs-mainchain-divergence-analysis.md`

---

## 1. 背景与动机

旧方案以「按需重放」为单一真相源——每次调用 RPC 在 archive 上重放该块，从 EVM tracer + workingset hook 双通道捕获 state_diff 和 events。这条路径有两个无法用打补丁解决的根本问题：

### 1.1 跨版本 EVM 漂移（pre-Sumatra）

主网生产 block N 时跑的是 v1.x 的 iotex-core，archive 节点跑 v2.3.8。两者之间隔了几百次 commit、十多次 hardfork、go-ethereum 库升级、自研 feature flag。结果：约 **0.7% 的 pre-Sumatra 块**至少有一笔 tx 的 replay status 与主网历史 receipt 不一致（典型例子：block 12,496,844 Unifi LP 部署，主网 `pair.mint()` 在 215,251 gas 内成功，replay 中 OOG 失败）。

详见 `replay-vs-mainchain-divergence-analysis.md` §2-§3。

### 1.2 跨纪元语义漂移（pre-fork 时代）

实测（在 x3 archive 上）发现：**几乎所有 pre-zero-nonce-fork（≲ 30M）的块**，replay state_diff 的 nonce 系统性偏离 canonical：
- replay 用 tip-context 的 `useZeroNonceForFreshAccount=true` 跑历史块 → fresh account nonce=0
- writer 当年提交时用 legacy 语义 → fresh account nonce=1
- `eth_getTransactionCount(addr, h)` 读 canonical Erigon 返回 1（与 writer 当年一致）
- replay state_diff 输出 0
- → **设计目标「state_diff.Nonce == eth_getTransactionCount」在早期块上没达成**

### 1.3 已修过的 replay-only bug 不下十几个

`be0e31e7 / 9757f5a1 / dc9cd4f9 / 6b716253 / 11d7923e / dd6b6162 / f8104506 / 8f65be274 / 5ffd88546 / 1c8bf3039 / acca119cc / c61369c18 / d0ed38815 / Bug A / Bug B`——每个 bug 都是「在 leafage 和 native 之间发现一个不一致 → 排查 → 加 fix」。这是结构性的副作用累积，不是一两次意外。

### 1.4 后置代价

- `Simulate` 模式的 5 处 fault-tolerance fallback（CreatePreStates / runAction / finalize 等）本身就是 replay 路径在历史块上不稳定的症状。每多一处容错 = 多一处「数据可能静默缺失」的入口。
- 每块 RPC = 完整重放 N 笔 action，全链 backfill 等于把链跑两遍。

### 1.5 实测结论

经 x3 上多块端到端核对，canonical 路径（直接读 Erigon `AccountChangeSet/StorageChangeSet/PlainState/kv.Code`）：
- 在 **post-Sumatra 正常块**上与 replay state_diff 完全一致（52/53 在 4 块抽样里），唯 1 mismatch 是 canonical 修正了 replay 的 0.8 IOTX 偏差
- 在 **pre-Sumatra 分叉块**（12,496,844）上修正了 replay 的 30 IOTX 错误
- 在 **pre-fork 早期块**上修正了 replay 的 nonce 系统性错误
- 与 native `eth_getBalance / eth_getTransactionCount / eth_getCode` 完全一致

详见 `replay-vs-mainchain-divergence-analysis.md` §6 + 本次 strict evaluation 实测数据。

---

## 2. 目标 / 非目标

### 目标

1. **state_diff** 切到 canonical 路径，完全消除 replay drift 对下游 leafage state 的污染
2. **events** 切到 canonical 路径（直接从历史 receipts 派生），与 `eth_getTransactionReceipt` 严格一致
3. **traces** 保留 replay 路径但标记为 best-effort，不再影响 state_diff/events 正确性
4. 全部其他字段（block / header / txs / storage_contracts / validation_hash / error_events / error_traces）功能完整保留
5. RLP 编码 schema 不变，leafage 端无需改造

### 非目标

1. 不改 trace 的内部结构（call tree 仍由 replay 产生）
2. 不试图修复 0.7% 的 trace divergence（标 metric 即可）
3. 不处理 protocol pool balance（`addProtocolPoolSyntheticAccounts` 直接删除）
4. 不动现有 EVM 重放逻辑（writer 的同步路径完全不动）
5. 不改 `eth_getBalance` 等 native iotex web3 路径

---

## 3. 顶层架构

### 3.1 数据来源拆分

| 输出字段 | 旧来源 | 新来源 | 准确性 |
|---|---|---|---|
| `state_diff.NewAccounts` | replay collector | Erigon `AccountChangeSet[N]` + `NewPlainState(tx, N+1)` | canonical |
| `state_diff.DeletedAccounts` | replay collector | 同上（post-state 为 nil 的 changed addr） | canonical |
| `state_diff.StorageDiff` | replay collector + EVM hook | Erigon `StorageChangeSet[N]` + `NewPlainState(tx, N+1)` | canonical |
| `state_diff.NewCodes` | replay collector | Erigon `kv.Code` 按 changed addr 的 codeHash 拉 | canonical |
| `state_diff.{Hash, ParentHash}` | `blk.DeltaStateDigest()` | 同上（不变） | canonical |
| `block_file.events` | replay tracer hooks | `dao.GetReceipts(N)` → `emitTransferLogsAsEvents` | canonical |
| `block_file.traces` | replay tracer | replay tracer（分叉 tx 替换为合成 minimal trace；divergence 仅 metric+log，不进 schema） | best-effort |
| `block_file.error_events` | replay tracer | replay tracer | best-effort |
| `block_file.error_traces` | replay tracer | replay tracer | best-effort |
| `block_file.storage_contracts` | replay-derived | canonical `StorageChangeSet[N]` 的 addr 集 | canonical |
| `block_file.txs` | block body | block body（不变） | canonical |
| `block_file.block` | block header/body meta | 同上（不变） | canonical |
| `header` | block header | 同上（不变） | canonical |
| `validation_hash` | RLP hash | RLP hash（不变） | canonical |

### 3.2 路径解耦

- canonical 路径：每块都跑（独立于 replay 是否分叉/失败）
- replay 路径：仍然跑（产生 traces 和 error_events/error_traces），可单独失败而不阻塞主路径
- **两条路径不共享任何 capture hook**——`PipelineStateDiffCollector` 的产物不再写入主 state_diff，仅供 debug 对照

### 3.3 数据流

```
                                ┌──────────────────────────┐
                                │  ErigonDB (RoTx)         │
                                │  ─ AccountChangeSet[N]   │
                                │  ─ StorageChangeSet[N]   │
                                │  ─ NewPlainState(N+1)    │
                                │  ─ kv.Code               │
                                └────────────┬─────────────┘
                                             ↓
                              ┌──────────────────────────────┐
                              │ buildCanonicalStateDiff(N)   │ ← 主路径
                              │ → BlockStorageDiff (5 桶)    │
                              └──────────────┬───────────────┘
                                             ↓ merge
┌──────────────────┐                ┌────────────────────────┐
│ dao.GetReceipts  │ ─receipts─→    │   buildCanonicalEvents │ ← 主路径
│        (N)       │                │   → []Event            │
└──────────────────┘                └──────────┬─────────────┘
                                               ↓ merge
                              ┌──────────────────────────────┐
                              │     replay (best-effort)     │ ← 旁路
                              │  → traces, error_events,     │
                              │    error_traces, replay      │
                              │    receipt status            │
                              └──────────────┬───────────────┘
                                             ↓
                              ┌──────────────────────────────┐
                              │ DebankOutPut (assemble)      │
                              │  validation_hash             │
                              └──────────────┬───────────────┘
                                             ↓
                                          → RPC 返回
```

---

## 4. 详细设计

### 4.1 Canonical state_diff builder

新文件：`api/debank_canonical_diff.go`

```go
// buildCanonicalStateDiff 从 Erigon changeset + post-state 构造整块 state_diff
//
// 前置条件：core.bc.TipHeight() >= height + 1（block N+1 已 commit）
//
// 步骤：
//   1. 用 erigonDB BeginRo 拿 read-only tx
//   2. ForRange(tx, kv.AccountChangeSet, height, height+1) 收集本块 changed addr 集
//   3. ForRange(tx, kv.StorageChangeSet, height, height+1) 收集本块 (addr, slot) 集
//   4. NewPlainState(tx, height+1, nil) 作为 post-state reader
//   5. 对每个 changed addr：
//      ─ ReadAccountData → nil → DeletedAccounts
//      ─ ReadAccountData → acc → NewAccounts{addr_hash, Nonce, Balance, CodeHash}
//   6. 对每个 changed (addr, slot)：
//      ─ ReadAccountStorage → 32-byte left-padded value
//      ─ 加入 StorageDiff[addr_hash].Values[slot_hash → value]
//   7. 对每个 NewAccounts 中 codeHash 与 (height-time) codeHash 不同的：
//      ─ tx.GetOne(kv.Code, codeHash) → bytecode
//      ─ 加入 NewCodes
//   8. 输出 BlockStorageDiff{
//        Hash:       blk(N).DeltaStateDigest(),
//        ParentHash: blk(N-1).DeltaStateDigest()（或 GenesisStateRoot for N==1）,
//        NewAccounts, DeletedAccounts, StorageDiff, NewCodes,
//      }
func buildCanonicalStateDiff(
    ctx context.Context,
    dao blockdao.BlockDAO,
    erigonDB *erigonstore.ErigonDB,
    height uint64,
) (*ptypes.BlockStorageDiff, error)
```

#### 关键实现点

1. **off-by-one**：Erigon `NewPlainState(tx, blockNr).ReadAccountData()` 返回的是「block blockNr 应用之前」的状态。要拿 block N 结束后的状态，传 `height+1`。已在 strict evaluation 上用 block 32,670,059 的 `0x36f7feee...` 反向验证（`NewPlainState(tx, 32670060).ReadAccountCode` 返回 1159 bytes，与 `eth_getCode(addr, 32670059)` 一致）。
2. **deleted account 检测**：`AccountChangeSet[N]` 有 entry，但 `NewPlainState(tx, N+1).ReadAccountData(addr)` 返回 nil，则该 addr 在本块被 selfdestruct（路径标准 Erigon 语义；缺历史样本，需补单测验证）。
3. **storage value padding**：Erigon `kv.PlainState` 存的是去掉前导零后的 byte 串。leafage 期望 `*uint256.Int`。`uint256.NewInt(0).SetBytes(rawValue)` 处理 padding。
4. **codeHash → code lookup**：`kv.Code` 表的 key 是 codeHash（32 bytes）。仅对 NewAccounts 中 codeHash != emptyCodeHash 且与 N-1 时不同的 addr 拉一次（避免重复传 bytecode）。
5. **storage 的 incarnation**：Erigon storage key 是 `addr || incarnation || slot` 复合形式。普通合约 incarnation=1，selfdestruct 后重新部署会增加。从 post-state account.Incarnation 拿。

### 4.2 Canonical events builder

新函数（重构现有 `emitTransferLogsAsEvents` 而来）：

```go
// buildCanonicalEvents 从历史 receipts 派生 events（与 eth_getTransactionReceipt 一致）。
//
// 仅填 contract_id / selector / topics / data / log_index 五个字段。trace 归因字段
// (parent_trace_id / pos_in_parent_trace / id) 留 Go 零值——leafage RPC 在
// build_trace_node 内基于本地 Reth CallTraceNode 重新派生（覆盖写入端值）。
func buildCanonicalEvents(receipts []*action.Receipt, txIDs []string) []ptypes.Event
```

#### 数据来源：直接读历史 receipts，**完全不 replay**

```
core.dao.GetReceipts(height)         ← blockchain/blockdao/blockdao.go:302
   ├─ blockStore.GetReceipts(N)      ← proto 反序列化，填 r.logs（EVM LOG opcodes）
   ├─ blockStore.TransactionLogs(N)  ← 从独立 sysStore 读 BlkTransactionLog
   └─ r.AddTransactionLogs(...)      ← 按 ActionHash 合并入 r.transactionLogs
   ↓
返回的 *action.Receipt 同时持有：
   r.Logs()            = EVM LOG opcodes（持久化在 receipt proto）
   r.TransactionLogs() = iotex 协议层流水（在 sysStore）
```

**两份物理来源都是 writer 当年同步该块时持久化的真值**——和 native `eth_getTransactionReceipt` 读的是同一份数据。新路径不开 EVM tracer、不调用 `WorkingSetAtTransaction`、不依赖任何 replay 输出。

#### 输出生成

每个 receipt 产生：
- `r.Logs()`（EVM LOG opcodes）→ ETH-style log，emitter = `iotexAddress.FromString(l.Address)` 转 ETH hex
- `r.TransactionLogs()` 经 `r.TransferLogs(account.ProtocolAddr().String(), 0)` 转合成 log → emitter 统一为 `account.ProtocolAddr()` (`0xCCD3d8...`)，编码 `IN_CONTRACT_TRANSFER / GAS_FEE / GRANT_REWARD / BUCKET_CREATE_AMOUNT / CLAIM` 等

两份不重叠（一份 EVM-only proto 持久化、一份协议层 sysStore 持久化），同时遍历不会双发。

**对所有 receipt 都 emit 这两类**——不再按 action type 区分（旧方案 `includeEVMLogs=true/false` 区分逻辑作废，因为 canonical receipt 已经是「主网当时实际看到的最终 logs」）。

#### 与 native `eth_getTransactionReceipt` 的等价性

native web3 在 `web3server_marshal.go:374` 用 `append(receipt.Logs(), transferEvents...)` 拼接同样两份 log（其中 `transferEvents = receipt.TransferLogs(account.ProtocolAddr(), logIndex)`）。新 builder 走完全相同的两份+拼接逻辑，仅差异是全局 LogIndex 编号方式。**结果集合（emitter / topics / data）完全一致**。

**Trace binding 字段不需要写入端填**：
经 leafage 源码确认（`crates/leafage-evm-rpc/src/api_impl/utils.rs:215-250` 的 `build_trace_node`），下游服务 `eth_getDebankBlock` 时基于本地 Reth `CallTraceNode` 树**重新计算** `parent_trace_id` / `pos_in_parent_trace` / `id`，覆盖写入端值。写入端没有 call frame 信息（receipts 是扁平 logs 列表），硬填只会引入误导性归因，简单留零值即可。

#### Events 数量对照（已实测）

x3 上 block 47487933 实测：

| 来源 | tx[0] | tx[1] | total |
|---|---|---|---|
| `eth_getTransactionReceipt` (native canonical) | 9 (= `receipt.Logs()` 5 + `transferEvents` 4) | 1 | **10** |
| 当前 trace_debankBlock（replay 路径，681f1554 部署） | 13 (含 4 个 zero-addr 错发) | 1 | 14 |
| 新 `buildCanonicalEvents`（dao.GetReceipts 派生） | 9 | 1 | **10** ✓ |

**结论**：新 canonical 路径的 events 数量与 native `eth_getTransactionReceipt` 严格一致。比当前 trace_debankBlock 少 4 个——那 4 个是 681f1554 deploy 上的已知 replay bug（同一笔内部 transfer 被以 zero-addr 格式额外发送一次）。f8104506 修过这个 bug 但 x3 没部署。新方案直接对齐 native truth，**消除该 bug**。

下游若依赖 14 这个数量，其实是依赖 bug——切 canonical 后看到 10 是修复，不是 regression。

### 4.3 Tx 字段覆盖 + 分叉 tx 的 trace 替换

`block_file.txs[i]` 由 replay tracer 在 `BuildPipelineTransaction` 中填充，其中 `Status` 和 `GasUsed` 来自 **replay receipt**——分叉 tx 上这两个字段会和主网不一致。同样 `traces` / `error_traces` / `error_events` 是 replay 调用树产物，分叉 tx 上整棵树都错。

新增后处理 `overrideTxsAndStripDivergedTraces` (`api/debank_canonical_override.go`)：

1. **`tx.Status` / `tx.GasUsed` 全部从 canonical receipt 覆盖**（每个 tx 都覆盖，不仅分叉——replay gas 在 status 一致时仍可能微漂）
2. **状态分叉的 tx**：
   - **drop** 其 `traces` / `error_traces`（按 `TxID` 匹配），并删除指向被删 trace 的 `error_events`（按 `ParentTraceID` 匹配）
   - **append** 一条**合成的 canonical-minimal trace**：单 frame、`from/to/value/input/gas/gasUsed/status/error` 全部用主网真值，`Subtraces=0`，`Output=空`（iotex receipt 不暴露返回数据）。CREATE tx 出 `CallCreateType="create"`、否则 `"call"`。按 canonical status 决定路由到 `Traces` 还是 `ErrorTraces`
3. **主 events 不动**：已是 canonical receipt 派生，永远对

合成 trace 的依据：每个 tx 至少有一条 trace 是下游可能依赖的不变量（leafage 的 trace 树构建假设每 tx ≥1 frame）。空 drop 会破坏这个不变量；replay 的错 trace 又会误导内部资金归因。合成的 minimal trace 让顶层数据全 canonical 正确，缺的只是「内部调用层级」——这部分原本 receipt 里也没有。

#### 行为对照表

| 场景 | tx.Status | tx.GasUsed | events | traces | error_traces | error_events |
|---|---|---|---|---|---|---|
| 状态一致 + gas 一致 | canonical | canonical | canonical | replay 保留（best-effort）| replay 保留 | replay 保留 |
| 状态一致 + gas 微漂移 | canonical | **canonical 修正** | canonical | replay 保留（best-effort）| replay 保留 | replay 保留 |
| replay-FAIL canonical-SUCCESS | true | canonical | canonical（有内容）| **drop replay + append synth** | drop replay | drop orphan events |
| replay-SUCCESS canonical-FAIL | false | canonical | canonical（空）| drop replay | **drop replay (空) + append synth** | drop replay (空)|

下游 ETL 看到的语义：分叉 tx 仍有 trace 条目（顶层 from/to/value/status 全对），events 是 canonical。缺的只是内部调用细节——下游若用此做内部资金归因，分叉块上会缺数据，但顶层归因不会错。

**Trace 内部一致性的固有局限**：即使 status 一致的非分叉 tx，replay trace 内部仍可能与 canonical 漂移：
- 子调用成功/失败翻转（被父 try/catch 兜住，顶层一致）
- LOG opcode 触发数（少量场景）
- gas 微漂、CALL value 计算偏差（如 47,487,940 上 0xa576c141 的 0.8 IOTX）

这些不在 traces 字段里被检测/修复——以「重新跑一遍 EVM 拿真 trace」为代价不划算。**资金/状态正确性由 state_diff + events（canonical）保证**；trace 仅作为顶层结构 + 内部调用「best-effort」的辅助信号。下游应当：
- **state correctness** → 用 state_diff
- **token transfer detection** → 优先 events（覆盖 ERC20/ERC721/合约 emit），traces 仅作为不发 event 的老式 token 的补充
- **internal call analysis** → traces，但接受 < 1% 块上数据缺失/略有偏差

### 4.4 Traces (replay, best-effort)

保留现有 replay 路径，做以下调整：

1. **去掉 state_diff capture**：
   - `evm.TracerContext.CaptureStateDiff` 仍然走（因为 callTracer 内部用），但产物**不再合入 finalAccounts/finalStorages/finalCodes**
   - `evm.TracerContext.EmitTransferLogs` 仍然 fire（因为 callTracer 把 logs 挂到 trace 树上），但**不再写到主 events 数组**——主 events 已由 §4.2 给出
2. **跑完 replay 后比对 receipt status**：
   ```go
   for i, replayReceipt := range replayReceipts {
       if replayReceipt.Status != historyReceipts[i].Status {
           traces[i].ReplayDiverged = true
           traceMetrics.diverged_tx_total.Inc()
       }
   }
   ```
3. **replay 整体失败的处理**：
   - panic 通过现有 lazy views nil-safe + simulate fallback 拦下
   - 不可恢复的失败：log + metric，traces 字段填 `[]`（空列表），不影响主输出

### 4.5 Storage contracts

```go
// 从 canonical StorageChangeSet[N] 派生
storageContracts := make(map[common.Address]struct{})
ForRange(tx, kv.StorageChangeSet, height, height+1, func(blockN uint64, k, v []byte) error {
    addr := common.BytesToAddress(k[:length.Addr])
    storageContracts[addr] = struct{}{}
    return nil
})
// 输出 sorted([]common.Address) 的字符串列表
```

### 4.6 Tip-1 滞后处理

canonical 要求 `NewPlainState(tx, height+1)` 的 height+1 changeset 已落盘。

```go
if core.bc.TipHeight() < height + 1 {
    return nil, errors.Errorf(
        "block %d not yet finalized for canonical state_diff (tip=%d, need >= %d)",
        height, core.bc.TipHeight(), height+1,
    )
}
```

ETL 端配套（`background-tracer`）：
- 如收到此错误，等待 `block_time + grace`（约 5-10 秒）后重试
- 实际生产：ETL 通常落后 tip 1-2 块，自然不会触发

### 4.7 Genesis (height=0) 特殊处理

不变：保留现有 `buildGenesisDebankOutput(g)` 早期返回（`coreservice.go:2369-2371`）。block 0 的状态从 genesis config 派生，不进 canonical 路径。

### 4.8 Pool 合成账户：保留并修正地址

旧 `addProtocolPoolSyntheticAccounts` 用的合成地址是 `address.RewardingProtocolAddrHash`（= `0x1b4f3289...` = `hash160("rewarding")`）和 `address.StakingProtocolAddrHash`（= `0xda39d866...`）。**实测发现这两个地址在 native iotex `eth_getBalance` 上返回 0**——iotex-native 实际路由的 pool 地址是 EVM 风格的 `keccak256(<name>)[12:]`：

| 资源 | EVM-style addr | 含义 |
|---|---|---|
| 奖励池 | `0xa576c141e5659137ddda4223d209d4744b2106be` | `keccak256("rewarding")[12:]` |
| 质押池 | `0x04c22afae6a03438b8fed74cb1cf441168df3f12` | `keccak256("staking")[12:]` |

新设计：在新文件 `api/debank_canonical_pool.go` 中实现 `appendProtocolPoolSyntheticAccounts`，每个非 genesis 块都注入这两个地址的 tip-snapshot 余额，与 native eth_getBalance 严格对齐。

#### 关键实现细节

1. **地址来源**：`crypto.Keccak256([]byte("rewarding"))[12:]` 和 `crypto.Keccak256([]byte("staking"))[12:]`，在 `init()` 中预计算
2. **balance 来源**：`rewarding.Protocol.TotalBalance(tipCtx, sf)` / `staking.ReadState(TOTAL_STAKING_AMOUNT)`，与 `getProtocolAccount` 内部用同一套
3. **冲突处理**：rewarding 这个地址同时被 GrantReward 通过 `accountstorage.Store` 写入 PlainState（每块都写）。canonical reader 会自然产出一个历史精确值的条目；synthesis 必须**覆盖**它（用 `upsertNewAccount` 替换同 addr_hash 的 entry），保持 tip-only 语义和 native 对齐
4. **failure tolerance**：保留 panic recovery（pre-Greenland 块上协议读取可能 panic），失败时 silently skip——leafage 上回退到「在该块未更新 pool 余额」的状态
5. **tip-context 构造**：`bc.Context()` + `tipHeight` + `WithFeatureCtx`——这些 flag 决定 rewarding fund v1/v2 layout 的选择，与 legacy 一致

#### 行为对照

| 查询 | iotex-native eth_getBalance | 旧 leafage（未修正前） | 新 leafage（带 synthesis） |
|---|---|---|---|
| `0xa576c141 @ h_old` | tip pool balance | tip pool balance（GrantReward 持续写入）| tip pool balance（synthesis override）|
| `0xa576c141 @ h_recent` | tip pool balance | tip pool balance | tip pool balance |
| `0x04c22afa @ h_old` | tip pool balance | **0**（不在 PlainState）| tip pool balance（synthesis 注入）|
| `0x04c22afa @ h_recent` | tip pool balance | 0 | tip pool balance |
| `0x1b4f3289 @ any h` | 0 | 0 | 0（无写入）|
| `0xda39d866 @ any h` | 0 | 0 | 0（无写入）|

**用户期望**：leafage 完全镜像 native iotex 行为——pool 地址返回最新值。新设计已对齐。

### 4.9 Replay 分叉 metric

新增 prometheus counter：

| metric | 类型 | label |
|---|---|---|
| `trace_debankblock_canonical_duration_seconds` | histogram | - |
| `trace_debankblock_replay_duration_seconds` | histogram | - |
| `trace_debankblock_replay_diverged_blocks_total` | counter | - |
| `trace_debankblock_replay_diverged_tx_total` | counter | - |
| `trace_debankblock_replay_failed_blocks_total` | counter | - |
| `trace_debankblock_canonical_history_unavailable_total` | counter | - |

---

## 5. 文件 / 接口变化

### 5.1 新增文件

| 路径 | 内容 |
|---|---|
| `state/factory/erigonstore/canonical_reader.go` | erigonDB 上的 changeset 遍历 + post-state 读取 helper |
| `api/debank_canonical_diff.go` | `buildCanonicalStateDiff(ctx, dao, erigonDB, height)` |
| `api/debank_canonical_events.go` | `buildCanonicalEvents(receipts, actions, replayBindings)` |
| `api/debank_canonical_diff_test.go` | 单测 |
| `api/debank_canonical_events_test.go` | 单测 |

### 5.2 新增 API（erigonstore 层）

```go
// canonical_reader.go

type CanonicalReader struct {
    db *ErigonDB
}

func (db *ErigonDB) NewCanonicalReader() *CanonicalReader { return &CanonicalReader{db: db} }

// 块 N 内变过的所有 account 地址
func (r *CanonicalReader) ChangedAccounts(ctx context.Context, height uint64) ([]common.Address, error)

// 块 N 内变过的所有 (addr, incarnation, slot) tuples
func (r *CanonicalReader) ChangedStorages(ctx context.Context, height uint64) ([]StorageKey, error)

// 块 N 结束后 addr 的 account state（nil 表示已 selfdestruct）
func (r *CanonicalReader) AccountAt(ctx context.Context, height uint64, addr common.Address) (*accounts.Account, error)

// 块 N 结束后 (addr, slot) 的 storage value（去掉前导零的原始字节）
func (r *CanonicalReader) StorageAt(ctx context.Context, height uint64, addr common.Address, incarnation uint64, slot common.Hash) ([]byte, error)

// codeHash → bytecode
func (r *CanonicalReader) CodeByHash(ctx context.Context, codeHash common.Hash) ([]byte, error)

// 历史下界（writer pruning 后的最早可查 block）
func (r *CanonicalReader) AvailableFrom(ctx context.Context) (uint64, error)
```

实现内部统一在一个 `BeginRo` tx 里完成（避免每个 call 开关 tx）；接口外部一次 call 给一个 height 就够。

### 5.3 修改文件

#### `api/coreservice.go`

`debankBlockImpl` 重构（line 2365-2538）：

```go
func (core *coreService) debankBlockImpl(ctx context.Context, height uint64, debug bool) (*ptypes.DebankOutPut, error) {
    g := core.bc.Genesis()
    if height == 0 {
        return buildGenesisDebankOutput(g)
    }
    if core.bc.TipHeight() < height+1 {
        return nil, ErrCanonicalNotFinalized
    }

    blk, err := core.dao.GetBlockByHeight(height)
    if err != nil { return nil, err }

    // ─── 主路径 1: canonical state_diff ─────────────────────────────
    stateDiff, err := buildCanonicalStateDiff(ctx, core.dao, core.erigonDB, height)
    if err != nil {
        return nil, errors.Wrap(err, "canonical state_diff")
    }

    // ─── 主路径 2: canonical events ─────────────────────────────────
    receipts, err := core.dao.GetReceipts(height)
    if err != nil { return nil, err }

    // ─── 旁路: replay (best-effort) ─────────────────────────────────
    replayBindings, traces, errEvents, errTraces, replayDivergedTxs := core.runReplayBestEffort(ctx, blk)
    // 即使 replay 失败上述变量为 zero-value, 不影响主输出

    events := buildCanonicalEvents(receipts, blk.Actions, replayBindings)

    // ─── 装配输出 ───────────────────────────────────────────────────
    return assembleDebankOutput(
        blk, stateDiff, events, traces, errEvents, errTraces, replayDivergedTxs,
    ), nil
}
```

删除：
- `addProtocolPoolSyntheticAccounts` 整个函数（line 2540-2617）
- `addSyntheticAccount` (line 2619-2636)
- `readStakingTotalAmount` (line 2638-2670)
- 所有相关 import

#### `api/api_debank.go`

- `emitTransferLogsAsEvents` 改名 / 拆分为 `convertReceiptToEvents(receipt, actionHash, txIdx)` 纯函数版本，不再向 rpcTracer emit
- 保留 `convertActionReceiptToGethReceipt` 用于 callTracer

#### `state/factory/erigonstore/workingsetstore_erigon.go`

`ErigonDB` 暴露 `RoTx(ctx)` 用于外部 read-only 访问（或者 `View(ctx, fn)` pattern）。

#### `state/factory/statedb.go`

`stateDB` 把 `erigonDB` 暴露给 coreservice（已经存在 sdb.erigonDB 字段，加 getter）。

### 5.4 删除代码

| 位置 | 内容 | 行数 |
|---|---|---|
| `coreservice.go:2540-2617` | `addProtocolPoolSyntheticAccounts` | 78 |
| `coreservice.go:2619-2636` | `addSyntheticAccount` | 18 |
| `coreservice.go:2638-2670` | `readStakingTotalAmount` | 33 |
| `coreservice.go:2475-2495` | pool ctx 构造 + 调用 | 21 |
| `coreservice.go:2453-2473` | PipelineStateDiffCollector 写入主输出的 merge 逻辑 | 21 |
| 总计删除 | | ~170 行 |

### 5.5 保留代码（不再是主路径但仍需）

| 位置 | 用途（保留原因） |
|---|---|
| `state/factory/workingset.go` Simulate fallback | replay best-effort 模式仍可能命中（不再影响主输出，但保留容错防止 trace 失败拖累） |
| `action/protocol/lazyviews.go` nil-safe | 同上 |
| `state/factory/statedb.go:289` lazy views init with sdb | 同上 |
| `PipelineStateDiffCollector` | replay path 内部仍然消费它的 Snapshot/Revert 逻辑；产物不导出但仍需 |

---

## 6. 数据 schema 兼容性

### 6.1 leafage 端消费的 RLP 格式（不变）

```go
// pipeline/types/state_diff.go
type BlockStorageDiff struct {
    Hash, ParentHash common.Hash
    NewAccounts      []NewAccount
    DeletedAccounts  []common.Hash
    StorageDiff      []AccountStorageDiff
    NewCodes         []NewCode
}

type NewAccount struct {
    Address  common.Hash    // = keccak256(eth_addr_bytes)
    Balance  *uint256.Int
    Nonce    uint64
    CodeHash common.Hash    // = keccak256(code), or emptyCodeHash for EOA
}

type AccountStorageDiff struct {
    Address common.Hash
    Values  []IndexValuePair
}

type IndexValuePair struct {
    Index common.Hash       // = keccak256(slot_bytes)
    Value *uint256.Int
}

type NewCode struct {
    CodeHash common.Hash
    Code     []byte
}
```

### 6.2 编码细节（与现有完全一致）

- `Address` = `keccak256(20-byte eth addr)`
- `Index` = `keccak256(32-byte slot key)`
- `Balance` / `Value` = `uint256.NewInt(0).SetBytes(stripped_zero_prefix_bytes)`
- `Nonce` = post-block account.Nonce（Erigon 直接存的值，无需 +/- 1 转换；写入端 `useZeroNonceForFreshAccount` 已经在 commit 时按 fork 高度切换了 legacy/zero-nonce 语义）
- `CodeHash` = post-block account.CodeHash；EOA 是 `0xc5d2460186...`（keccak256(empty)），`SlimAccountRLP` 编码时 EOA 的 code_hash 字段会保留这个值或 nil（与现状对齐）
- DeletedAccounts 元素直接是 addr_hash（不带 account 内容）

### 6.3 leafage MPT 链式校验

`Hash` 和 `ParentHash` 仍来自 block header `DeltaStateDigest()`。leafage 的「前一块 NewRoot == 本块 ParentRoot」校验逻辑不变。

---

## 7. 行为差异对照表

| 场景 | 旧 replay 行为 | 新 canonical 行为 |
|---|---|---|
| post-Sumatra 正常块（绝大多数） | 与主网一致 | 与主网一致（实测全 match） |
| post-Sumatra 微小 drift 块（如 47487940 上 0xa576c141） | 偏离主网 ~0.8 IOTX | 与主网一致（自动 fix） |
| pre-Sumatra 正常块 | 与主网一致 | 与主网一致 |
| pre-Sumatra 分叉块（如 12,496,844） | 严重偏离（30+ IOTX 错误，整组 state 错） | 与主网一致 |
| pre-fork 时代块（< ~30M）涉及 fresh account | nonce 系统性 +1 错（legacy 应=1，replay 输 0） | 与 `eth_getTransactionCount` 一致 |
| Bug A 复现块 | 旧 trace_debankBlock 输出 -1 偏差 | 与 `eth_getTransactionCount` 一致（实测当前 writer 已修） |
| Bug B 复现块 | 旧 trace_debankBlock 输出 +1 偏差 | 与 `eth_getTransactionCount` 一致 |
| pool 余额 (rewardingPool / stakingPool) | tip-snapshot 合成（地址用错的 hash160，native eth_getBalance 返 0） | tip-snapshot 合成（地址用 keccak[12:]，与 native eth_getBalance 一致） |
| trace 在分叉块 | 静默错误（误导下游） | 替换为合成 canonical-minimal trace；divergence 走 metric + log，不在 RPC 响应里显式标记 |
| events 在分叉块 | 跟随 replay 错误 | 来自 receipt，永远与主网一致 |
| Erigon history 不可用（pruning 后的 block） | replay 仍可跑（自给自足） | 报错（generic JSON-RPC Internal）→ ETL 自动重试 → 持续失败阻塞 pipeline 触发人工介入 |

---

## 8. 测试与验证计划

### 8.1 单元测试

| 测试 | 验证点 |
|---|---|
| `TestCanonicalStateDiff_NewAccount` | EOA 收 IOTX → NewAccounts 含正确 balance/nonce/empty codeHash |
| `TestCanonicalStateDiff_ContractDeploy` | EVM CREATE → NewAccounts + NewCodes + 初始 StorageDiff 全部正确，codeHash 与 NewCode.CodeHash 一致 |
| `TestCanonicalStateDiff_Selfdestruct` | 合成 EVM SELFDESTRUCT → DeletedAccounts 包含目标 addr，NewAccounts 不含 |
| `TestCanonicalStateDiff_StorageMutation` | EVM SSTORE → StorageDiff[contract].Values 含正确 (slot_hash, value) |
| `TestCanonicalStateDiff_EmptyBlock` | 仅 system action 的块 → 5 桶都正确（可能为空） |
| `TestCanonicalStateDiff_FreshLegacyAccount` | pre-fork 时代 fresh EOA → nonce=1 (legacy semantics) |
| `TestCanonicalStateDiff_FreshZeroNonceAccount` | post-fork 时代 fresh EOA → nonce=0 |
| `TestCanonicalStateDiff_TipNotFinalized` | tip < height+1 → 返回 `ErrCanonicalNotFinalized` |
| `TestCanonicalEvents_FromEVMLogs` | LOG opcode 产物 → events 含正确 emitter / topics / data |
| `TestCanonicalEvents_FromTransactionLogs` | TransactionLog → synthetic event with emitter=account.ProtocolAddr |
| `TestCanonicalEvents_TraceBindingPresent` | replay 成功 → events 含 parent_trace_id / pos_in_parent_trace |
| `TestCanonicalEvents_TraceBindingAbsent` | replay 失败 → events 仍正确，trace binding 为空 |

### 8.2 x3 集成测试

抽样 100 块覆盖：
- pre-Sumatra 正常块（10 块）
- pre-Sumatra 分叉块（5 块，包含 12,496,844）
- pre-fork 早期块（10 块，h ∈ [1, 10M]）
- post-Sumatra 正常块（70 块，均匀分布；包含 47,487,940 — `0xa576c141` 已知 0.8 IOTX 漂移样本）
- 包含 CREATE 部署的块（5 块，已知 32,670,059）

#### `0xa576c141` 专项覆盖

`verify_canonical_block.py` 对每块单独追踪 `0xa576c141`（= `keccak256("rewarding")[12:]`，rewarding action 的 EVM-style 路由地址）。该 addr 由 GrantReward handler 通过 `accountstorage.Store` → `intraBlockState.SetBalance` 写入 PlainState，**每非 genesis 块都应当出现**在 canonical state_diff 里（既不是池合成账户、不在过滤列表中）。

每块都比较：
- `present_in_state_diff`：state_diff 中是否包含该 addr
- `match`：state_diff 的 (balance, nonce, codeHash) 是否与 `eth_getBalance/Nonce/Code` 完全一致
- 若 addr 缺席但 `eth_getBalance(N) ≠ eth_getBalance(N-1)` → 标 `BUG`（canonical 漏写）

预期结果：
- 新 writer 在所有 rewarding 活动块上 `present=yes ∧ match=✓`
- 老 writer 在 47,487,940 上 `match=✗`（已知 0.8 IOTX 偏差，canonical 是对的）

报告输出有专门一节 `## 0xa576c141 (rewarding-EVM routing addr) coverage`，按块输出 new/old 两侧的命中状态表格。

#### 每块通用对账

```python
# pseudo-code
for h in sample_heights:
    new = trace_debankBlock(h)  # new impl
    # state_diff
    for entry in new.state_diff.NewAccounts:
        addr = invert_or_lookup(entry.Address)
        assert eth_getBalance(addr, h) == entry.Balance
        assert eth_getTransactionCount(addr, h) == entry.Nonce
        assert keccak256(eth_getCode(addr, h)) == entry.CodeHash
    # events
    receipts = native_eth_getBlockReceipts(h)
    for tx_idx, r in enumerate(receipts):
        new_logs = [e for e in new.events if e.tx_idx == tx_idx]
        receipt_logs = r.logs + synthetic(r.transactionLogs)
        assert (emitters, topics, data) of new_logs == receipt_logs
    # traces — divergence 不在响应字段里，从 metric 端旁路验证
    diverged_count_from_metric = scrape("trace_debankblock_replay_diverged_tx_total")
    # spot check: 已知分叉块（如 12,496,844）跑完后 metric 应 +1
```

### 8.3 灰度回归

1. 部署到 x3 上一个独立 writer instance（保留旧 writer 跑作对照）
2. 新 writer + 新 ETL + 新 leafage（独立 topic）
3. 跑全链 backfill，consistency-checker 对比新 leafage state vs native iotex 24h+
4. 与旧 leafage diff：预期发现一批历史 silent drift 已被 fix
5. 性能基线：trace_debankBlock 平均 latency 应下降 50%+（去掉了 collector 同步路径的开销，主路径只读 changeset）

---

## 9. 已识别风险与未决问题

### 9.1 selfdestruct 缺真实样本

iotex 主网 SELFDESTRUCT 罕见，本次 strict eval 在数千块扫描内未抓到。

**应对**：
- Mock 单测覆盖（合成 EVM SELFDESTRUCT round-trip）
- 在更大窗口（百万块）做一次专门的 SELFDESTRUCT 扫描
- 路径已经是 Erigon 标准语义（DeletedAccounts via empty post-state），行为可预测

### 9.2 Erigon `kv.Code` 表的初始化完整性

`kv.Code` 表里所有 codeHash → bytecode 是 writer 在 commit 时 `PlainStateWriter.UpdateAccountCode` 写的。如果某个 codeHash 在表里缺失（极罕见但可能因 pruning 配置不当），canonical NewCodes 会拿不到 bytecode。

**应对**：
- 每次 canonical 路径拿不到 code 时报错而不是 silent 跳过
- 加 metric `canonical_code_missing_total`
- x3 实测 deploy block 32,670,059 上 1159 bytes 完整匹配，路径已验证

### 9.3 `0xa576c141`-类 protocol 路由地址

它**会出现在 canonical state_diff 中**（writer 确实把它写进 PlainState）。语义上它是 protocol 路由副产物，不是用户账户。

**问题**：要不要保留？保留的话 leafage 会看到一个实际是 rewarding-protocol-routed 的 EOA-like 地址，余额每块变化。

**应对（建议）**：
- 默认保留——canonical 路径自然吐出，下游愿意按自己的逻辑过滤就过滤
- 不在 writer/RPC 层做特殊化
- 文档化这个地址的语义

### 9.4 leafage 历史数据回灌

当前 leafage 数据中已有的 silent drift（pre-fork nonce 错、pre-Sumatra 分叉块的错误 state）：是否需要回灌？

**决定**：
- **建议回灌**——旧数据已知错，长期不一致积累影响审计
- 配合实施时间表 P2 阶段：新 writer 起 + 新 ETL 起 + 旧 leafage 数据清空 + 从 0 backfill

### 9.5 ETL 端 tip-1 滞后

旧 ETL 以为 trace_debankBlock(tip) 总是可用。新方案下，必须 tip+1 已 commit。

**应对**：
- 不需要改 background-tracer 错误处理逻辑——ETL 已有的「任何错误自动重试 / 持续失败阻塞」语义对所有 canonical sentinel 都正确
- 自然生产环境 ETL 通常落后 tip 1-2 块，几乎不触发
- 提供 fallback：如果 ETL 配置 `prefer_canonical=false`，可以走 replay-only 路径（旧方案保留为后门）

### 9.6 Erigon RoTx 生命周期

每次 trace_debankBlock 都开 BeginRo + 多个 cursor + defer Rollback。高 QPS 下要确认 mdbx 没有锁开销热点。

**应对**：
- 性能测试：x3 上跑 100 QPS 验证
- 如有问题：考虑 RoTx 池或长连 RoTx + per-call lock-free reads

### 9.7 Trace 内部一致性 — **不可消除的固有局限** ⚠️

> 这是新方案最 tricky 的一块——放在最显眼的位置以避免后人误读为 bug。

#### 现状

我们能在 trace 上做到的：
- ✅ **顶层 status 一致**：分叉 tx 的顶层 `tx.Status` / `tx.GasUsed` 用 canonical receipt 强制覆盖
- ✅ **顶层 trace 不缺失**：分叉 tx 的 replay 调用树丢弃后，合成一条 canonical-minimal trace（单 frame，from/to/value/input/gas/status 全对，subtraces=0）
- ✅ **events**：完全 canonical（receipt.Logs + TransactionLogs）

我们**做不到**的：
- ❌ **内部 call 树正确**：即使顶层 status 与 canonical 一致的非分叉 tx，replay 跑出来的 traces 内部仍可能漂移：
  - 子调用成功/失败翻转（被外层 try/catch 吞掉，顶层不可见）
  - 内部 LOG opcode 触发与否（极少数情况）
  - gas 的 frame 级分摊（如 47,487,940 上 0xa576c141 的 0.8 IOTX）
  - CALL 时 value 转账金额（不发 LOG 的老式 token 用这个转账）
  - SSTORE 在哪个 frame 内执行
- ❌ **canonical traces 不存在**：iotex-native 没有持久化的 canonical call trace，receipts 也只暴露 logs 不含 frame——没法重构「主网当年内部到底怎么调」

#### 为什么不修

要让 traces 内部和 canonical 完全一致，唯一办法是**用 v1.x 时代的 iotex-core binary 重跑历史块**——这是 `replay-vs-mainchain-divergence-analysis.md` §6.5 已经否决的方案 D，工程量数月、ROI 极低。在 0.7% 分叉量级 + 「state 正确性已被 state_diff/events 守住」的前提下，不值得做。

#### 这意味着什么（写给下游 ETL）

| 用途 | 用什么数据 | 准确性 |
|---|---|---|
| **资金/余额变更** | `state_diff`（canonical changeset 派生） | 100% canonical |
| **token 转账检测**（ERC20/721 等 emit Transfer 的） | `events`（canonical receipts 派生） | 100% canonical |
| **token 转账检测**（不发 event 的老式 token） | `traces` 中的 CALL value | **best-effort**——分叉块上有合成 minimal trace（无内部调用，会漏报）；非分叉块上 replay 派生的内部 value 可能数额漂移 |
| **internal call 关系图**（"A 调了 B 调了 C"） | `traces` | best-effort——非分叉块通常对，但 frame 级 gas/value 不保证，分叉块只有顶层 frame |
| **call 失败回滚原因分析** | `error_traces` + `error_events` | 同上，best-effort |

**金句**：`state_diff` 是法律意义上的真相源，`events` 是日志意义上的真相源，`traces` 只是**调试意义上的近似还原**。

#### 下游识别能力缺失（schema 不扩 = 不暴露）

一个值得强调的盲区：**下游 ETL 拿到 RPC 响应，无法从中辨识哪些 trace 不可信**。

具体来说，一条 `Subtraces=0` 的 trace 可能是以下三种之一，**ETL 看不出区别**：

| 情况 | trace 形态 | trace 是否反映主网 |
|---|---|---|
| (a) 主网就是简单 transfer，replay 也跑出单 frame | Subtraces=0，from/to/value 都对 | ✓ |
| (b) 主网有内部调用，但本笔 tx 是 status 分叉 → 写入端合成成单 frame | Subtraces=0，from/to/value 都对 | ✗ 内部调用丢失 |
| (c) 状态一致的非分叉 tx，replay 顶层和 canonical 一样但内部漂移 | Subtraces > 0，可能内部 gas/value/sub-call 错 | ⚠️ 顶层对、内部 best-effort |

**ETL 用 trace 做内部资金归因（特别是不发 event 的老式 token transfer）会在 case (b) 上漏报、case (c) 上算错**——且响应里没有任何字段告诉 ETL 这条 trace 是哪一档。

**Metric 在这件事上帮不上忙**——`trace_debankblock_replay_diverged_*_total` 只是 ops dashboard 信号，ETL 不会一边消费 RPC 一边查 Prometheus。

**根治需要扩 schema**——给 `pipeline/types/trace.go` 的 `Trace` 加 `Replaced bool` 或顶层 `DebankOutPut.DivergedTxIDs []string`，写入端在合成 / 分叉时填值，ETL 据此降级处理。**本次方案不做**（避免跨 repo schema 改动），作为待决策项记入 plan。

ETL 端目前的安全做法：
- 资金归因优先走 `state_diff` 和 `events`（这两份永远 canonical）
- `traces` 只做「我是不是错过了什么」的辅助信号，不直接得出资金结论
- 对未发 event 的老式 token，接受 ≤ 0.7% 分叉块上有归因缺失

#### 检测可加固但意义有限

可在未来补一层「per-tx canonical log count vs replay log count 比对」，发现内部 LOG 漂移就也降级成合成 minimal trace。但漏检面（CALL value 漂移、gas 漂移）依然存在，所以是边际收益。**当前不实施**，作为 P2 备用手段。

---

## 10. 实施时间表

| 阶段 | 工作项 | 估时 |
|---|---|---|
| P0 | `canonical_reader.go` + 单测 | 1 天 |
| P0 | `buildCanonicalStateDiff` + 单测（覆盖 §8.1 前 8 项） | 1.5 天 |
| P0 | `buildCanonicalEvents` + 单测 | 0.5 天 |
| P0 | `debankBlockImpl` 重构（含删 pool 合成代码） | 1 天 |
| P1 | tip-1 lag handling + ETL 配套（background-tracer） | 0.5 天 |
| P1 | replay best-effort 路径整理 + diverged metric | 0.5 天 |
| P1 | x3 集成测试（100 块对账脚本 + 跑） | 1 天 |
| P2 | 部署独立 writer/ETL/leafage + consistency-checker 24h | 2 天 |
| P2 | 旧数据决定（保留/回灌） + 切流 | 1 天 |
| 总计 | | **~9 天** |

---

## 11. 附录

### 11.1 Erigon API 速查（v1.9.7-0.20250305121304）

| API | 位置 | 用途 |
|---|---|---|
| `kv.AccountChangeSet` | erigon-lib | 块级 (addr, oldEncodedAccount) tuples |
| `kv.StorageChangeSet` | erigon-lib | 块级 (addr+incarnation+slot, oldValue) tuples |
| `kv.PlainState` | erigon-lib | 当前 state（post-tip） |
| `kv.Code` | erigon-lib | codeHash → bytecode |
| `kv.IncarnationMap` | erigon-lib | addr → incarnation（对 selfdestruct 重 deploy 必要） |
| `historyv2.AvailableFrom(tx)` | erigon-lib | 历史下界 |
| `historyv2.FromDBFormat(k, v)` | erigon-lib | changeset key 解码 |
| `changeset.ForRange(db, bucket, from, to, walker)` | erigon | 块级 changeset 遍历 |
| `changeset.GetModifiedAccounts(db, from, to)` | erigon | 改过 account 的 addr 列表 |
| `erigonstate.NewPlainState(tx, blockNr, nil)` | erigon | as-of state reader |
| `historyv2read.GetAsOf(tx, accCursor, csCursor, isStorage, key, blockNr)` | erigon-lib | 直接 as-of 读 |

### 11.2 height 语义警示

```
NewPlainState(tx, blockNr).ReadAccountData(addr)
  → returns state of addr BEFORE block blockNr is applied
  → 要拿 block N 结束后的状态，传 blockNr = N + 1
```

ReadAccountIncarnation 内部已经调 `GetAsOf(..., s.blockNr+1)`（plain_readonly.go:260），但上层 ReadAccountData/Storage 用的是 `s.blockNr`——所以**外部要自己 +1**。

### 11.3 旧 replay-only 路径转换说明

旧方案以「按需重放」为唯一真相源，state_diff 和 events 全靠在 archive 节点上重新执行该块得到。本方案保留 replay 但仅作 best-effort traces 来源；state_diff / events / tx 顶层字段全部切换为 canonical 数据源。具体每条主路径的迁移见 §3.1 数据来源拆分表。

### 11.4 相关文档（branch 内）

- `replay-vs-mainchain-divergence-analysis.md`（同目录）— 动机论证：跨版本 EVM 漂移、跨纪元语义漂移、已修过的 replay-only bug 列表、各方案 ROI 评估
- `CANONICAL_PR.md`（同目录）— PR 描述草稿，含行为变更对照、ETL 错误处理契约、已知 caveat

### 11.5 关键代码引用

- `iotex-core-cc/api/coreservice.go:debankBlockImpl` — 重构入口
- `iotex-core-cc/state/factory/erigonstore/workingsetstore_erigon.go:prepareCommit` — writer commit（数据来源保证）
- `iotex-core-cc/state/factory/erigonstore/accountstorage.go:Store` — 非 EVM action 写 intraBlockState（覆盖性保证）
- `iotex-core-cc/api/api_debank.go:emitTransferLogsAsEvents` — events 转换（重构基础）
- `pipeline/types/state_diff.go` — leafage schema（不变）
