# Code Review Response

## #1 currentIdx 索引漂移 — 置信度 100

**判定：不存在**

经分析，当前代码中此问题不成立：

1. 非 EthCompatibleAction 只有 `PutPollResult`，且 `processLegacy` 已在 `Simulate` 模式下跳过整个 action validation 循环（含 PutPollResult），所以 PutPollResult 不会进入 TraceStart 路径
2. `iotexRPCTracer` 有 `txStarted` 标志位防止 CaptureTxStart 双重调用，CaptureTxEnd 同样有去重保护
3. EVM 内部（`evm.Call/Create`）不会独立调用 `OnTxStart`，只调用 `OnEnter`。`OnTxStart` 仅由 `TraceStart` 主动触发
4. 对于 EthCompatibleAction 但非 Execution 的 action（如 Transfer、GrantReward），TraceStart 成功 → CaptureTxStart 正常调用 → currentIdx 正常递增

**不修复。**

---

## #2 preCommitDigest 在 trace_debankBlock 路径下恒为零 — 置信度 75

**判定：路径分析有误，但存在一个相关的真实 bug**

preCommitDigest 在 `workingset.Commit()` 中计算，但 DebankBlock 路径调用的是 `WorkingSetAtTransaction` → `Process()`，**不调用 Commit()**。所以 preCommitDigest 与 DebankBlock 无关。

但 `originRoot` 确实硬编码为 `common.Hash{}`（coreservice.go:2458），没有从父块读取 DeltaStateDigest。这导致 `state_diff.parent_hash` 为 EmptyRootHash。

**实际影响**：leafage 的 `kafka_updater.rs:216` 会用父块 header 的 `state_root` 覆盖 `diff.parent_hash`，所以**不影响当前同步结果**。但代码不应依赖下游纠正，应修复。

**修复**：从父块读取 DeltaStateDigest 设为 originRoot。

---

## #3 新文件缺少 Apache 2.0 License Header — 置信度 75

**判定：有效**

`api/api_debank.go` 和 `api/rpc_tracer.go` 缺少 copyright header。

**修复**：添加。

---

## #4 fmt.Errorf("%w") 替代 errors.Wrap() — 置信度 75

**判定：有效**

coreservice.go:2424 用了 `fmt.Errorf("%w")`，应改为 `errors.Wrapf`。

**修复**：改为 `errors.Wrapf(err, "WorkingSetAtTransaction failed at height %d", blk.Height())`。

---

## #5 buildGenesisDebankOutput 静默吞掉 RLP 编码错误 — 置信度 75

**判定：轻微，加 log**

有 fallback 处理（设空字节），但缺少日志告警。

**修复**：加 `log.L().Warn`。

---

## #6 Archive 节点 GenesisStateRoot 与全量同步节点不一致 — 置信度 75

**判定：已知设计决策**

archive 节点无法在已有数据的 DB 上重放 genesis 状态创建（账户已存在会报错），用 genesis config hash 作为确定性非零占位符是当前最务实的方案。IoTeX 上游未持久化 genesis state root，彻底修复需要上游改动。

已记录在 `docs/implement.md`。**不修复。**

---

## #7 tracer.BlockCtx.BlockHash 全局可变状态 — 置信度 75

**判定：不影响 DebankBlock**

DebankBlock 不使用 pipeline tracer 的全局 BlockCtx（那是实时同步路径的），用的是自己的 iotexRPCTracer。**不修复。**

---

## #8 process() 路径未在 Simulate 模式下跳过 action 验证 — 置信度 75

**判定：有效**

`process()`（post-CorrectValidationOrder fork 路径）的用户 action validate 循环没有 Simulate 检查。当 ETL 同步到 post-fork 高度时，如果 archive state 与历史 delegate/candidate 数据不匹配，会遇到与 processLegacy 同样的问题。

**修复**：加 Simulate 检查，与 processLegacy 一致。

---

## #9 state/factory 反向依赖 blockchain 包 — 置信度 50

**判定：已有**

`blockchain.GenesisStateRoot` 的 import 在 PR 之前已存在（`createGenesisStates` 中设置）。不是本次引入的。**不修复。**

---

## 修复计划

| # | 修复 | 优先级 |
|---|------|--------|
| 2 | originRoot 从父块读 DeltaStateDigest | 中（不影响同步） |
| 8 | process() 加 Simulate 检查 | 中（post-fork 区块遇到再说也行） |
| 3 | 加 license header | 低 |
| 4 | fmt.Errorf → errors.Wrapf | 低 |
| 5 | RLP 编码错误加 log | 低 |
