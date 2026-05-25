# IoTeX Archive Replay 与主网历史结果分叉分析报告

> 日期：2026-04-24  
> 节点：`blockchain-misc-x3:/data/iotex-archive`，writer `amd64-681f1554`  
> 背景：修复 `trace_debankBlock` 在 pre-Sumatra 块上丢记 CREATE 合约 code 的问题时，发现的一个**更底层、更宽泛**的问题

---

## 1. 问题概述

在 IoTeX archive 节点上对历史块做 **EVM 交易 replay**（通过 `trace_debankBlock` 或 `debug_traceBlockByNumber`）时，**某些交易的执行结果与主网当时原始写入的 receipt 不一致**：

- 主网历史 receipt 存的是 `status=0x1`（成功），replay 跑出来是 `status=ErrExecutionReverted`（失败）
- 或者反过来：主网历史 receipt 是失败，replay 跑出来是成功

两种方向都存在。这种分叉在 pre-Sumatra 区间采样得到的比例约 **0.7%**（100 blocks / 286 tx 中 2 个分叉），post-Sumatra 尚未精确采样但肉眼观察多个样本成功。

对 Debank ETL 的直接后果：**state_diff 输出和主网真实状态不一致**——被分叉 tx 触及的账户、storage、合约 code 在 Debank 侧看不到或被误记。

---

## 2. 直接触发场景与证据

### 具体触发样本

- **区块**：`12,496,844`（2021-08-04，pre-Sumatra）
- **交易**：`0x11c86f8c901b93df5dd38e53cf69c2e9084ca9b927091224cd83a1978c0e9501`
- **类型**：`addLiquidityETH` 到 Unifi Router `0xBd562d5cF2c62Da3143D862aF39eDb6dF59A4679`
- **内部行为**：Unifi Factory 通过 CREATE2 部署一个 Unifi LP pair（合约地址 `0x17ee4b8ADBAB...`，17,645 bytes bytecode）

### 关键观测

| 数据源 | 结果 |
|---|---|
| `eth_getTransactionReceipt(tx)` | `status=0x1`（主网当年真实成功） |
| `eth_getCode(pair, 12496844)` | 17,645 bytes（主网当年真实部署） |
| `trace_debankBlock(12496844)` | 这笔 tx 在 replay 中 **reverted**，state_diff 里无 code、无合约初始 storage |
| `debug_traceBlockByNumber(12496844)` | 同上，replay 里 tx 报 `error="execution reverted"` |
| `eth_call(tx_params, 12496843)` | 同上 revert |

### 精确失败点

通过 `debug_traceTransaction` 的 `callTracer` 拿到的调用树显示：

```
CALL router → pair.mint()   gas=215,251   err='out of gas'
```

Router 给新部署的 pair 合约调 `mint()`，给的 gas budget 是 215,251。**主网当年这笔调用在这 gas budget 内执行完**；replay 中**同样 gas budget 下跑 OOG**。差异在 mint() 内部某个 opcode 的 gas 计费。

### 分叉率采样

扫描 `12,496,800 – 12,496,899` 共 100 块，286 笔 tx：
- `replay_matches`: 284
- `replay_diverge`: 2
  - `main_SUCCESS_replay_FAIL`: 1（block 12496844，即我们的目标样本）
  - `main_FAIL_replay_SUCCESS`: 1（block 12496898）
- **分叉率 ≈ 0.70%**

样本很小，后续应该做更大、多段采样。

---

## 3. 根因分析

### 3.1 基本原理

EVM replay 能和主网一致，**必须**同时满足：

1. **pre-state 一致**：archive 存的「block N-1 结束时的状态」和主网当时真实的状态一致
2. **执行规则一致**：chainConfig 返回的 EIP 组合，以及这组 EIP 在 EVM 实现中的精确语义
3. **block context 一致**：BlockHeight / BlockTimeStamp / BaseFee / Producer / GasLimit 等
4. **协议侧状态一致**：iotex 自己的 staking / rewarding / poll 等非-EVM 状态

任一项不一致都可能导致分叉。

### 3.2 本次已经排除的

- **pre-state 整体漂移**：如果 archive 存的历史状态不对，那么 `eth_getCode / Balance / StorageAt / TransactionCount` 这些接口在历史高度查询都会拿到错的值。实测对 `0x17ee4b`、token、creator 等地址在 pre-block 高度查询，返回值都能对上链上共识——数据本身对
- **block context**：probe 打印出 replay 用的是 `height=12496844 ts=2021-08-04 02:52:40 baseFee=<nil>`，和主网当年一致
- **trace_debankBlock 独有实现问题**：verify `debug_traceBlockByNumber`（iotex 官方 RPC）和 `trace_debankBlock` 走同一条 replay 路径——同一笔 tx 两者得到的 replay status 完全一致，说明分叉点在共用的底层 EVM 执行，不在我们的 state_diff capture hook 层

### 3.3 剩下的可能原因（合称 **edge-case rules/impl drift**）

主网生产 block 12496844 时跑的是 iotex-core v1.x（2021 Q3），而当前 archive 节点跑 v2.3.8。从 v1.x 到 v2.3.8 之间，iotex-core 经历了 Bering → Greenland → Hawaii → Iceland → Jutland → Kamchatka → Midway → Newfoundland → Okhotsk → Palau → Quebec → Redsea → Sumatra → Tsunami → Upernavik → Vanuatu → Wake → Xingu 等十多次 hardfork 和无数细节改动。

虽然 `getChainConfig(height)` 按高度返回的 EIP flag（Istanbul 开 / Berlin 关等）**在 v1.x 和 v2.x 之间是一致的**——否则 `eth_call` 在历史高度的查询会 broken 全盘，现状不是——但**同一个 EIP flag 下的实现**可能在微观 gas 级别有差别：

| 潜在偏差源 | 具体表现 |
|---|---|
| **go-ethereum 库版本差异** | iotex-core 依赖的 go-ethereum 在不同版本对 EIP-2200 / EIP-1884 / EIP-1283 等 SSTORE/SLOAD gas 规则有过多次微调。同一个 `rules.IsIstanbul=true` 下，新旧 go-ethereum 对同一 SSTORE 场景（例如 `0 → 非零 → 0` 的 tx 内连续修改）可能算出不同 gas 数 |
| **iotex 自研 feature flag** | `FixSortCacheContractsAndUsePendingNonce`、`CorrectGasRefund`、`RefactorFreshAccountConversion` 等 flag 控制了账户遍历顺序、gas 退款计算、nonce 语义。v1→v2 期间这些 flag 对应的代码分支可能改过 |
| **nonce 语义变化** | 例如 `PendingNonce()` 和 `PendingNonceConsideringFreshAccount()` 对 fresh legacy 账户返值不同。如果某合约在 EVM 里通过 `CREATE` 计算地址，两边算出不同地址 |
| **Protocol / precompile 行为** | iotex 有 staking / rewarding / poll 的 native protocol，EVM 可能通过特殊地址或 precompile 调用。这些 protocol 的状态查询或 gas 成本在 v1→v2 间可能变化 |

### 3.4 为什么影响**少数 tx**而非全部

大多数交易 gas 给得比较宽松（即使某个 opcode 的 gas 计费多一两百也不会 OOG），或者不触及发生变化的 opcode/分支。只有当 tx **同时满足**：

1. 在 replay 和主网间存在 gas/逻辑 diff 的 opcode 或分支被执行
2. 该 tx 在那段执行里 gas 预算刚好够主网但不够 replay，或者某个 require/revert 的分支条件反了

才会表现为 status 分叉。所以 0.7% 这个数量级是**少部分 borderline tx**的大致规模。

---

## 4. 影响范围评估

### 4.1 直接影响：state_diff 正确性

`trace_debankBlock` 的设计假设是「replay 真实还原主网那一刻的执行」。分叉 tx 下，这个假设不成立：

- 如果 main SUCCESS，replay FAIL：tx 的所有 state 变化（balance / nonce / code / storage）在 state_diff 里**全部漏掉**。下游 ETL 拿不到这笔 tx 的任何影响
- 如果 main FAIL，replay SUCCESS：tx 的 replay 产生一堆「不应该发生」的 state 变化写进 state_diff。下游 ETL 会误记这些变化

### 4.2 二级影响：下游 leafage 和 Debank ETL

`trace_debankBlock` → Kafka → leafage-evm → DB。leafage 根据 state_diff 更新自己的 state tree。**leafage 的 state tree 在这些分叉 tx 上和主网脱节**，表现为：

- 对受影响账户，`eth_getBalance` 主网返 X，Debank 查到 Y
- 对受影响合约的 storage，`eth_getStorageAt` 同样 Y ≠ X
- 对缺失的 code，合约被误认为 EOA

因为 0.7% 这个数量级很小，短期对面向用户的数据影响也许不明显；但做守恒性对账、跨链余额合计等**分析工作时会暴露**。

### 4.3 时间区间的差异

目前只小样本扫了 pre-Sumatra 一段（12,496,8xx），**不代表全链**。合理推测：

- **pre-Sumatra**：v1.x 时代 → v2.3.8，改动最多，分叉率可能最高
- **Sumatra 到当前**：EVM 层相对稳定，分叉率应该更低甚至接近 0
- **最早期（Bering / Greenland 附近）**：改动也多，分叉率可能类似 pre-Sumatra

采样脚本 `/tmp/replay_divergence_scan.py` 已留在 x3 上，可随时扩大采样。

### 4.4 不在 trace_debankBlock 责任范围

重要结论：**这是 iotex-core 整体的 archive replay 限制，不是 `trace_debankBlock` 独有的 bug**。任何用 v2.3.8 iotex-core 做历史 EVM replay 的 RPC（包括标准 `debug_traceTransaction / debug_traceBlockByNumber`）都有同样问题。`trace_debankBlock` 只是暴露了这个问题。

---

## 5. 诊断方法与工具

### 5.1 已有工具

1. **`/tmp/replay_divergence_scan.py`**（在 x3）：  
   扫描一段块，逐 tx 对比 `debug_traceBlockByNumber` 的 `error` 字段 vs `eth_getTransactionReceipt` 的 `status`，输出分叉率和样本

2. **`debug_traceTransaction(tx, {tracer: callTracer})`**：  
   对单笔 tx 拿完整调用树，能定位到哪个 sub-call 开始 revert（例如本报告中 `CALL router→pair gas=215251 err='out of gas'`）

3. **`debug_traceTransaction(tx, {tracer: structLogger})`** 或默认 opcode trace：  
   拿 opcode 级执行记录，能看到每个 op 的 gas 消耗 / stack / memory，是定位**具体哪个 opcode 计费不一致**的终极手段（输出量很大）

### 5.2 要彻底定位到具体 opcode

理论上需要：

1. 找一个 v1.x 时代的 iotex-core binary，能跑 block 12496844 replay
2. 同样用 structLogger 拿那个版本的 opcode-level trace
3. diff 两份 trace，找到**第一个 `gasCost` 不同的 op**

这个「找老版本」是最大阻力。如果能做到，能精准指出「v1.x 的 SSTORE 在场景 X 算 5000 gas，v2.3.8 算 5120 gas」这种结论，从而反推到 go-ethereum 哪次更新 / iotex-core 哪个 commit 引入了这个差异。

### 5.3 非终极但可行的收窄

在不折腾 v1.x binary 的前提下：

- **扩大分叉 tx 采样**：跨 pre-Sumatra 几个不同时间段扫 5,000-10,000 块
- **把分叉 tx 的公共特征提取出来**：是否集中在某些合约（factory / DEX / oracle），是否都是 CREATE，是否都走 SSTORE 的特定模式等
- **对其中一笔 tx 精确打印 gas 差异**：用 structLogger 标注每一步 gas，手工走一遍 EVM 规范验证

共性如果集中，就能把 drift 原因收窄到少数几个 EIP/opcode。

---

## 6. 应对方案

### 6.1 方案 A：最小守门（仅作诊断/止血）

在 `trace_debankBlock` / `debankBlockImpl` 里，**每笔 tx 跑完 replay 后对比 historical receipt status**：

- 一致 → 说明这笔 tx 的 replay status 和历史 receipt 对齐
- 不一致 → 记录 metric / 日志 / trace quality marker

**效果**：可以可靠发现 replay divergence，避免把 trace 误认为 canonical 执行结果。

**局限**：不能作为最终 state_diff 方案。因为一笔 tx replay 分叉后，后续 tx 的 working set 已经可能偏离主网；单独 skip 这一笔 tx 的 replay diff，仍然无法保证整块最终 state_diff 和 canonical state root 对齐。

### 6.2 方案 B：分叉 tx 单独换路（不推荐）

在方案 A 基础上，对分叉 tx 不放弃它的 state_diff，改用**非 replay 方式**生成：

- 查询 archive 的 `eth_getBalance/Code/Storage @ tx_pre_height` 和 `@ tx_post_height`
- 直接 diff 两个高度的账户状态，构造 NewAccounts / NewCodes / StorageDiff
- 理想情况下，希望能拿到「action_idx 前 vs 后」的状态

**问题**：这条路目前不可靠。Erigon `AccountChangeSet` / `StorageChangeSet` 是**块级** changeset，记录 block N 前的旧值和 block N 后的 canonical state，不提供 action_idx 级快照。只靠 historical receipt logs 也无法完整知道这笔 tx 触及了哪些 account/storage。

**结论**：不要做 tx-level fallback。要补救就补**整块**，否则 leafage 的块级 state_diff 和 root chaining 仍然可能错。

### 6.3 方案 C'：块级 canonical state_diff（推荐）

`state_diff` 不再从 replay collector 来，而是直接从 Erigon history 构造：

- 用 block N 的 `AccountChangeSet` 枚举本块变过的 account；
- 用 block N 的 `StorageChangeSet` 枚举本块变过的 storage slot；
- 用 block N 后的 canonical state 读取这些 account / storage / code 的新值；
- 输出整块 canonical `NewAccounts / DeletedAccounts / StorageDiff / NewCodes`。

**效果**：完全绕过 EVM replay rules drift 对 state_diff 的影响。正常块和分叉块都走同一条 canonical 输出路径。

**代价**：`trace_debankBlock` 的 state_diff 生成逻辑要重写；但成本是按本块 changed keys 线性增长，不需要扫全状态，效率可接受。

### 6.4 方案 D：根治——修 EVM（理论上最好，实际不可行）

把 v2.3.8 的 `getChainConfig` 和 go-ethereum 实现，按 iotex-core 的 git history，**把每次 EVM 相关改动都绑定到当时生效的高度**（retroactive fork-gating）。

**效果（理论）**：replay 的 EVM 行为和 v1.x → v2.3.8 任意时间点都一致。

**为什么实际做不到**：见下一节专门展开。

---

## 6.5 为什么这个问题「修不了」（至少修不干净）

短答：**问题本质是跨版本历史行为差异，而主网共识已经把当年那个行为锁死了。EVM 层很难干净根治；Debank state_diff 应该绕开 replay，以 canonical history state 为真相源**。

详细拆一下：

### 原因 1：drift 分散在太多地方，没法穷举

v1.x → v2.3.8 之间，iotex-core 在 EVM 相关领域改了**几百个 commit**，涉及：

- go-ethereum 依赖本身从某个旧版本升到新版本（go-ethereum 自己 EIP-2200 / EIP-2929 / EIP-3529 等实现也迭代过）
- iotex 自研 feature flag（`FixSortCacheContractsAndUsePendingNonce`、`CorrectGasRefund`、`RevertLog`、`FixRevertSnapshot`、`AsyncContractTrie` 等）的语义变化
- 账户体系变化（legacy nonce ↔ zero-nonce 账户的切换、`PendingNonce` vs `PendingNonceConsideringFreshAccount` 的分支）
- 协议层（rewarding v1→v2 migration、staking 多版本 view）变动
- stateDBAdapter / contractAdapter 的重写

每一处都可能在**某个高度**引入过一次"新版和旧版对同一场景算得不一样"的改动。要完全复现，得把每个改动都打上精确的激活高度。这需要：

- 逐个 commit 追溯，判断每个 EVM-touching 改动算不算"行为变更"
- 每个被认定为行为变更的改动引入新的 feature flag，默认在某高度前关
- 飞机上改引擎——还要保证改完现在的主链 sync 不出问题

现实里几乎没人能认真做这件事。iotex 官方都没做。

### 原因 2：有些改动是 bug fix，「retroactive 修复」反而破坏共识

假设 v1.x 的 SSTORE 在某场景下按 EIP-2200 规范**少扣了 100 gas**（一个 bug），到 v2.x 修了按规范扣对。那么：

- 历史 block 在 v1.x 下跑，SSTORE 少扣 100 gas → 某 tx 刚好够用，`status=success`
- 同一历史 block 在 v2.x 下跑，SSTORE 按规范扣，多扣 100 gas → OOG → `status=fail`

两者行为差异是**"v1.x 有 bug，v2.x 是对的"**。如果我们按"忠于 v1.x 行为"的思路把 v2.x 也改回 bug 版本：

- 用户查 `eth_call` 历史高度，行为"对齐 v1.x" → 正确
- 但新来的 tx 走这条修改后的 EVM → 仍然走 bug 行为 → 和当前主网共识不一致
- 节点会从主网 fork 出去

要规避，只能做 height-gated bug：只对 < fork_height 的块走旧行为，对 ≥ fork_height 的块走新行为。这是能做的，但每个被修过的 bug 都要这样处理，成本随 bug 数量线性增长。

### 原因 3：archive 读接口本身也走 v2.3.8 EVM，没有「真相源」

本来能验证"EVM 修对了"的 oracle，是**历史真实 EVM 执行**。但那个执行已经过去了，只留下一张 receipt 存着 `status=0x1` 和一堆 logs，**没有逐 opcode 的 gas 消费记录**。

所以哪怕我们改了 v2.3.8 EVM 想对齐 v1.x 行为，也**没有细粒度 oracle 能告诉我们改对了**。只能看 receipt `status` 这一个位的结果。能做到"status 对了"就满意，opcode 级行为是不是完全一致无从验证。

如果要拿到真的 opcode-level oracle，得找一个**能跑**的 v1.x 二进制，拿它在同样 pre-state 上重放做 structLogger trace。这不是完全不可能，但：

- 需要找历史版本的 iotex-core 源码、编译它（依赖、Go 版本可能都换了）
- 需要找到那个版本兼容的 archive 数据格式
- 需要对比两份 opcode trace 发现 drift，然后再在 v2.3.8 引入高度门控修复
- 一个 drift 点修完，还有下一个 drift 点

这是一个**跨几百个 commit 的对齐工程**，iotex 官方想做都需要投入几个月专人，Debank fork 更不合适。

### 原因 4：0.7% 的事，做 D 不划算

分叉率目前看是 0.7% 数量级。为修这个，做方案 D 的工作量（几个月）和收益（0.7% 修到近 0%）完全不匹配。方案 A 适合做 replay quality metric；真正保护 ETL state 的高 ROI 方案，是方案 C'：用块级 canonical changeset 直接构造 state_diff。

### 原因 5：这个问题不是 iotex 特有

其他 chain 的 archive node 都有类似问题——以太坊 mainnet 上用最新 geth 替换一个很早区块的 pre-state 重放，偶尔也会和历史 receipt 对不上。原因都是**"现在的代码不是当年的代码"**。业界更可靠的应对方式是把历史 receipt / canonical state 当真相源，而不是让最新 replay 结果驱动下游状态。

### 小结（更新）

| 方向 | 本质 | 可行性 |
|---|---|---|
| 方案 A（守门 / skip 分叉 tx） | 只按 receipt status 发现分叉并跳过这笔 tx 的 replay diff | ⚠️ 只能做诊断/止血，不足以作为最终方案 |
| 方案 B（分叉 tx 单独补 diff） | 对单笔分叉 tx 试图用 pre/post state 补救 | ⚠️ 不可靠，Erigon changeset 是块级，不是 action_idx 级 |
| 方案 C'（块级 canonical state_diff） | 一旦生成 Debank state_diff，就直接从 Erigon 历史 changeset + post-state 构造整块 canonical diff | ✅ 推荐方案 |
| 方案 D（对齐 EVM） | 修根因 | ❌ 理论可行，工程上不现实，ROI 极低 |

**更新结论：这个问题在我们的 scope 里仍然"修不干净"，但最终方案不应该是"守门 + 跳过分叉 tx"。tx-level skip 只能避免写入某一笔错误 diff，却无法保证后续 tx 的 replay working set 仍然正确；而且 leafage 消费的是块级 state_diff，跳过分叉 tx 会让下游 state root 从这个块开始脱离 canonical state。**

因此，`trace_debankBlock` 的职责边界应调整为：

- `state_diff`：不再以 replay collector 为真相源，改为从 Erigon `AccountChangeSet` / `StorageChangeSet` 枚举本块 changed keys，再从 block 后 canonical state 读取 account / storage / code，构造整块 canonical state_diff。
- `events`：以历史 receipts / transaction logs 为真相源，保证和 `eth_getTransactionReceipt` 看到的主网结果一致。
- `traces`：继续用 replay 生成，但标记为 best-effort；当 replay receipt status 与历史 receipt 不一致时记录 `replay_diverged` metric/debug 信息，不让 trace 结果影响 state_diff。
- `root`：继续使用 canonical block header 的 parent/root 对，作为 leafage 链式校验依据。

### 6.6 推荐组合（最新）

**短期 P0**：实现块级 canonical state_diff builder。不要等发现分叉后才 fallback；直接把 `state_diff` 主路径切到 Erigon changeset + post-state。这样正常块和分叉块走同一条输出路径，避免双路径语义漂移。

**短期 P0.5**：保留 replay-vs-receipt status 对比，但用途改成 metric / debug / trace quality 标记，不参与 state_diff 选择。

**中期 P1**：继续修 replay capture 层已发现的 bug（例如 `contractErigon.SetCode` / `collectPreCommitDiff` 的 code capture 缺口），因为它影响 debug trace、对照测试和 changeset 不可用时的降级能力，但它不再是 state_diff 正确性的根保证。

**长期 P2**：把 replay divergence 样本和证据上报 upstream；是否做 EVM 历史行为对齐由 iotex 官方判断。

---

## 7. 行动清单（proposed）

| 优先级 | 动作 | 产出 |
|---|---|---|
| P0 | 在 Erigon store 层新增 `CanonicalStateDiff(ctx, height)`：读取 block N 的 `AccountChangeSet` / `StorageChangeSet`，再用 block N 后的 canonical state 读取 account / storage / code | 整块 state_diff 不再依赖 replay |
| P0 | `trace_debankBlock` 改为使用 canonical state_diff builder 的输出；replay collector 产物只用于 debug 对照，不写入最终 state_diff | leafage state 不被 replay divergence 污染 |
| P0 | 从 `dao.GetReceipts(height)` 构造 events / synthetic logs，避免 replay 分叉时 event 流和历史 receipt 不一致 | events 与主网历史 receipt 对齐 |
| P0.5 | 加 replay-vs-receipt status 对比 metric：按 block / tx 记录 `main_SUCCESS_replay_FAIL`、`main_FAIL_replay_SUCCESS` | 量化 trace best-effort 风险 |
| P1 | 修复 `contractErigon.SetCode` 不更新/缓存 code hash、`collectPreCommitDiff` 跳过 `contractErigon` 的问题 | replay/debug 路径不再漏 CREATE code |
| P1 | 验证 block number 语义：用 `12496844` 的 `0x17ee4b8a...` 确认 `NewPlainState(tx, height+1)` 读到 17,645 bytes bytecode | 避免 canonical diff off-by-one |
| P1 | 扩大分叉率采样：用 `/tmp/replay_divergence_scan.py` 覆盖 pre-Sumatra 全区间 + post-Sumatra 对照 | 评估 trace divergence 影响面 |
| P2 | 上报给 iotex 官方，附分叉样本和 replay_divergence_scan 数据 | 推动 upstream 判断是否需要历史 EVM 行为对齐 |

---

## 8. 附录

### 8.1 采样脚本

`/tmp/replay_divergence_scan.py` 用法：

```bash
ssh blockchain-misc-x3 "python3 /tmp/replay_divergence_scan.py <start_block> <end_block>"
```

输出：分叉率、main/replay status 的 2×2 分布、前 20 笔分叉 tx 样本。

### 8.2 已知分叉样本

| block | tx | case | 备注 |
|---|---|---|---|
| 12,496,844 | `0x11c86f8c...0e9501` | main_SUCCESS_replay_FAIL | Unifi LP 部署，pair.mint() OOG |
| 12,496,898 | `0x119754bd...c3255` | main_FAIL_replay_SUCCESS | 反向分叉样本，尚未深挖 |

### 8.3 相关文件

- `api/coreservice.go:debankBlockImpl` — 当前 replay 入口
- `api/coreservice.go:traceBlock` — 官方 debug_traceBlockByNumber 入口，共用底层逻辑
- `state/factory/workingset.go:runAction` / `processLegacy` — action-level replay
- `action/protocol/execution/evm/evm.go:ExecuteContract / executeInEVM` — EVM 执行入口
- `action/protocol/execution/evm/evm.go:getChainConfig` — chainConfig 生成（按高度）
