# debank_* RPC namespace 设计文档（IoTeX writer 端协议地址路由 + batch simulate + EVM call trace）

> 版本：v1-draft
> 日期：2026-05-07
> 关联 PR：[Chaintable/iotex-core-x#6](https://github.com/Chaintable/iotex-core-x/pull/6)
> 配套：leafage-evm 端 [Chaintable/leafage-evm#145](https://github.com/Chaintable/leafage-evm/pull/145)

---

## 1. 背景与动机

### 1.1 问题

leafage-evm（Rust revm-based reader）在 IoTeX 链上对 4 个**协议地址**调用返回错误结果：

| Protocol | 地址（EVM hex） |
|---|---|
| Staking | `0x04c22afae6a03438b8fed74cb1cf441168df3f12` |
| Rewarding | `0xa576c141e5659137ddda4223d209d4744b2106be` |
| Poll | `0x166b743c2c1a57c93c2e2bc3e169d28bbb9f6da3` |
| RollDPoS | `0x041370e00a711cd81da1918f0e494459aadae50e` |

这些地址在 IoTeX 上不是普通合约，**没有 EVM bytecode**，由 `iotex-core-cc` 在 `eth_call` 路径（`api/web3server.go`）通过 `BuildReadStateRequest` 路由到对应协议的 ABI 解析器，最终调 `coreService.ReadState(...)` 返 ABI-encoded 数据。

leafage 的 revm 看到这些地址 `code = 0x` 直接当空合约返 `0x`，**对客户端假成功**。具体表现：
- 用户调 `candidatesV3` / `unclaimedBalance` 等协议 ABI 返空响应
- batch `simulateTransactions` 撞协议地址时整批 `Invalid gas limit -39001`

详见 `task_iotex/chain-sync-verification-iotex-20260506.md` TODO #1 / #6。

### 1.2 现有协作机制

`nodex-proxy` 为 cosmos chains 已实现**完整的 native-retry 机制**（`lb/lb.go::shouldRetryWithNative`）：

```
client → proxy → leafage 处理失败（返 -39008 UnsupportedPrecompile）
       → proxy.Reset response
       → rewriteMethodForNativeRetry: 把 simulateTransactions/contractMultiCall/estimateGas
         前缀加 "debank_"
       → forward to native node 池
       → native node 用 chain-specific 实现处理
```

这个机制对 IoTeX 完全适用——只缺两个端点：
1. **leafage 端**：识别 IoTeX 协议地址 → 返 `-39008`（在 [leafage PR #145](https://github.com/Chaintable/leafage-evm/pull/145) 实现）
2. **writer 端（本 PR）**：暴露 `debank_simulateTransactions` / `debank_contractMultiCall` / `debank_estimateGas` 三个 RPC method

---

## 2. 目标 / 非目标

### 目标

1. 在 writer 暴露三个 `debank_*` method，wire schema 严格遵循 DeBank 标准，nodex-proxy 透明转发
2. `debank_contractMultiCall` 支持协议地址 ABI 路由（read-only batch），跟 `eth_call` 行为一致
3. `debank_simulateTransactions` 支持 batch state-mutating simulation（前 tx 改的 state 后 tx 能看到），且对协议地址走 ABI 旁路（跟 eth_call 一致）
4. `debank_simulateTransactions` 输出每个 tx 的 EVM call frame trace（DeBank `traces[]` 字段）
5. 复用 writer `eth_call` 现有的协议地址路由代码（`callProtocolAddr` helper）
6. 复用 writer 现有 working set / EVM execution 框架，不动 `evm.SimulateExecution` / `ExecuteContract` 等核心代码

### 非目标

1. **不修 envelope-bloat bug**（TODO #3）：`debank_estimateGas` 直接 wrap 现有 `eth_estimateGas`，envelope-bloat 留独立 PR 修
2. **不实现 0xeeee...eeee native token 自动 ERC20 metadata**：leafage 已正确处理这条路径，proxy 不会 retry 到 writer
3. **不实现 multiCall 的 fastFail / useParallel / disableCache 优化**：v1 接受参数但忽略，串行执行
4. **不重构 cosmos-evm fork 让两边共用 wire types**：iotex-core-cc 内独立定义但字段名 / JSON tag 严格一致

---

## 3. 端到端架构

```
client → nodex-proxy → leafage
                       ├─ EVM 标准路径 → 正常返回
                       └─ to ∈ {0x04c2..., 0xa576..., 0x166b..., 0x0413...}
                          → IotexEvm.frame_init → unsupported precompile
                          → ContextError::Custom("unsupported precompile address: 0x...")
                          → ToJsonRpcError → -39008 UnsupportedPrecompile
                                                                    ↓
                       proxy.shouldRetryWithNative ✓
                       ├─ Reset response
                       ├─ rewriteMethodForNativeRetry:
                       │    simulateTransactions → debank_simulateTransactions
                       │    contractMultiCall    → debank_contractMultiCall
                       │    estimateGas          → debank_estimateGas
                       ├─ NodeSelector.GetNode(_, "native") → iotex writer 池
                       └─ 重投 → writer (本 PR):
                                ├─ debank_simulateTransactions
                                │  ├─ 协议地址 → callProtocolAddr → ReadState ABI dispatch
                                │  └─ 普通 EVM → SimulateExecutionBatch (cross-state)
                                │     + callTracer → traces[]
                                │     + receipt.Logs → events[]
                                ├─ debank_contractMultiCall
                                │  ├─ 协议地址 → callProtocolAddr
                                │  └─ 普通 EVM → ReadContract (read-only)
                                └─ debank_estimateGas → wrap eth_estimateGas
```

---

## 4. Wire schema（DeBank 标准）

> 标准 owner 是 DeBank 公司，**不是** cosmos-evm（cosmos-evm 是 reference 实现之一）。canonical 来源以 [leafage-evm `crates/leafage-evm-types/src/rpc/debank.rs`](https://github.com/Chaintable/leafage-evm/blob/main/crates/leafage-evm-types/src/rpc/debank.rs) 为准。

### 4.1 请求示例

`debank_simulateTransactions` JSON-RPC params：

```json
[
  [
    {
      "from":     "0x...",
      "to":       "0x...",
      "gas":      "0x186a0",
      "gasPrice": "0xe8d4a51000",
      "value":    "0x0",
      "data":     "0x...",
      "nonce":    "0x0",
      "chainId":  4689
    },
    { "from": "0x...", "to": "0xa576c141...", "data": "0xad7a672f" }
  ],
  { "block_id": "latest", "type": "Equals" }
]
```

- `params[0]` = `[]debankCallArgs`（8 字段精简版）
- `params[1]` = `debankBlockContext = { block_id, type }`，`block_id` 复用 go-ethereum `rpc.BlockNumberOrHash`，`type` ∈ `"Equals"` / `"Contains"`（后者语义降级到 latest）
- `params[2]` (可选) = `BlockOverrides`，**v1 接收忽略**

### 4.2 响应示例

```json
{
  "results": [
    {
      "code": 0,
      "err": "",
      "gas_used": 23456,
      "traces": [
        {
          "id": "0",
          "from_addr": "0x...",
          "gas_limit": 100000,
          "input": "0x...",
          "to_addr": "0x...",
          "value": "0x0",
          "gas_used": 23456,
          "output": "0x...",
          "type": "call",
          "call_type": "CALL",
          "tx_id": "0x0000...0001",
          "parent_trace_id": "",
          "pos_in_parent_trace": 0,
          "self_storage_change": false,
          "storage_change": false
        }
      ],
      "events": [
        {
          "id": "",
          "contract_id": "0x...",
          "selector": "0xddf252ad...",
          "topics": ["0x...", "0x..."],
          "data": "0x...",
          "tx_id": "0x0000...0001",
          "parent_trace_id": "",
          "pos_in_parent_trace": 0
        }
      ]
    }
  ],
  "stats": {
    "block_num": 47862026,
    "block_hash": "0xec54d058...",
    "block_time": 1778066817,
    "success": true
  }
}
```

### 4.3 关键 wire 约定

- **`tx_id`** = `BigToHash(big.NewInt(i+1))`（1-based 索引哈希），**不是** real onchain tx hash（hypothetical 没 hash）
- **trace.id** 是路径式字符串：`"0"` 根、`"0_0"` 第一个子调用、`"0_1_0"` 子的第一个孙
- **trace.gas / gas_used** 是 `*big.Int` → JSON number（go `*big.Int.MarshalJSON` 输出十进制 raw number）
- **trace.value** 是 `*hexutil.Big` → JSON `"0x..."` hex string
- **events / traces** 数组即使空也必须 `[]` 不能 `null`（Go 端 `[]debankEvent{}` 初始化保证）
- **stats.success** = 所有 `results[].code == 0` 的 AND
- **block_time** = Unix **秒**（int64），不是毫秒
- **错误码**（按 leafage canonical enum）：
  - `-39000` Reverted（EvmRevert）
  - `-39001` GasExhausted
  - `-39002` BalanceExhausted
  - `-39003` NonceError
  - `-39004` Unknown（EvmFailed）
  - `-39008` UnsupportedPrecompile（leafage 端用，writer 端不主动返）

---

## 5. 关键设计决策

### 5.1 `debank_simulateTransactions ≈ trace_callMany + 4 层薄壳`

EVM 执行路径跟 parity 的 `trace_callMany` **完全相同**：单 ws + 串行 `evm.ExecuteContract` + `ReadOnly=false` + callTracer 收集 callFrame。

差异仅 4 层 IoTeX/DeBank 适配壳：

| Layer | trace_callMany | debank_simulateTransactions |
|---|---|---|
| 协议地址路由 | 无（在 EVM 里 = 空合约 → revert） | 入口检测 → `callProtocolAddr` → `ReadState` ABI dispatch（不进 EVM） |
| `events[]` 字段 | 仅 trace | trace + `receipt.Logs` 转 events |
| Wire schema | parity（`Result/Trace[]/StateDiff/VMTrace`） | DeBank（`DebankSimulateResp/DebankSingleSimulateResult`） |
| 错误码 | standard JSON-RPC | `-39000`/`-39001`/`-39002`/`-39004` 按 DeBank |

### 5.2 协议地址在 simulate 中的处理

simulate batch 入口预处理：每个 arg 先尝试 `protocolAddrSimulateResult` 路由，命中标 `Skip=true` + 缓存 synthetic 结果；不命中走 EVM。

```go
batchArgs := make([]SimulateBatchArg, len(args))
protoOverrides := make([]*debankSingleSimulateResult, len(args))
for i := range args {
    if override, ok := svr.protocolAddrSimulateResult(&args[i], height, int64(i+1)); ok {
        protoOverrides[i] = override
        batchArgs[i] = SimulateBatchArg{Skip: true}
        continue
    }
    caller, elp, err := buildEnvelopeFromDebankCallArgs(&args[i])
    if err != nil { return nil, err }
    batchArgs[i] = SimulateBatchArg{Caller: caller, Envelope: elp}
}
results, info, err := svr.coreService.SimulateExecutionBatch(ctx, height, archive, batchArgs)
```

`SimulateExecutionBatch` 内部对 `Skip=true` 的 slot 直接 continue，**不动 working set state**。这保留 cross-state 语义：协议地址是 read-only，不影响后续 EVM tx 看到的 state。

合并：
```go
for i := range results {
    if protoOverrides[i] != nil {
        resp.Results[i] = *protoOverrides[i]
    } else {
        resp.Results[i] = simulateBatchToDebank(&results[i], int64(i+1))
    }
}
```

### 5.3 Batch state-mutating 实现

写 `coreService.SimulateExecutionBatch(ctx, height, archive, args[]) → (results[], info, err)`：

1. 拿一次 working set（`WorkingSetAtHeight` for archive，`WorkingSet` for tip）
2. setup `BlockCtx` 模拟 tip+1 块（仿 `evm.SimulateExecution` ctx 设置）
3. 串行 loop 每 args：
   - 如 `Skip=true` → continue（协议地址 slot 由 caller 填）
   - 否则：lookup sender 的 `PendingNonce` → `SetNonce`
   - 创建 per-tx callTracer（`tracers.TraceConfig{Tracer: "callTracer"}`）
   - `protocol.WithVMConfigCtx(ctx, vm.Config{Tracer, NoBaseFee})`
   - `protocol.WithActionCtx(ctx, ActionCtx{Caller, ActionHash, ReadOnly: false})`
   - **关键：`ReadOnly: false`** 让 EVM 推进 nonce / state diff，前 tx 的修改后 tx 能看到（同 `evm.SimulateExecution` 默认 `ReadOnly: true` 不同）
   - `evm.ExecuteContract(ctx, ws, envelope)` 拿 `(retval, receipt, err)`
   - 从 tracer.GetResult() 提取 callFrame JSON

4. 全部 tx 跑完 close ws。**不调 ws.Commit()**（state changes 仅在内存，不持久化到 chain）

### 5.4 callTracer hook + flatten

每 tx 的 callTracer 输出是 nested callFrame JSON：

```json
{
  "type": "CALL", "from": "0x...", "to": "0x...",
  "value": "0x...", "gas": "0x5208", "gasUsed": "0x5208",
  "input": "0x...", "output": "0x...",
  "calls": [/* nested */]
}
```

`flattenCallFrames` DFS 遍历，输出 `[]debankTrace`：
- ID 路径式：`"0"` 根、`"0_0"` 子、`"0_1_0"` 孙的子
- `parent_trace_id` 由 ID 反推
- `pos_in_parent_trace` 是兄弟序号
- type 字段映射：`CREATE/CREATE2/SELFDESTRUCT` → `CallCreateType` `create/create2/suicide`，其他 → `call`；`CallType` 保留原 EVM op 名

### 5.5 错误码映射

`simulateBatchToDebank` 把 `(receipt, err)` 映射到 DeBank code：

| 来源 | DeBank code |
|---|---|
| `r.Err == ErrInsufficientFunds` | `-39002` BalanceExhausted |
| `r.Err != nil` 其他 | `-39004` Unknown |
| `receipt.Status == ReceiptStatus_ErrExecutionReverted` | `-39000` Reverted（含 `receipt.ExecutionRevertMsg()`） |
| `receipt.Status == Success` | `0` |

`-39001` GasExhausted / `-39003` NonceError / `-39008` UnsupportedPrecompile 常量已定义，但 v1 不主动返（Halt::OutOfGas 等需要更细的 EVM error type，留 future work）。

---

## 6. 实现细节

### 6.1 callProtocolAddr helper 抽取（refactor，行为不变）

`api/web3server.go` 原 `call()` 函数内部的 4 个 protocol addr 分支（491-549 行）结构相同，提取成 helper：

```go
func (svr *web3Handler) callProtocolAddr(to string, data []byte, height uint64) (string, bool, error) {
    var (
        proto     string
        heightStr string
        sctx      protocol.StateContext
        err       error
    )
    switch to {
    case address.StakingProtocolAddr:
        proto = "staking"
        if height > 0 { heightStr = strconv.FormatUint(height, 10) }
        sctx, err = stakingabi.BuildReadStateRequest(data)
    case address.RewardingProtocol:
        proto = "rewarding"
        if height > 0 { heightStr = strconv.FormatUint(height, 10) }
        sctx, err = rewardingabi.BuildReadStateRequest(data)
    case address.PollProtocol:
        proto = "poll"
        sctx, err = pollingabi.BuildReadStateRequest(data)
    case address.RollDPoSProtocol:
        proto = "rolldpos"
        sctx, err = rolldposabi.BuildReadStateRequest(data)
    default:
        return "", false, nil
    }
    // ... ReadState + EncodeToEth ...
    return "0x" + ret, true, nil
}
```

`call()` 改为：

```go
if result, handled, err := svr.callProtocolAddr(to, data, height); handled {
    return result, err
}
```

注意 **Poll / RollDPoS 忽略 height**（保留原行为：传空字符串 height 给 `ReadState`）。

`TestCall` 现有 4 个 sub-test（staking / rewarding / contract / revert）通过，证明重构行为不变。

### 6.2 类型定义 `api/web3server_debank_types.go`

13 个 wire-compatible struct + 错误码常量。字段名 / JSON tag 严格按 leafage canonical（`crates/leafage-evm-types/src/rpc/debank.rs`）。

不 import cosmos-evm（依赖太重），独立定义但 wire 字节级一致。

### 6.3 三个 handler `api/web3server_debank.go`

`api/web3server.go` switch 注册 3 个 case：

```go
case "debank_simulateTransactions":
    res, err = svr.simulateTransactionsDebank(ctx, web3Req)
case "debank_contractMultiCall":
    res, err = svr.contractMultiCallDebank(ctx, web3Req)
case "debank_estimateGas":
    res, err = svr.estimateGasDebank(ctx, web3Req)
```

handler 实现策略：
- `simulateTransactionsDebank` — 协议地址旁路 + `SimulateExecutionBatch` (cross-state) + callTracer flatten
- `contractMultiCallDebank` — 全部 read-only，每个 args 走 `callProtocolAddr` 或 `ReadContract`，最多 50 calls
- `estimateGasDebank` — 直接 wrap `estimateGas()`（即现有 `eth_estimateGas` 逻辑），envelope-bloat bug 继承

### 6.4 `SimulateExecutionBatch` 接口签名

`api/coreservice.go` CoreService interface 加：

```go
type (
    SimulateBatchArg struct {
        Caller   address.Address
        Envelope action.Envelope
        Skip     bool  // true = caller pre-handled (e.g. protocol addr), batch engine skips this slot
    }
    SimulateBatchResult struct {
        Output    []byte
        Receipt   *action.Receipt
        TraceData json.RawMessage  // callTracer JSON output (callFrame tree)
        Err       error
    }
    SimulateBatchInfo struct {
        BlockHeight uint64
        BlockHash   hash.Hash256
        BlockTime   time.Time
    }
)

interface CoreService {
    // ... existing methods ...
    SimulateExecutionBatch(
        ctx context.Context,
        height uint64,
        archive bool,
        args []SimulateBatchArg,
    ) ([]SimulateBatchResult, SimulateBatchInfo, error)
}
```

实现位置 `api/coreservice.go` `simulateExecution` 之后。`mock_apicoreservice.go` 加对应 gomock-style mock。

---

## 7. 测试策略

### 7.1 Unit (`api/web3server_debank_test.go`)

10 个测试覆盖：

1. `TestContractMultiCallDebank_ProtocolAddr` — 协议地址通过 callProtocolAddr → 返非空 ABI 数据
2. `TestContractMultiCallDebank_RegularContract` — 普通 EVM 通过 ReadContract → gas_used 正确
3. `TestContractMultiCallDebank_RevertMapsErrorCode` — revert 映射到 `-39000`
4. `TestSimulateTransactionsDebank_BatchAndTxIDInjection` — batch + tx_id 1-based + success bit
5. `TestSimulateTransactionsDebank_ProtocolAddrRouting` — 协议地址走旁路（断言 `Skip=true` + synthetic STATICCALL trace + ABI-encoded output）
6. `TestParseDebankBlockContext` — `Equals` / `Contains` 解析 + 默认 latest
7. `TestDebankBlockTypeJSONRoundtrip` — wire string 序列化
8. `TestSimulateBatchToDebank_ReceiptLogsToEvents` — receipt logs 转 events 字段
9. `TestSimulateBatchToDebank_TraceDataPropagated` — TraceData JSON 流入 traces[]
10. `TestFlattenCallFrames` + `TestClassifyCallType` — 4-frame nested call tree DFS 顺序、ID/parent/pos 链接、EVM op 名映射

### 7.2 现有测试回归

- `TestCall` 4 个 sub-test 全过（验证 `callProtocolAddr` 提取行为不变）
- `TestEstimateGas` / `TestHandlePost` / `TestGetWeb3Reqs` 全过（验证 switch 注册不破坏现有 dispatch）

### 7.3 Integration（kava-1，待两个 PR merge + 部署后）

1. 直调 writer：
   ```bash
   curl http://kava-1:15014 -d '{"jsonrpc":"2.0","method":"debank_simulateTransactions",
     "params":[[{"from":"0x...","to":"0xa576c141...","data":"0x..."}],
               {"block_id":"latest","type":"Equals"}],"id":1}'
   ```
   验证 wire shape 跟 leafage 期望一致

2. 端到端：调 leafage `eth_call(0xa576c141..)`
   - leafage 返 `-39008`（`IotexEvm.frame_init` 触发）
   - proxy 触发 retry，rewrite method 加 `debank_` 前缀
   - writer 用 `callProtocolAddr` 处理 → 返 ABI 数据

3. 重跑 `chain-sync-verification` skill：协议地址相关 7 个 MATCH\* 应转为纯 MATCH，TODO #1 / TODO #6 关闭

---

## 8. 部署 & 验证

### 8.1 部署顺序

1. 部署 writer 新 image（含 `debank_*` handlers），rolling restart
2. 直调 writer `curl debank_simulateTransactions` 验证 wire shape
3. 部署 leafage 新 image（含 `IotexEvm` 协议地址 guard），切 chain config `evm_type: "iotex"`
4. proxy 部署的 IoTeX 链 config 加 `native_node_url` 指向 writer (kava-1:15014)
5. 验证端到端

### 8.2 回滚

writer 这边的改动是 **additive**（新 handler + 新接口 method），不动现有 `eth_call` / `eth_estimateGas`。回滚直接降版本即可，老 client 不受影响。

leafage 端切回 `evm_type: "mainnet"` 关闭协议地址 guard（行为退化为返 `0x` 假成功）。

---

## 9. 已知约束 / future work

### 9.1 envelope-bloat bug 未修

`debank_estimateGas` wrap 现有 `eth_estimateGas`，所以继承 `coreservice.go:1791-1838` `estimateExecutionGasConsumptionAt` 的系统性 ~14% 高估问题（详见 [project_estimategas_writer_bug.md](https://github.com/Chaintable/iotex-core-x/issues/...) 或 chain-sync-verification 报告 TODO #3）。独立 PR 修。

### 9.2 协议地址在 simulate 中的局限

simulate 路径下协议地址走旁路 `callProtocolAddr`，等价于 `eth_call` 单 tx 行为：
- gas_used 固定返 21000（不真实模拟 ABI dispatch 的 gas 消耗）
- 不支持 trace 展开（synthetic 1 条 STATICCALL trace）
- 不支持事件输出（events 永远空）

如未来需要支持协议地址内部 trace，要在 IoTeX core `protocol/staking/handler` 层加 hook 输出 EVM-style trace，工作量大。v1 接受此局限。

### 9.3 batch 内协议地址不影响 cross-state

按设计协议地址 read-only 不影响 ws，所以 batch 内即使穿插协议地址 + EVM tx，EVM tx 之间的 state 流转跟没有协议地址完全相同。**这是有意的语义**——客户端依赖此行为时无 surprise。

### 9.4 错误码映射不全

只映射 `-39000` Reverted / `-39002` BalanceExhausted / `-39004` Unknown。如要细分 OOG、Nonce 等，需要解析 `evm.ExecuteContract` 内部 EVM error 类型，未来改进。

### 9.5 0xeeee 不特殊处理

cosmos-evm 的 `contractMultiCall` 对 native-token 地址 `0xeeee...eeee` 返 hardcoded ERC20 metadata。leafage 已正确处理这条路径（`balance` 直查 `eth_getBalance`），proxy 不会 retry 到 writer，writer 端 v1 不实现。

---

## 10. 相关文档

- 配套 PR 设计：[Chaintable/leafage-evm#145](https://github.com/Chaintable/leafage-evm/pull/145) 把协议地址注册成 IotexEvm 的 unsupported precompile（仿 cosmos chain module 模式）
- nodex-proxy native-retry 机制：`chaintable/nodex-proxy/lb/lb.go::shouldRetryWithNative` + `rewriteMethodForNativeRetry`
- DeBank wire schema canonical：[`leafage-evm/crates/leafage-evm-types/src/rpc/debank.rs`](https://github.com/Chaintable/leafage-evm/blob/main/crates/leafage-evm-types/src/rpc/debank.rs)
- DeBank wire schema 另一参考：[`cosmos-evm/rpc/types/debank.go`](https://github.com/Chaintable/cosmos-evm/blob/main/rpc/types/debank.go) + [`cosmos-evm/x/vm/types/simulate_result.go`](https://github.com/Chaintable/cosmos-evm/blob/main/x/vm/types/simulate_result.go)（注意：错误码列表不全，以 leafage 为准）
- 测试基线 / 验证报告：`task_iotex/chain-sync-verification-iotex-20260506.md`
