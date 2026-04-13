# trace_debankBlock 实现检查项

## 实现细节 TODO

- [ ] **OnLog 传播**: StateDBAdapter.AddLog() 需传播到 callTracer.OnLog()，否则 BlockFile 中 events 缺失。确认传播路径：StateDBAdapter → logHook → RPCTracer → callTracer.OnLog()
- [ ] **BaseFee nil panic**: pipeline 的 `BuildPipelineTransaction` 访问 `BaseFeePerGas.ToInt()` 会 panic。所有路径确保 BaseFee 非 nil（pre-Vanuatu fork 设为 0）
  - buildSyntheticGethBlock 中设置
  - buildPipelineHeader 中设置
  - OnTxEnd 调用 BuildPipelineTransaction 时传入的 baseFee 参数

## 验证检查项

- [ ] `make build` 编译通过
- [ ] StateDiff() 单元测试：已知合约状态变更，验证输出格式
- [ ] mergeStateDiffs() 单元测试：重叠 key 合并
- [ ] 集成测试：在 archive 节点调用 trace_debankBlock
- [ ] 对比 BlockFile txs/traces/events 数量与区块实际交易数
- [ ] StateDiff RLP 可被 leafage-evm 正确解析
- [ ] ValidationHash 计算一致性
- [ ] 回归：现有 TraceTransaction 和实时 pipeline hooks 不受影响
- [ ] 非 EVM action（staking/reward）的余额变更出现在 StateDiff accounts 中
