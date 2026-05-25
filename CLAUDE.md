# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

iotex-core is the official Go implementation of the IoTeX protocol, a modular DePIN (Decentralized Physical Infrastructure Networks) Layer-1 blockchain. It uses Roll-DPoS consensus, embeds a forked go-ethereum EVM for smart contract execution, and integrates Erigon for state storage optimization.

## Build & Development Commands

```bash
make                    # Build server + ioctl CLI (build tag: nosilkworm)
make build-all          # Build all targets (server, actioninjector, addrgen, minicluster, etc.)
make ioctl              # Build CLI tool only

make test               # Format + unit tests (with race) + e2e tests (without race)
make test-unit          # Unit tests only (with race detector)
make test-e2e           # E2E tests only (without race detector)

# Run a single test:
CGO_ENABLED=1 go test -v -run TestFunctionName ./path/to/package

make lint               # go vet on all packages
make lint-rich          # golangci-lint with full checks
make fmt                # go fmt + go mod tidy

make run                # Build and run standalone node
make reboot             # Clean databases and restart from scratch
make minicluster        # Run local multi-node test cluster
make mockgen            # Regenerate mocks (misc/scripts/mockgen.sh)
make docker             # Build Docker image
```

## Go Module

- Module: `github.com/iotexproject/iotex-core/v2`
- Go version: 1.23.0, toolchain: go1.23.7
- Build tag: `nosilkworm` (always applied)

## Key Dependency Forks

The project replaces upstream go-ethereum with an IoTeX-maintained fork:
```
replace github.com/ethereum/go-ethereum => github.com/iotexproject/go-ethereum
```
The master branch and release branches may use different fork versions (master tracks Pectra/EIP-7702 development, release branches stay on stable versions). Always check `go.mod` on the current branch.

Erigon is integrated for state storage: `github.com/erigontech/erigon` and `github.com/erigontech/erigon-lib`.

## Architecture

### Entry Points
- **Node server:** `server/main.go` - loads config/genesis, creates `itx.Server`, starts all services
- **CLI tool:** `tools/ioctl/ioctl.go`

### Service Orchestration
`chainservice.Builder` wires together the core components:
```
ChainService
  ├── Blockchain (block management, context creation)
  ├── Factory (state management, WorkingSet pattern)
  ├── ActPool (mempool, action validation)
  ├── Consensus (Roll-DPoS with FSM)
  ├── BlockSync (block synchronization)
  ├── BlockDAO (block persistence, PebbleDB)
  └── API (gRPC + Web3 JSON-RPC dual interface)
```

### Protocol Registry Pattern
Core extensibility mechanism. Each protocol registers handlers for specific action types:
- **Interface:** `action/protocol/protocol.go` - `Protocol`, `ActionHandler`, `StateReader`
- **Key protocols:** Account, Execution (EVM), Staking, Rewarding, Poll (delegate election), RollDPoS
- Protocols are registered in `protocol.Registry` and dispatched during block processing

### EVM Integration (`action/protocol/execution/evm/`)
- `StateDBAdapter` implements geth's `vm.StateDB`, bridging IoTeX state to the EVM
- `ErigonStateDBAdapter` wraps `StateDBAdapter` with Erigon's `IntraBlockState` for performance
- Local `stateDB` interface extends `vm.StateDB` with `CommitContracts()`, `Logs()`, `TransactionLogs()`, `clear()`, `Error()`
- `tracer.go` handles `TraceStart`/`TraceEnd` lifecycle for debug tracing
- Release branches use old `vm.EVMLogger` (CaptureStart/CaptureState), master uses new `tracing.Hooks` (OnTxStart/OnOpcode)

### State Management (`state/factory/`)
- `Factory` interface: `WorkingSet()`, `WorkingSetAtHeight()`, `PutBlock()`, `Mint()`, `Validate()`
- `StateManager` for read/write state operations within a block
- Merkle Patricia Trie in `db/trie/mptrie/`
- Erigon-based storage backend in `state/factory/erigonstore/`

### API Layer (`api/`)
- `coreservice.go` - core business logic, shared by both API interfaces
- `grpcserver.go` - native IoTeX gRPC API (iotex-proto definitions)
- `web3server.go` - Ethereum-compatible JSON-RPC (eth_*, debug_traceTransaction, etc.)
- `web3server_utils.go` - parsing helpers, tracer config, type conversions

### P2P Networking (`p2p/`)
Uses libp2p (not devp2p). Topics: broadcast, consensus, action propagation, unicast.

### Consensus (`consensus/`)
Roll-DPoS: delegate rotation per epoch, FSM-based block proposal/endorsement flow (PROPOSE -> LOCK -> COMMIT).

## Code Conventions

- **Error wrapping:** `github.com/pkg/errors`
- **Logging:** `go.uber.org/zap` structured logging, accessed via `pkg/log`
- **Testing:** `github.com/stretchr/testify` (require/assert), `go.uber.org/mock` for mocks
- **Context:** always first parameter; protocol contexts (`BlockCtx`, `ActionCtx`, `FeatureCtx`) passed via `context.WithValue`
- **Line length:** 140 max
- **Cyclomatic complexity:** 15 max
- **Function length:** 150 lines / 50 statements max
- **Imports:** local prefix `github.com/iotexproject/iotex-core`
- **License:** Apache 2.0 header required on all files (`make license`)

## Test Patterns

- E2E tests live in `e2etest/` (run without race detector due to performance)
- Unit tests colocated with source in `*_test.go`
- Test packages excluded from builds: anything matching `pb$|testdata|mock`
- Mocks generated via `make mockgen` -> `misc/scripts/mockgen.sh`
- `testutil/` provides shared test helpers

## Configuration

- Standalone config: `config/standalone-config.yaml`
- Genesis config: `config/standalone-genesis.yaml`
- Feature flags are height-gated in genesis config (hard forks activated at specific block heights)
