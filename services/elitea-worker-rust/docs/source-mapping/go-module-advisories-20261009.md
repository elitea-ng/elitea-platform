# Go module advisories: x/text in elitea-subapp-host, x/net in elitea-llm-gateway

This record covers a dependency change in the Go services. It changes no Worker code. The Worker documentation holds it
because the delivery gate keeps every source mapping here. It continues
[go-module-advisories-20261008.md](go-module-advisories-20261008.md).

## Business behavior and scope

The current platform (Python SDK, Pylon) is not involved: it has no Go modules. Nothing was ported.
The change takes upstream fixes for advisories that `govulncheck` reported against merged `main` at `0def77b22`. The
vulnerability database was updated on 2026-10-08 22:31 UTC.

| Advisory | Module (before) | Called path (govulncheck source mode) | Modules affected | Outcome |
| --- | --- | --- | --- | --- |
| GO-2026-6629 | `golang.org/x/text` v0.39.0 | `spi.NewPostgresStore` → `pgxpool.New` → … → `precis.Profile.String` | elitea-subapp-host | **Fixed**: v0.41.0 |
| GO-2026-6617, 6612, 6611, 6610, 6603 | `golang.org/x/net` v0.58.0 | HTTP/2 server and transport (`http2.ConfigureServer`, `grpc.Server.Serve` → `http2.Framer.*`) | elitea-llm-gateway | **Fixed**: v0.60.0 |
| GO-2026-6617, 6612, 6611, 6610, 6603 | `golang.org/x/net` v0.58.0 | the same HTTP/2 paths | elitea-main, elitea-scheduler, `libs/go/observability`, `libs/proto/gen/go`, `gen/go`, `conformance/nativeclient` | **Deferred**: v0.60.0 needs Go 1.26 |

The x/text call path ends in pgx's SCRAM SASLprep: `pgx/v5@v5.9.2/pgconn/auth_scram.go:197` calls
`precis.OpaqueString.String(password)`. The input is the password in the operator-supplied DSN
(`services/elitea-subapp-host/internal/spi/pgstore.go:53`). It is not request data.

### Why x/net stays at v0.58.0 in the go.work modules

`golang.org/x/net` v0.60.0 declares `go 1.26.0`. Its go.mod also requires x/crypto v0.57.0, x/sys v0.48.0,
x/term v0.46.0 and x/text v0.42.0. In elitea-main, `go get golang.org/x/net@v0.60.0` moved the `go` directive from
1.25.13 to 1.26.0. That is the same constraint that deferred x/crypto on 2026-10-08:

- `go.work:1` declares `go 1.25.13`;
- `services/elitea-main/Containerfile:38` and `services/elitea-scheduler/Containerfile:9` build from
  `golang:1.25-trixie`, which sets `GOTOOLCHAIN=local`;
- CI workflows pin `go-version: "1.25"` (for example `.github/workflows/ci-go.yml:150`), and
  `scripts/contract/check_compiled_snapshots_postgres.sh:15` exports `GOTOOLCHAIN=local`.

CI workflows are out of scope for this change. The user chose to take the two fixes that need no toolchain move now, and
to defer x/net in the go.work modules to the Go 1.26 move (user decision 2026-10-09).

elitea-subapp-host is not affected by this constraint. x/text v0.41.0 declares `go 1.25.0`, and the module's
`go 1.25.13` directive is unchanged. elitea-llm-gateway already declares `go 1.26.6` and builds from
`golang:1.26-trixie` (`services/elitea-llm-gateway/Containerfile:38`).

## Changed paths

| Path | Change |
| --- | --- |
| `services/elitea-subapp-host/go.mod:12` | `golang.org/x/text` v0.39.0 → v0.41.0 (indirect, via pgx) |
| `services/elitea-subapp-host/go.mod:11` | `golang.org/x/sync` v0.21.0 → v0.22.0 (indirect). This is the minimum that x/text v0.41.0 requires. |
| `services/elitea-subapp-host/go.sum` | the four matching hash lines |
| `services/elitea-llm-gateway/go.mod:88` | `golang.org/x/net` v0.58.0 → v0.60.0 (indirect) |
| `services/elitea-llm-gateway/go.sum` | the two matching hash lines |

`go get <module>@<version>` and `go mod tidy` ran per module with `GOWORK=off`, which matches how each Containerfile
builds. Tidy changed nothing else. No other module version moved, and both `go` directives are unchanged.

`go.work.sum` is unchanged. Local workspace commands rewrite it as a side effect, and CI does not gate on it.
`go work sync` was not run, because it would push minimum versions into unrelated modules. The workspace build
(`go build ./services/elitea-subapp-host/... ./services/elitea-main/... ./services/elitea-scheduler/...`) and
`go vet ./services/elitea-subapp-host/...` pass in workspace mode.

## Tests

Runs are from 2026-10-09 on darwin/arm64. The toolchains were go1.26.5 (elitea-subapp-host, `GOTOOLCHAIN=local`) and
go1.26.6 (elitea-llm-gateway). All runs used `GOWORK=off` and `-count=1`. Counts come from `go test -json`.

| Run | Packages ok / fail | Tests pass / fail / skip | Subtests pass / fail / skip |
| --- | --- | --- | --- |
| `go vet ./...` in both modules | clean | — | — |
| elitea-subapp-host `go test ./...`, with `ELITEA_SUBAPP_HOST_TEST_DSN` set | 5 / 0 (5 without tests) | 168 / 0 / 8 | 93 / 0 / 0 |
| elitea-llm-gateway `go test ./...` | 19 / 0 (1 without tests) | 1,121 / 0 / 9 | 508 / 0 / 0 |

The DSN pointed at a throwaway `pgvector/pgvector:0.8.1-pg18` container whose `pg_hba` uses `scram-sha-256` for TCP.
The three PostgreSQL store tests (`internal/spi/pgstore_test.go:83`, `:134`, `:171`) therefore authenticate through the
exact `pgxpool.New` → SCRAM → `precis` path of GO-2026-6629, on x/text v0.41.0. Without the DSN they skip.

Each remaining skip needs something this local run did not provide. None of them covers x/text or x/net code:

- **elitea-subapp-host, 8 skips:** the native-engine tests in `internal/apps/{deepwiki,inventory}/run` need
  `ELITEA_DEEPWIKI_NATIVE_ENGINE_BIN` or `ELITEA_INVENTORY_NATIVE_ENGINE_BIN`. `ci-deepwiki-engine.yml` and
  `ci-inventory-engine.yml` build the Rust engines and run them.
- **elitea-llm-gateway, 9 skips:**
  - 6 PostgreSQL tests (`cmd/elitea-llm-gateway` budget probe; `internal/failmode`);
  - 3 secured-NATS tests (`internal/infra/nats`).

`golangci-lint` is not installed locally, so the lint gate was not run here. `ci-go.yml` and `ci-gateway.yml` run it.
This is an open item, not a pass.

## Performance

- **Budget:** no runtime change. The bump must not change latency, round trips, writes or allocations on any request
  path.
- **Measured:** stripped `linux/amd64` binary sizes, built with `-trimpath -ldflags="-s -w"` as the Containerfiles do:

| Binary | Before | After |
| --- | --- | --- |
| elitea-subapp-host | 11,796,642 B | 11,796,642 B (unchanged size; the build info names x/text v0.41.0) |
| elitea-llm-gateway | 37,839,010 B | 37,851,298 B (+12,288 B) |

- SASLprep runs once per new pooled PostgreSQL connection, never per request. The HTTP/2 changes in x/net v0.60.0 add
  checks on frames and headers, and do not add round trips.

## Durability

Not applicable. The change touches no durable write, checkpoint, lease, receipt or migration, so it creates no crash
window. The subapp-host invocation store keeps its schema and its write path. The three real-PostgreSQL store tests
above prove round-trip, drain-once and orphan reconcile on the new version.

## Resilience

- GO-2026-6629: a crafted password string no longer panics `precis` during connection setup. Before the fix, a
  malformed DSN password could crash `NewPostgresStore` at start-up instead of returning the typed
  `invocation store: …` error (`pgstore.go:53-55`).
- The x/net HTTP/2 fixes bound server memory and CPU against hostile peers. They cover trailer headers, repeated
  window updates and flow-control refunds, and they reject malformed framing headers. They apply to the gateway's
  HTTP/2 and gRPC listeners.

## Security

- **Fixed:** GO-2026-6629 is gone from elitea-subapp-host. GO-2026-6617/6612/6611/6610/6603 are gone from
  elitea-llm-gateway at module level.
- **Deferred:** GO-2026-6617/6612/6611/6610/6603 remain called in elitea-main, elitea-scheduler,
  `libs/go/observability`, `libs/proto/gen/go`, `gen/go` and `conformance/nativeclient` until the Go 1.26 move. They are
  listed under Follow-ups.
- **Supply chain:**
  - No new dependency; only the version moves listed above.
  - Versions are exact in `go.mod`, and the Go checksum database verified the `go.sum` hashes during `go get`.

### govulncheck v1.1.4 (DB updated 2026-10-08 22:31 UTC), before → after

| Module | Toolchain | Source mode, called module findings before | After | `-mode binary` (stripped) module findings before | After |
| --- | --- | --- | --- | --- | --- |
| elitea-subapp-host | go1.26.5 | 1: GO-2026-6629 (x/text) | 0 | 1: 6629 | 0 |
| elitea-llm-gateway | go1.26.6 | 5: GO-2026-6617, 6612, 6611, 6610, 6603 (x/net) | 0 | 6: the five x/net + 5932 | 1: 5932 |

No new findings were added.

GO-2026-5932 (`x/crypto/openpgp`, no fix exists) is the pre-existing module-level match that
[go-module-advisories-20261008.md](go-module-advisories-20261008.md) records. Its guard test keeps that package out of
the binaries.

**Pre-existing standard-library findings depend on the toolchain and are out of scope.** In source mode,
elitea-subapp-host reports 15 standard-library findings both before and after. elitea-llm-gateway reports 9 both
before and after. The gateway's 9 include the bundled-HTTP/2 copies of 6617/6612/6611/6610/6603 in `net/http`. Before
the bump govulncheck listed those under x/net; after it lists them under `net/http`. The standard-library findings are
fixed in go1.26.6 or go1.26.9, which are toolchain releases and not module versions. For the gateway they follow the
floating `golang:1.26-trixie` builder. For the go.work modules they belong to the Go 1.26 move.

## Recovery guarantees

No (component × phase) guarantee changes, because the change alters no durable path.

| Component × phase | Class | Basis |
| --- | --- | --- |
| Sub-app host × admission (invocation store open and write) | unchanged | `NewPostgresStore` returns a typed error on connect failure (`pgstore.go:53-55`); the store tests at `pgstore_test.go:83`, `:134` and `:171` pass on real PostgreSQL |
| LLM gateway × model call (HTTP/2 and gRPC listeners) | unchanged | Upstream protocol hardening only; all 19 gateway test packages pass |
| Main, Worker, Scheduler, Sandbox supervisor, NATS, PostgreSQL, Web | not touched | — |

## Real-browser evidence

Not performed. The change has no behavior visible in the UI. The rehearsal stack was not redeployed with these images,
and no image was built. The affected paths are covered by the real-PostgreSQL store tests (subapp-host) and the
gateway's test suite. Confirmation on a deployed stack stays with the next rehearsal deploy that includes this commit.
This is an open item, not a pass.

## Fixtures

No fixtures were created. The PostgreSQL tests ran against a throwaway local `pgvector/pgvector:0.8.1-pg18` container,
which was removed after the run. No UI or database fixtures were used, and no shared stack was touched.

## Follow-ups

1. Move the go.work modules to Go 1.26. This covers `go.work`, the `go` directives, the elitea-main and
   elitea-scheduler Containerfile builders, and the CI `go-version` pins, which need user approval. Then take
   `golang.org/x/net` v0.60.0 in elitea-main, elitea-scheduler, `libs/go/observability`, `libs/proto/gen/go`, `gen/go`
   and `conformance/nativeclient`, together with the deferred `golang.org/x/crypto` ≥ v0.56.0 from 2026-10-08.
