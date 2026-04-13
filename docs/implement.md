# trace_debankBlock 实现记录

实现过程中的决策、疑问、发现记录在此。

---

## 2026-04-13 RPCTracer 放在 pipeline iotex 分支而非本地复制

**决策**: RPCTracer 实现在 `Chaintable/pipeline` 的 iotex 分支（`pipeline_iotex/tracer/rpc_tracer.go`），而非在 iotex-core-cc 中复制 callTracer。
**原因**: callTracer 在 pipeline 包中是 unexported 的，RPCTracer 作为其包装可以直接访问。避免跨仓库代码复制，保持 pipeline 包一致性。
**后续**: 完成后需发布 pipeline 新版本，iotex-core-cc 更新 go.mod 引用。

## 2026-04-13 RPCTracer 使用 vm.EVMLogger 接口（旧式）

**决策**: RPCTracer 使用 `vm.EVMLogger` 接口（CaptureTxStart/CaptureStart/CaptureEnd 等），而非 v0.0.63 的 `tracing.Hooks` 接口（OnEnter/OnExit 等）。
**原因**: IoTeX v2.3.3 的 EVM 使用旧式 `vm.EVMLogger` 接口，pipeline iotex 分支的 callTracer 已适配此接口。

## 2026-04-13 不需要 dirtyStorage/dirtyStorageOrigin

**发现**: pipeline-cc2 在 StateDBAdapter 中新增了 `dirtyStorage`/`dirtyStorageOrigin` 来追踪 storage 变更。但 pipeline 分支的 `contract.committed` map 已经追踪了所有 `SetState` 调用（包括 fresh keys），因为 `SetState()` 中会先调用 `GetState()` 将原始值存入 `committed`。
**决策**: `StateDiff()` 直接复用 `contract.committed` + `trie.Get()` 来获取 storage 变更，无需额外字段。同时复用已有的 `dirtyAccounts` 追踪非合约账户的余额/nonce 变更。
**好处**: 减少侵入性修改，不改 SetState/clear 方法，降低回归风险。

## 2026-04-13 测试发现的问题

### 问题 1: 多 tx 区块 EVM tx 未被捕获（txs=0）
**现象**: 多 tx 区块（如 47040000, 2 txs）返回 txs=0，但 state_diff 有数据
**根因**: 待确认。可能是 CaptureTxStart/CaptureTxEnd index 管理问题 — 非 EVM action 不触发 CaptureTxStart 但 CaptureTxEnd 仍被调用导致 index 错位；或 TraceStart → newEVM 失败跳过整个 trace 链路
**影响**: EVM tx 的 txs/traces/events 全部缺失

### 问题 2: 单 tx 区块非 EVM action 返回空 tx
**现象**: 单 tx 区块（47040001）txs=1 但字段全为 null/0/false
**根因**: 非 EVM action 的 CaptureTx 回调被触发了，但 RPCTracer.OnTxStart 没被调用（非 EthCompatibleAction），callTracer 为 nil，OnTxEnd 直接 return，tx 内容未填充
**处理**: 非 EVM action 不应该出现在 txs 中，或应以特殊方式处理

### 问题 3: genesis block 返回 height=1
**现象**: trace_debankBlock("0x0") 返回 height=1，txs=0
**根因**: web3server 的 parseBlockNumberOrHash 把 "0x0" 解析为 height=1（IoTeX 没有 block 0 概念），没走到 DebankBlock 的 `if height == 0` 分支

### 问题 4: latest 返回 height=0
**现象**: trace_debankBlock("latest") 返回 height=0
**根因**: parseBlockNumberOrHash 对 "latest" 的处理可能有问题

### 问题 5: gasUsed/gasLimit header 不匹配
**现象**: header.gasUsed=0x2141d 但 eth_getBlockByNumber.gasUsed=0x0; gasLimit 也不匹配
**根因**: IoTeX 的 eth RPC 返回的 gasUsed/gasLimit 与 pipeline header 使用的值计算方式不同

### 已通过的测试
- Section 1: 顶层结构 4/4 PASS
- Section 2: block hash/parent/miner/timestamp PASS, gasLimit/gasUsed MISMATCH
- Section 9: header 7/9 PASS (hash/parentHash/stateRoot/txRoot/receiptRoot/number/timestamp/miner), gasUsed MISMATCH
- Section 10: validation_hash type/non-zero/idempotent PASS
- Section 11: 不存在区块返回 error PASS, parent_id 链 PASS, genesis/latest 有问题
- Section 12: 性能 30ms PASS, parent_id 链 PASS
- Batch: 10 blocks hash 全部 PASS, 单 tx 区块 tx 数量匹配

## 2026-04-14 debug_traceTransaction "unknown tracer type" 是已有 bug

**发现**: `debug_traceTransaction` 对 EVM tx 报 `"unknown tracer type: *api.evmTracer"`
**根因**: `traceTransaction`（web3server.go:1301）直接对 `traceTx` 返回的 `*evmTracer` 做 type switch，但 `*evmTracer` 不是 `*logger.StructLogger` 也不是 `tracers.Tracer`。应该用 `tracer.(*evmTracer).Unwrap()` 拿到内部 tracer 再做 switch（`traceBlock` 就是这样做的）。
**影响**: 与 trace_debankBlock 无关，是 iotex-core-x 分支 `traceTx` 重构后的遗留 bug。x5 旧镜像未暴露是因为没有 EVM tx 可测。
**修复**: web3server.go:1301 改为 `tracer.(*evmTracer).Unwrap()` 后再 switch

## 2026-04-13 DebankBlock 缺少 HelperCtx 导致 panic

**发现**: 首次部署测试时 `trace_debankBlock` 返回 500，日志显示 `Miss evm helper context` panic。
**根因**: `DebankBlock` 没有设置 `evm.HelperContext`（`GetBlockHash`、`GetBlockTime`、`DepositGasFunc`），而 `TraceStart` → `newParams` → `mustGetHelperCtx` 要求必须有。
**修复**: 参考 `traceBlock` 中的设置，在 `DebankBlock` 中添加 `evm.WithHelperCtx`。Commit `12c7abca7`。

## 2026-04-13 自定义镜像 docker-compose command 问题

**发现**: 自定义镜像 ENTRYPOINT 是 `/usr/local/bin/iotex-server`，docker-compose command 中不能再写 `iotex-server`，否则变成 `iotex-server iotex-server -config-path=...`。
**修复**: command 只写参数 `-config-path=... -genesis-path=... -plugin=gateway`。

## 2026-04-13 两层 State Diff 采集

**决策**: 使用 workingset 层 `PipelineStateDiffCollector`（accounts/destructs）+ EVM 层 `StateDBAdapter.StateDiff()`（storages/codes）双层采集。
**原因**: 非 EVM action（staking/reward）的余额变更走 protocol handler，绕过 EVM StateDB。只用 EVM 层会导致 getBalance 不准。
