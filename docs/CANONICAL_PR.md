# PR: trace_debankBlock canonical state_diff (replaces replay-derived path)

## What

`trace_debankBlock` 的 `state_diff` 与 `events` 主路径从「按需重放」切到「Erigon canonical changeset + post-state」。`traces` / `error_events` / `error_traces` 仍由 replay 产出（best-effort）。

## Why

经 x3 archive 上端到端实测：当前 replay-derived 路径在三类块上系统性偏离主网真实状态，无法靠继续打补丁修复（详见 `replay-vs-mainchain-divergence-analysis.md`）：

1. **Pre-Sumatra 分叉块（约 0.7%）**：跨 v1.x→v2.3.8 EVM 行为漂移。block 12,496,844 上 `0x576a4891` 偏离主网 ~30 IOTX
2. **Pre-fork 时代块（h ≲ 30M）**：replay 在 tip-context 跑、用 zero-nonce 语义；writer 当年用 legacy 语义。fresh account nonce 系统性偏离 `eth_getTransactionCount`
3. **后期偶发 drift**：block 47,487,940 上 `0xa576c141` ~0.8 IOTX 偏差

新路径以 writer 当年 commit 进 Erigon `kv.AccountChangeSet/StorageChangeSet/PlainState/kv.Code` 的事实为真相源，与 native `eth_getBalance / eth_getTransactionCount / eth_getCode` 严格一致。

## Scope of changes

### New files
- `state/factory/erigonstore/canonical_reader.go` — block-scoped reader API over Erigon
- `state/factory/erigonstore/canonical_reader_test.go` — 8 unit tests
- `state/factory/erigonstore/test_support.go` — synthetic-block helper for cross-package tests
- `api/debank_canonical_diff.go` — `buildCanonicalStateDiff` builder
- `api/debank_canonical_diff_test.go` — 8 unit tests
- `api/debank_canonical_events.go` — `buildCanonicalEvents` builder
- `api/debank_canonical_events_test.go` — 9 unit tests
- `api/debank_canonical_metrics.go` — 7 prometheus counters/histograms
- `docs/trace_debankBlock-canonical-design.md` — full design doc

### Modified files
- `state/factory/statedb.go` — exposes `(*stateDB).ErigonDB()` getter
- `api/coreservice.go` — `debankBlockImpl` rewritten to use canonical primary path; sentinel errors
  + replay-status capture + diverged metric; pool synthetic-account code deleted (~170 lines)
- `api/web3server.go` — no special error code mapping; ETL auto-retries on any error (transient errors recover, persistent errors block ETL pipeline so a human investigates)

### Replaced code
- `addProtocolPoolSyntheticAccounts` / `addSyntheticAccount` / `readStakingTotalAmount`
  in `coreservice.go` (deleted ~130 lines) → re-implemented in `api/debank_canonical_pool.go`
  (~210 lines including dedup-via-upsert logic). The replacement uses the CORRECT pool
  addresses (see "Behavior changes" below) — the legacy code injected at
  `RewardingProtocolAddrHash` / `StakingProtocolAddrHash` (returns 0 from native), the new
  code injects at `keccak256("<name>")[12:]` (matches native eth_getBalance routing).
- The merge logic that combined collector + per-action EVM diffs (~40 lines deleted)

## Behavior changes

| Field | Before (replay) | After (canonical) | Net effect |
|---|---|---|---|
| `state_diff.NewAccounts` | replay collector | Erigon AccountChangeSet + PlainState(N+1) | balance/nonce/codeHash now match `eth_getBalance/Nonce/Code` exactly |
| `state_diff.DeletedAccounts` | replay collector | post-state == nil for changed addr | unchanged for happy path |
| `state_diff.StorageDiff` | replay collector | StorageChangeSet + PlainState(N+1) | identical for non-divergent blocks; correct for divergent |
| `state_diff.NewCodes` | replay collector | kv.Code by codeHash | byte-identical when present |
| `state_diff.{Hash, ParentHash}` | block header DeltaStateDigest | same (unchanged) | — |
| `block_file.events` | replay tracer | dao.GetReceipts + emitTransferLogsAsEvents | matches `eth_getTransactionReceipt`. Synthetic logs use `account.ProtocolAddr` |
| `block_file.traces` | replay tracer | replay tracer (best-effort) | unchanged; replay-status mismatch logged via metric |
| `block_file.storage_contracts` | replay-derived | StorageChangeSet addr set | identical for non-divergent |
| `pool synthetic accounts` | injected at `0x1b4f3289` / `0xda39d866` (wrong — returns 0 from native) | injected at `0xa576c141` (rewarding) / `0x04c22afa` (staking) = `keccak256(<name>)[12:]` (matches native routing); upserts to override historical canonical entry with tip snapshot | leafage now mirrors native `eth_getBalance(<pool>, h)` (always tip) for both pool addrs |

## ETL error handling

ETL (Rust `background-tracer`) auto-retries on any RPC error and blocks the pipeline on persistent failure. This is the desired behavior for ALL canonical-path errors:

- `ErrCanonicalNotFinalized`: transient (writer tip catches up in seconds) → retries succeed automatically
- `ErrHistoryUnavailable` / `ErrCanonicalCodeMissing`: persistent → retries exhaust → ETL blocks → human investigates
  - `Skip` would silently lose data; `block` forces operator attention to fix root cause

Writer side does NOT do special error-code mapping. All sentinels propagate as generic JSON-RPC `-32603 Internal error`. Differentiation for ops dashboards is via prometheus counters:
- `trace_debankblock_canonical_history_unavailable_total`
- `trace_debankblock_canonical_code_missing_total`

## Bugs fixed (incidentally)

The canonical path automatically resolves the following previously-tracked replay-only bugs:
- Bug A (sender nonce off-by-one) — confirmed correct on x3 strict eval
- Bug B (fresh EOA receiver nonce=1) — confirmed correct on x3 strict eval
- ~10 commits worth of replay-state-capture fixes (`be0e31e7` / `9757f5a1` / `dc9cd4f9` / `6b716253` / `11d7923e` / `dd6b6162` / `f8104506` / etc.) become irrelevant because state_diff no longer depends on replay correctness

## Tests

- 25 new unit tests pass (`go test ./api/ ./state/factory/erigonstore/ -run "TestCanonical|TestBuildCanonical"`)
- Integration test script: `verify_canonical_block.py` — three-way comparison vs old writer + native eth_*. **Not yet run** (requires deploying new writer instance to x3, which is out of scope per current rollout plan)

## Out of scope

- Deploying new writer / ETL / leafage on x3 (per "只写代码" instruction)
- Rust `background-tracer` ETL retry logic (separate repo, future work)
- Backfilling existing leafage data to remove pre-fork nonce drift (deployment-time decision)

## Known caveats

### Trace 内部一致性局限（最 tricky 的一点，⚠️ 必读）

新方案能保证 **state_diff / events / tx 顶层字段 / 顶层 trace** 全部 canonical 准确，**但 trace 内部调用细节**（frame 级 gas、CALL value、子调用成败、内部 LOG 触发）即使在 status 一致的非分叉 tx 上也可能与主网漂移。

**为什么**：
- canonical traces 不存在（iotex receipts 只有 logs，没有 frame 信息）
- 唯一拿到正确内部 trace 的办法是用 v1.x iotex-core binary 重跑——已否决
- 当前 traces 仍由 v2.3.8 replay 派生，内部 EVM drift 不可消除

**已做到**：
- 顶层 status 翻转的 tx：drop replay trace tree → append canonical-minimal trace（单 frame、tx-level 全对、Subtraces=0）
- 顶层 status 一致的 tx：保留 replay trace tree 作 best-effort

**约束下游**：
- 资金/余额 → 用 `state_diff`（100% canonical）
- token transfer 检测 → 用 `events`（100% canonical），traces 仅老式 token 的辅助
- internal call 关系图 → traces，best-effort，分叉块只有顶层 frame

详细分析见设计文档 §9.7。

### 其他

- `selfdestruct` path is standard Erigon semantics but lacks real-data sample on iotex; covered by mock unit test only
- Pool addresses (`RewardingProtocolAddrHash` = `0x1b4f3289...`, `StakingProtocolAddrHash` = `0xda39d866...`) — these are the iotex-native protocol IDs but native `eth_getBalance` returns 0 for them. Canonical state_diff also returns 0 (no synthesis). This matches native behavior.
- Pool addresses `0xa576c141` (rewarding, = `keccak256("rewarding")[12:]`) and `0x04c22afa` (staking, = `keccak256("staking")[12:]`) — native `eth_getBalance` routes these to pool ReadState; new canonical path synthesizes both with tip-snapshot balance to match.
- `0xa576c141` is ALSO written by GrantReward via accountstorage.Store every block. The synthesis upserts (overrides) the historical canonical entry with tip-snapshot to preserve tip-only semantics across all heights.
