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

## 2026-04-13 两层 State Diff 采集

**决策**: 使用 workingset 层 `PipelineStateDiffCollector`（accounts/destructs）+ EVM 层 `StateDBAdapter.StateDiff()`（storages/codes）双层采集。
**原因**: 非 EVM action（staking/reward）的余额变更走 protocol handler，绕过 EVM StateDB。只用 EVM 层会导致 getBalance 不准。
