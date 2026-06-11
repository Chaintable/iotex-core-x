# Upstream v2.4.2 Merge — Verification Report

- **PR**: #10 (`merge/upstream-v2.4.2` → `v2.4.2`), merge commit `1652d2b57`
- **Image under test**: `blockchain/iotex-x:amd64-1652d2b5` (PR dispatch build)
- **Date**: 2026-06-10 ~ 2026-06-11
- **Verdict**: **PASS** — fit to cut release `v2.4.2-debank-1`

## Impact assessment (merge analysis)

### Conflict surface

34 git conflicts + 1 semantic conflict git could not see:

- **26 add/add** — all in the fork-first features upstream adopted in
  #4816/#4817 (`ioswarm/` ×24, `server/itx/admin_producer_keys.go`,
  `tools/snapshotexporter/snapshot.go`). Resolved to upstream: differences
  were gofmt formatting plus upstream improvements (exported `JSONCodec`
  keeping our `ioswarm-json` codec name; producer-keys endpoint +97 lines
  of token-auth hardening). Fork-only post-adoption increments
  (`40676b838`) deliberately dropped — see release notes section.
- **8 content** — resolved by patch ownership: kept fork `ErigonDB()`
  accessor (`statedb.go`) and `stateDiffCollector` field
  (`workingset.go`); took upstream for the producer-keys/coordinator
  overlap files (`nodeinfo/manager.go`, `heartbeat.go`, `server.go`,
  `workingsetstore.go`, `pkg/log/log.go` — fork-only
  `SetDynamicFields`/`dynamicFieldsCore` log machinery removed, superseded
  by upstream explicit rotation log lines; no debank functionality
  depended on it) and for `evm.go` (upstream promoted `AccessedSlots()`
  into the `stateDB` interface, making the fork type-assertion switch over
  Erigon wrappers unnecessary via embedded-method promotion).
- **1 duplicate declaration** — `StateDBAdapter.AccessedSlots` added by
  both sides at different locations in `evmstatedbadapter.go`; git merged
  cleanly, Go failed to compile. Removed the fork copy (implementations
  were byte-identical).

Net consensus-layer delta vs upstream after merge: 2 lines
(`TipInfo.StateDigest`, consumed by the pipeline). `go.mod` merged with
zero drift (`Chaintable/pipeline` pin and `go-ethereum-iotex` replace
intact; `go mod tidy` produced no diff).

### Forward impact: upstream changes → pipeline collection

- **`ExtractRevertMessage` hardening (#4844)** — the one semantic change
  touching our data surface. Old code: selector-matched but ABI-malformed
  revert payloads hit an unchecked slice and **panicked**; new code
  returns an error and `ExecuteContract` invalidates the action.
  Replay-safe: a block containing such a payload would have crashed every
  v2.4.1 node, so none exists on chain — historical replay and receipt
  semantics for well-formed reverts are unchanged. Behavior change is
  limited to simulate-family APIs (error instead of junk-hex receipt
  message). Callers (`erigonstore/contract_backend.go` ×2, `evm.go`)
  were all adapted by upstream itself.
- **`AddLog` empty-topics guard (#4844)** — defensive only; the
  `IN_CONTRACT_TRANSFER` path our transfer-log collection relies on is
  unchanged (previously an empty-topics log would panic; now it appends
  normally).
- **`AccessedSlots` interface promotion** — functionally equivalent to
  the fork's explicit adapter chain; Erigon dryrun wrappers satisfy the
  interface via embedding.
- **`db_bolt.ForEach` / `kvstorewithbuffer`** — additive, no behavior
  change on existing paths.
- **Mint-path changes (#4839/#4840)** — pre-mint is auto-disabled on our
  form factor (`historyIndexPath` set), removing a spurious error and the
  mint path entirely from our nodes; no effect on collection, which hooks
  block commit, not drafts.

### Reverse impact: debank patches → chain correctness

- Pipeline hooks are injected at three sites: `statedb.Validate`,
  `statedb.Mint`, `statedb.PutBlock` (collector, fresh per call) and
  `blockchain.MintNewBlock`/`commitBlock` (`bc.logger`, a stateless
  `*tracing.Hooks` function table).
- Under the new mint panic-recover (#4840), a draft can now abort mid-hook
  sequence (e.g. `OnTxStart` without `OnTxEnd`). The per-context collector
  is discarded with the panicked draft (no leak); the shared hook table
  holds no state itself. This path is reachable only on delegate
  (block-producing) form factors — on our api/archive nodes #4839 turns
  pre-mint off, so the mint path does not run at all. Recorded as a known
  boundary for any future delegate-form deployment.
- No fork patch sits on the new recover/premint-gate code paths;
  `consensus/` carries only the 2-line `StateDigest` delta.

## Test setup

Snapshot-restored archive writer (production data volume clone), run as a
backup-form node: `node` container only — no etl sidecar (so nothing is
written to the production pipeline), no jrpcx. Entrypoint, config and
genesis identical to the production StatefulSet (config/genesis read from
the data volume, `-plugin=gateway`). `historyIndexPath` configured
(archive/api form factor), `ioswarm.enabled=false`, admin port disabled,
no producer keys.

## Build & unit tests

- `go build ./...`, `go vet`, `gofmt` on hand-merged files: clean
- `go mod tidy`: zero diff (`Chaintable/pipeline` pin and
  `go-ethereum-iotex` replace intact; `iotex-proto` v0.6.6)
- `go test` on affected packages (`evm`, `state/factory`, `erigonstore`,
  `nodeinfo`, `server/itx`, `blockchain/...`, `rolldpos`, `db`, `ioswarm`,
  `api`): pass
- Pre-existing (baseline) failures, identical on the pre-merge `v2.4.1`
  line, not introduced by this merge: `blockchain/blockdao`
  (`Test_blockDAO_Stop`, `TestBlockIndexerChecker_CheckIndexer`, flaky
  `Test_blockDAO_checkIndexers`/`_Start`) and
  `api/TestEstimateExecutionGasConsumption` (nil-pointer in test mock)

## Sync verification

- Catch-up: ~75k blocks synced by the new binary (start 48,991,310 →
  head), entirely under post-Yap (48,985,561) rules; ~10–12 blk/s once the
  restored volume was hydrated (initial slowness was EBS snapshot
  lazy-restore, not the binary)
- Steady state: full lockstep with the official Babel RPC — sampled lag
  `1,0,0,0,0,0` over 6 probes; `eth_syncing=false`
- Stability: 0 container restarts, 0 error lines in sampler window,
  103 one-minute samples collected
- Five checks (startup clean / samples collected / head growing /
  not syncing / lag within limit): **all PASS**

## Hash sampling vs official RPC (`babel-api.mainnet.iotex.io`)

| Range | Blocks | Result |
|---|---|---|
| 49,000,000–49,000,019 (catch-up segment) | 20 | 20/20 MATCH |
| 49,066,480–49,066,499 (near-head, fresh blocks) | 20 | 20/20 MATCH |

## Operator-visible notes for this release

- `#4839`: pre-mint is now auto-disabled on nodes with `historyIndexPath`
  set (our form factor) — the spurious
  `KVStore() not supported in *workingSetStoreWithSecondary` mint error
  disappears
- `#4844`: admin mux binds `127.0.0.1` only (we run with admin port
  disabled — no impact); `ExtractRevertMessage` hardened — simulate-family
  APIs now return an error for malformed `Error(string)` revert payloads
  instead of a junk-hex receipt message (replay-safe: the old code panicked
  on such payloads, so no historical block contains one)
- ioswarm converged to the upstream-adopted version; the fork-only
  post-adoption increments (`40676b838`: 32MB gRPC message cap, nonce-race
  shadow exclusion, rewards API) were dropped by decision — cherry-pick
  back if ioswarm testing on this fork resumes
