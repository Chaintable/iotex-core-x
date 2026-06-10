# Upstream v2.4.2 Merge — Verification Report

- **PR**: #10 (`merge/upstream-v2.4.2` → `v2.4.2`), merge commit `1652d2b57`
- **Image under test**: `blockchain/iotex-x:amd64-1652d2b5` (PR dispatch build)
- **Date**: 2026-06-10 ~ 2026-06-11
- **Verdict**: **PASS** — fit to cut release `v2.4.2-debank-1`

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
