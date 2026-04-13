# trace_debankBlock 测试计划 (IoTeX)

## 测试环境

- 节点: blockchain-misc-x3
- 镜像: `blockchain/shib-bor-x:amd64-12c7abca`
- 端口: 15014
- 对照: `eth_getBlockByNumber` / `eth_getTransactionReceipt` / `debug_traceTransaction`
- 日期: 2026-04-13
- 测试区块:
  - 0xd29240 (13800000, 10 txs, 含 revert tx4 status=0x6a, 多 logs, 主测试块)
  - 0x15ef3c0 (23000000, 4 txs, 含 revert tx0 status=0x6a)
  - 0x2cdc600 (47040000, EVM 合约调用, 10 logs)
  - 0x2cded24 (47050020, 2 txs, tx0 成功 15 logs, tx1 revert)
  - 0xe4e1c0 (15000000, 18 txs, 最多 tx 测试块)
  - 0x1399170 (20550000, 7 txs, 含非 EVM action)
  - 0x0 (genesis)
  - 0x1 (block 1, 接近空块)
  - latest (最新区块)

### IoTeX 特有差异（相对 go-ethereum/Tempo）

1. **非 EVM action**: IoTeX 有 staking/candidate/reward 等非 EVM action，不产生 traces/events，但余额变更体现在 state_diff
2. **Block hash**: IoTeX 使用 native hash（嵌入 MixDigest），非 geth RLP hash
3. **对照 API**: IoTeX 用 `debug_traceTransaction` 而非 `trace_transaction`
4. **无 AA tx**: IoTeX 无 EIP-4337 AA 交易
5. **无 EIP-1559**: IoTeX 在 Vanuatu fork 前无 baseFee（设为 0）
6. **Revert 状态码**: IoTeX 用 `0x6a`（ReceiptStatus_ErrExecutionReverted = 106）而非 `0x0` 表示 revert

---

# 测试结果概要

| 大类 | 测试点 | 通过 | 失败 | 不适用 |
|------|--------|------|------|--------|
| 1. 顶层结构 | 4 | | | |
| 2. block | 9 | | | |
| 3. txs | 25 | | | |
| 4. traces | 10 | | | |
| 5. events | 10 | | | |
| 6. error_traces/events | 10 | | | |
| 7. storage_contracts | 5 | | | |
| 8. state_diff (RLP) | 16 | | | |
| 9. header | 15 | | | |
| 10. validation_hash | 4 | | | |
| 11. 特殊区块 | 10 | | | |
| 12. 兼容性 | 4 | | | |
| 13. 非 EVM action | 5 | | | |
| **合计** | **127** | | | |

### trace 类型覆盖

| 类型 | 状态 |
|------|------|
| call | 待测 |
| delegatecall | 待测 |
| create | 待测 |
| staticcall | 待测 |
| suicide | 待测 (IoTeX EIP-6780 后极少) |

---

## 1. DebankOutPut 顶层结构

验证方法: jq 检查字段存在性和类型。

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 1.1 | 返回结构完整性 | jq `has("block_file","header","state_diff","validation_hash")` | |
| 1.2 | validation_hash 类型 | jq `type == "number"` 且非零 | |
| 1.3 | state_diff 格式 | 检查 `0x` 前缀 + 长度 > 10 | |
| 1.4 | header 一致性 | 关键字段与 `eth_getBlockByNumber` 对比 (详见 section 9) | |

---

## 2. block_file.block (DebankBlock)

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 2.1 | id | 类型 string(hex), IoTeX native block hash (MixDigest) | |
| 2.2 | height | 类型 number, 与请求的 block_id 一致 | |
| 2.3 | parent_id | 类型 string(hex), 与前一个区块的 id 一致 | |
| 2.4 | base_fee_per_gas | 类型 number, pre-Vanuatu 应为 0 | |
| 2.5 | miner | 类型 string(address), 与 eth_getBlockByNumber.miner 一致 | |
| 2.6 | gas_limit | 类型 number, 与 eth_getBlockByNumber.gasLimit 一致 | |
| 2.7 | gas_used | 类型 number, 与 eth_getBlockByNumber.gasUsed 一致 | |
| 2.8 | timestamp | 类型 number, 与 eth_getBlockByNumber.timestamp 一致 | |
| 2.9 | process_start_timestamp | 类型 number, 合理范围 (近期 ms 时间戳) | |

---

## 3. block_file.txs (DebankTransaction)

### 3.1 字段类型验证

对比来源:
- `eth_getTransactionReceipt` (简写 receipt): id, from_addr, gas_price, gas_used, status, idx
- `eth_getBlockByNumber(block, true)` 的 transactions 数组 (简写 tx): to_addr, gas_limit, nonce, input, value

| # | 字段 | 类型 | 对比 API 和字段 | 结果 |
|---|------|------|---------------|------|
| 3.1.1 | id | string | receipt.transactionHash | |
| 3.1.2 | from_addr | string(address) | receipt.from | |
| 3.1.3 | to_addr | string(address) | tx.to | |
| 3.1.4 | gas_limit | number | tx.gas | |
| 3.1.5 | gas_price | number | receipt.effectiveGasPrice | |
| 3.1.6 | gas_used | number | receipt.gasUsed | |
| 3.1.7 | status | boolean | receipt.status (0x1→true, 0x0→false) | |
| 3.1.8 | input | string(hex) | tx.input | |
| 3.1.9 | nonce | number | tx.nonce | |
| 3.1.10 | idx | number | receipt.transactionIndex, 从 0 递增 | |
| 3.1.11 | value | string(hex U256) | tx.value | |

### 3.2 tx 类型覆盖

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 3.2.1 | Legacy tx (type=0x0) | gas_price>0 | |
| 3.2.2 | 成功 tx | status=true | |
| 3.2.3 | Revert tx | status=false | |
| 3.2.4 | txs 数量 | 与 eth_getBlockByNumber.transactions 数量一致 | |
| 3.2.5 | idx 顺序 | 从 0 递增, 与区块内 tx 顺序一致 | |
| 3.2.6 | Contract creation tx | to_addr = 创建的合约地址 | |

注: IoTeX 不支持 EIP-1559 (pre-Vanuatu) 和 AA tx (type=0x76)，对应测试项不适用。

---

## 4. block_file.traces (DebankTrace)

### 4.1 字段验证 (逐字段与 debug_traceTransaction 对比)

| # | 字段 | 类型 | debug_traceTransaction 对应 | 结果 |
|---|------|------|--------------------------|------|
| 4.1.1 | id | string(MD5 hex, 32 chars) | 无对应 (DeBank 自有字段) | |
| 4.1.2 | from_addr | string(address) | from | |
| 4.1.3 | gas_limit | number | gas | |
| 4.1.4 | input | string(hex) | input | |
| 4.1.5 | to_addr | string(address) | to | |
| 4.1.6 | value | string(hex U256) | value | |
| 4.1.7 | gas_used | number | gasUsed | |
| 4.1.8 | output | string(hex) | output | |
| 4.1.9 | type | string | type ("call"/"create") | |
| 4.1.10 | call_type | string | callType (call 时) / "" (create 时) | |
| 4.1.11 | tx_id | string(tx hash) | transactionHash | |
| 4.1.12 | subtraces | number | calls.length | |
| 4.1.13 | trace_address | array[number] | 由 call depth 推导 | |
| 4.1.14 | error | string | error (成功=null, 失败有值) | |

### 4.2 trace type 覆盖

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 4.2.1 | call 类型 | type="call", call_type="call" | |
| 4.2.2 | delegatecall 类型 | type="call", call_type="delegatecall" | |
| 4.2.3 | staticcall 类型 | type="call", call_type="staticcall" | |
| 4.2.4 | create 类型 | type="create", call_type="" | |
| 4.2.5 | storage_change 传播 | 子 trace 有 SSTORE, 父 trace.storage_change=true | |

### 4.3 ID 计算验证

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 4.3.1 | trace id 算法 | id = MD5(tx_id + parent_trace_id + pos_in_parent_trace) | |
| 4.3.2 | root trace id | parent_trace_id="", pos=0 | |
| 4.3.3 | id 全局唯一 | 同一区块内所有 trace id 无重复 | |

---

## 5. block_file.events (DebankEvent)

### 5.1 字段类型验证

对比来源: `eth_getTransactionReceipt.logs[]`

| # | 字段 | 类型 | 对比 API 和字段 | 结果 |
|---|------|------|---------------|------|
| 5.1.1 | id | string(MD5 hex, 32 chars) | 无对应 (DeBank 自有字段) | |
| 5.1.2 | contract_id | string(address) | logs[].address | |
| 5.1.3 | selector | string(hex, topic[0]) | logs[].topics[0] | |
| 5.1.4 | topics | array[string] | logs[].topics[1:] (不含 topic[0]) | |
| 5.1.5 | data | string(hex) | logs[].data | |
| 5.1.6 | parent_trace_id | string | 必须指向同区块内真实存在的 trace id | |
| 5.1.7 | pos_in_parent_trace | number | 同一 parent 下无重复且按序排列 | |
| 5.1.8 | idx | number | 全局 log index, 递增 | |

### 5.2 event 数量验证

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 5.2.1 | events 总数 | 与 `sum(eth_getTransactionReceipt.logs.length)` 一致 | |
| 5.2.2 | idx 全局递增 | idx 值连续无间隔 | |

---

## 6. block_file.error_traces / error_events

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 6.1 | revert tx traces → error_traces | status=0x0 的 tx traces 在 error_traces 中 | |
| 6.2 | revert tx events → error_events | status=0x0 的 tx events 在 error_events 中 | |
| 6.3 | 成功 tx 不进 error | 全部 status=0x1 → error_traces=0, error_events=0 | |
| 6.4 | error_traces 字段完整 | 与 traces 字段结构一致 | |
| 6.5 | error_events 字段完整 | 与 events 字段结构一致 | |
| 6.6 | traces + error_traces 数量 | 与 debug_traceTransaction 总数一致 (per tx) | |
| 6.7 | events + error_events 数量 | success_events + revert_tx_receipt_logs = total_receipt_logs | |
| 6.8 | error 字段非空 | error_traces 中 error 字段非空 | |
| 6.9 | revert tx with EVM events | error_events 包含 revert 前 emit 的 events | |
| 6.10 | revert tx event 总数 | error_events 数 = EVM events + fee log | |

---

## 7. block_file.storage_contracts

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 7.1 | 类型 | jq `type == "array"` | |
| 7.2 | 含 SSTORE 合约 | traces 中 storage_change=true 的合约地址出现在列表中 | |
| 7.3 | 与 state_diff 对应 | storage_contracts 地址集合与 state_diff.storage_diffs 地址对应 | |
| 7.4 | 空区块 | 无 tx 的区块: storage_contracts=[] | |
| 7.5 | 非空 | 有合约调用的区块: storage_contracts 非空 | |

---

## 8. state_diff (RLP-encoded BlockStorageDiff)

RLP 解码验证使用 Python rlp 库。

### 8.1 结构验证

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.1.1 | RLP 可解码 | hex → bytes → RLP decode 成功 | |
| 8.1.2 | hash | 与 debankBlock.header.stateRoot 一致 | |
| 8.1.3 | parent_hash | 与前一区块的 stateRoot 一致 | |

### 8.2 new_accounts

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.2.1 | address | H256, keccak256(原始地址), 非零 | |
| 8.2.2 | balance | U256, 合理数值 | |
| 8.2.3 | nonce | u64, >= 0 | |
| 8.2.4 | code_hash | H256, EOA 为 KECCAK_EMPTY, 合约为非空 hash | |
| 8.2.5 | 非空区块有 new_accounts | 有 tx 的区块 new_accounts > 0 | |
| 8.2.6 | 非 EVM action 账户变更 | staking/reward action 的余额变更出现在 new_accounts 中 | |

### 8.3 storage_diffs

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.3.1 | address | H256, keccak256(合约地址) | |
| 8.3.2 | diffs[].index | H256, keccak256(storage slot) | |
| 8.3.3 | diffs[].value | U256, 新值 | |
| 8.3.4 | 与 storage_contracts 对应 | storage_diffs 地址集合 ⊆ storage_contracts (hash 后) | |

### 8.4 new_codes

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.4.1 | code_hash | H256, = keccak256(code) | |
| 8.4.2 | code | Bytes, 合约 bytecode | |
| 8.4.3 | 有部署区块 | 含 CREATE tx 的区块 new_codes >= 1 | |

### 8.5 deleted_accounts

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.5.1 | selfdestruct | 含 selfdestruct 的区块 deleted_accounts > 0 | |
| 8.5.2 | 正常区块 | 无 selfdestruct 的区块 deleted_accounts=0 | |

### 8.6 空区块

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 8.6.1 | 无 tx 区块 | new_accounts=0, storage_diffs=0, new_codes=0, deleted=0 | |

---

## 9. header

验证方法: 关键字段与 `eth_getBlockByNumber` 返回值对比。

| # | 测试项 | 验证内容 | 结果 |
|---|--------|---------|------|
| 9.1 | hash | IoTeX native hash (MixDigest) | |
| 9.2 | parentHash | 与前一区块 hash 一致 | |
| 9.3 | stateRoot | 与 eth_getBlockByNumber.stateRoot 一致 | |
| 9.4 | transactionsRoot | 一致 | |
| 9.5 | receiptsRoot | 一致 | |
| 9.6 | number | 一致 | |
| 9.7 | gasLimit | 一致 | |
| 9.8 | gasUsed | 一致 | |
| 9.9 | timestamp | 一致 | |
| 9.10 | baseFeePerGas | pre-Vanuatu = 0 | |
| 9.11 | miner | 一致 | |
| 9.12 | logsBloom | 一致 | |
| 9.13 | nonce | 0x0000000000000000 | |
| 9.14 | difficulty | 0x0 | |
| 9.15 | blobGasUsed | 一致 | |

---

## 10. validation_hash

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 10.1 | 类型 | jq `type == "number"` | |
| 10.2 | 非零 | jq `!= 0` | |
| 10.3 | 算法验证 | SHA1(所有 id 拼接) 取末 6 位 | |
| 10.4 | 幂等 | 同一 block_id 两次调用对比值相同 | |

---

## 11. 特殊区块

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 11.1 | Genesis (block 0) | synthetic txs/traces 存在, state_diff 非空 | |
| 11.2 | 空区块 | 无 EVM tx: txs 可能仅含非 EVM action 转换 | |
| 11.3 | 多 tx 区块 | txs[].idx 从 0 递增 | |
| 11.4 | CREATE 区块 | traces 含 type="create" | |
| 11.5 | Revert 区块 | error_traces/error_events 非空 | |
| 11.6 | 高 height 区块 | 近期区块可正常回放 | |
| 11.7 | 不存在的区块 | 返回 JSON-RPC error | |
| 11.8 | 最新区块 (latest) | 返回当前链头 | |
| 11.9 | 含 staking action | txs 中含非 EVM action 的表示 (或为空) | |
| 11.10 | 连续区块 parent_id | 连续 5 个 block 的 parent_id 链验证 | |

---

## 12. 兼容性

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 12.1 | JSON 可解析 | background-tracer 反序列化 DebankOutPut | |
| 12.2 | dry-run | `background-tracer dry-run --rpc-address=... --start-block=X --end-block=X+5` | |
| 12.3 | 连续区块 parent_id 链 | 连续 5 个 block parent_id = 前一个 block.id | |
| 12.4 | 性能 | 单次调用耗时 < 5s | |

---

## 13. 非 EVM Action 验证 (IoTeX 特有)

验证 staking/reward 等非 EVM action 的余额变更是否正确反映在 state_diff 中。

| # | 测试项 | 验证方法 | 结果 |
|---|--------|---------|------|
| 13.1 | staking action 余额变更 | 找到含 stakeCreate/stakeAddDeposit 的区块，state_diff.new_accounts 中包含 staker 地址的余额变更 | |
| 13.2 | reward claim 余额变更 | 找到含 claimFromRewardingFund 的区块，state_diff.new_accounts 中包含 claimer 余额增加 | |
| 13.3 | 非 EVM action 无 traces | staking action 不产生 EVM traces (traces 中无对应条目) | |
| 13.4 | 非 EVM action 无 events | staking action 不产生 EVM events | |
| 13.5 | 混合区块 state_diff 完整 | 同一区块内含 EVM tx + staking action，state_diff 包含两者的变更 | |

---

## 14. 批量回归测试

| # | 测试项 | 覆盖区块 | 结果 |
|---|--------|---------|------|
| 14.1 | tx 数量一致 (debankBlock.txs vs eth_getBlockByNumber.transactions) | 200 blocks | |
| 14.2 | block hash 一致 | 200 blocks | |
| 14.3 | event idx 全局递增无重复 | 200 blocks | |
| 14.4 | trace 数量一致 (per tx, debankBlock vs debug_traceTransaction) | 200 blocks | |
| 14.5 | event 数量一致 | 200 blocks | |
| 14.6 | validation_hash 幂等 | 50 blocks 两次调用对比 | |
