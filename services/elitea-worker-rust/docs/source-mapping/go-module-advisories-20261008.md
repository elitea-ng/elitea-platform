# Go module advisories: cel-go, klauspost/compress, x/crypto

This record covers a dependency change in the Go services. It changes no Worker code. The Worker documentation holds it
because the delivery gate keeps every source mapping here.

## Business behavior and scope

The current platform (Python SDK, Pylon) is not involved: it has no Go modules. Nothing was ported.
The change takes upstream fixes for third-party advisories that `govulncheck` reported on 2026-10-08.
It also adds a guard test for the advisories whose fix cannot be taken yet.

| Advisory | Module (before) | Affected package and symbols (vuln.go.dev) | Modules affected | Outcome |
| --- | --- | --- | --- | --- |
| GO-2026-6094 (GHSA-gcjh-h69q-9w9g) | `github.com/google/cel-go` v0.29.0 | `cel-go/ext`: `NativeTypes`, `ParseStructTag` | elitea-main, elitea-llm-gateway | **Fixed**: v0.30.0 |
| GO-2026-5841 (GHSA-259r-337f-4rfw) | `github.com/klauspost/compress` v1.18.5 | `compress/s2`: `NewDict` | elitea-main, elitea-scheduler | **Fixed**: v1.18.7 |
| GO-2026-6354 (CVE-2026-78662) | `golang.org/x/crypto` v0.55.0 | `x/crypto/ssh`: `Dial`, `NewClientConn`, `NewServerConn`, … | elitea-main, elitea-scheduler | **Deferred**: fix v0.56.0 needs Go 1.26 |
| GO-2026-6355 (CVE-2026-56855) | `golang.org/x/crypto` v0.55.0 | `x/crypto/ssh`: `Dial`, `NewClientConn`, `NewServerConn`, … | elitea-main, elitea-scheduler | **Deferred**: fix v0.56.0 needs Go 1.26 |
| GO-2026-5932 | `golang.org/x/crypto` (every version) | `x/crypto/openpgp` and its subpackages | elitea-main, elitea-scheduler, elitea-llm-gateway | **No fix exists**: the package is unmaintained |

### Why x/crypto stays at v0.55.0 in elitea-main and elitea-scheduler

`golang.org/x/crypto` v0.56.0 and v0.57.0 declare `go 1.26.0`. No v0.55.x backport exists. elitea-main and
elitea-scheduler build on Go 1.25:

- `go.work` declares `go 1.25.8`;
- `services/elitea-main/Containerfile:38` and `services/elitea-scheduler/Containerfile:9` use the
  `golang:1.25-trixie` builder;
- CI workflows pin `go-version: "1.25"` (for example `.github/workflows/ci-go.yml:150`).

Taking v0.56.0 would mean moving both modules to Go 1.26, which needs CI workflow changes. CI workflows are out of
scope for this track (user decision 2026-10-08). The user chose to defer x/crypto to a separate Go 1.26 move.
elitea-llm-gateway already builds on Go 1.26 and has x/crypto v0.57.0, so GO-2026-6354/6355 do not apply to it.

## Changed paths

| Path | Change |
| --- | --- |
| `services/elitea-main/go.mod:28` | `github.com/google/cel-go` v0.29.0 → v0.30.0 (direct) |
| `services/elitea-main/go.mod:110` | `github.com/klauspost/compress` v1.18.5 → v1.18.7 (indirect) |
| `services/elitea-main/go.sum` | the four matching hash lines |
| `services/elitea-scheduler/go.mod:28` | `github.com/klauspost/compress` v1.18.5 → v1.18.7 (indirect) |
| `services/elitea-scheduler/go.sum` | the two matching hash lines |
| `services/elitea-llm-gateway/go.mod:11` | `github.com/google/cel-go` v0.29.0 → v0.30.0 (direct) |
| `services/elitea-llm-gateway/go.sum` | the two matching hash lines |
| `services/elitea-main/cmd/elitea-main/linked_advisory_packages_test.go` | new guard test (`x/crypto/openpgp`, `x/crypto/ssh`) |
| `services/elitea-scheduler/cmd/elitea-scheduler/linked_advisory_packages_test.go` | new guard test (`x/crypto/openpgp`, `x/crypto/ssh`) |
| `services/elitea-llm-gateway/cmd/elitea-llm-gateway/linked_advisory_packages_test.go` | new guard test (`x/crypto/openpgp`) |

`go get <module>@<version>` and `go mod tidy` ran per module with `GOWORK=off`, which matches how each Containerfile
builds. Tidy changed nothing else: no other module version moved, and the `go` directives are unchanged.
`go.work.sum` is unchanged, and the workspace build (`go build ./services/elitea-main/... ./services/elitea-scheduler/...`)
passes.

None of the other modules in `go.work` requires any of the three modules. The checked modules are `conformance/nativeclient`,
`gen/go`, `libs/go/{authlib,egresslib,eventslib,natsconn,observability,rpclib}`, `libs/proto/gen/go`,
`services/elitea-subapp-host` and `tools/uictl`. elitea-llm-gateway is outside `go.work` and was checked with
`GOWORK=off`. Its `klauspost/compress` (v1.20.0) and `x/crypto` (v0.57.0) were already past the fixed versions.

## cel-go v0.30.0 behavior review

Both CEL call sites build a plain environment of typed variables, compile, and require a `bool` output type:

- elitea-main: `internal/api/gateway/routing_cel.go:28-40` (`newRoutingCELEnv`) and `:145-165` (`CompileRoutingCEL`),
  which governance rule writes use (`governance.go:208`, `:402`);
- elitea-llm-gateway: `internal/policy/routing.go:32-42` (`celEnv`) and `:93-113` (`CompileCEL`). Its
  `Route` (`:201`) evaluates programs against a custom `Activation`.

Neither site enables `cel-go/ext` libraries, the optimizer, or `cel.bind`.

The upstream range v0.29.0…v0.30.0 has 25 commits. Here is each one that can change what these call sites observe:

| Upstream change | Effect on Elitea |
| --- | --- |
| a492a702 Expression node limits for parser and checker (#1386): new default cap of 100,000 expression nodes, including macro expansion | Stricter bound. A routing rule above 100,000 nodes now fails to compile and is refused on write (main) or at load (gateway). No realistic routing predicate approaches it. This is a new upstream resilience bound, not a regression. |
| 41d9149f `timestamp()` rejects non-RFC3339 strings (#1338) | Stricter. A stored rule using a non-RFC3339 `timestamp("…")` literal would now error at evaluation. The gateway skips an erroring rule and never treats it as a match (`routing.go:197-213`, proven by `TestErroringRuleIsSkippedNotMatched`). |
| 82a222f4 Graceful NaN ordering (#1370), af38f759 / 5933d078 / 0175dbb6 constant-folding fixes | Folding runs only with the optimizer, which is not used. NaN ordering affects only `double` comparisons against NaN; `budget_used` is never NaN. |
| be9dacd3 depth validation for `ParsedExprToAst` / `CheckedExprToAst` (#1334) | Not used. |
| e15d3ca0 `cel.bind` nesting limit (#1378) | `ext.Bindings` is not enabled. |
| 83eed56b, ecb49cce, 12328e54: JSON/native-type field handling (the GO-2026-6094 fix) | `cel-go/ext` is not linked into any Elitea binary. |
| 3d507116, 1d92000f, 270df174, 94b0cced: async/`ConcurrentEval` internals | Opt-in only; not used. |
| 36ff97de `Bytes.Add` copies operands | Correctness fix; no `bytes` variables are declared. |

The existing tests that pin the behavior, all of which pass on v0.30.0:
- `TestCompileRoutingCEL` and `TestValidateCELAction{Valid,Invalid,Malformed}` (`internal/api/gateway/governance_test.go`);
- `TestCELVariableSetsMatchTheGateway` and `TestUnevaluableCELVariablesMatchTheGateway`
  (`routing_cel_parity_test.go`);
- gateway `internal/policy/routing_test.go`: 11 tests, including `TestErroringRuleIsSkippedNotMatched` and
  `TestCompileCELRejectsNonBool`.

## Tests

Runs are from 2026-10-08 on darwin/arm64. Toolchains: go1.25.13 (elitea-main, elitea-scheduler) and go1.26.6
(elitea-llm-gateway, `GOWORK=off`). PostgreSQL tests used a throwaway `pgvector/pgvector:0.8.1-pg18` container. All
runs used `-count=1`. Counts come from `go test -json`.

| Run | Packages ok / fail | Tests pass / fail / skip | Subtests pass / fail / skip |
| --- | --- | --- | --- |
| `go vet ./...` in all three modules | clean | — | — |
| elitea-main `-race`, the 26 packages whose graph includes cel-go or compress | 26 / 0 | 2,551 / 0 / 60 | 3,239 / 0 / 0 |
| elitea-scheduler `-race`, the 2 packages whose graph includes compress | 2 / 0 | 56 / 0 / 6 | 8 / 0 / 0 |
| elitea-llm-gateway `GOWORK=off go test -race ./...` (module CLAUDE.md gate) | 19 / 0 | 1,127 / 0 / 3 | 508 / 0 / 0 |
| elitea-main `go test ./...` | 185 / 0 (22 without tests) | 7,665 / 0 / 63 | 8,130 / 0 / 4 |
| elitea-scheduler `go test ./...` | 8 / 0 (2 without tests) | 134 / 0 / 6 | 16 / 0 / 0 |
| elitea-llm-gateway `GOWORK=off go test ./...` | 19 / 0 (1 without tests) | 1,127 / 0 / 3 | 508 / 0 / 0 |
| `-race` re-run of `cmd/elitea-main` and `cmd/elitea-scheduler` after the final guard change | 2 / 0 | 106 / 0 / 6 | 151 / 0 / 0 |

Each skip needs a service this local run did not provide. None of them covers CEL or compress code:

- **NATS:** 44 skips in elitea-main, 6 in elitea-scheduler and 3 in elitea-llm-gateway.
  - These tests need `ELITEA_TEST_NATS_URL`, `ELITEA_TEST_NATS_SERVER_BIN`, `ELITEA_TEST_NATS_SECURE_CONF` or
    `ELITEA_TEST_NATS_CLI_BIN`.
  - They include the opt-in JetStream reliability and visibility-repair gates.
- **Storage emulators:** 15 elitea-main skips need `S3_ENDPOINT_URL` or the Azure/GCS emulators. This includes the
  `s3`, `azure` and `gcs` subtests of `TestArtifactConformance`.
- **Deployed stack:** 8 elitea-main contract tests need `CONTRACT_AUTH_TOKEN`.
- **Opt-in gates:** 6 elitea-main skips need an explicit flag. The flags are `ELITEA_RUNTIME_SYSTEM_TEST`,
  `ELITEA_INDEX_V2_PREFLIGHT_SYSTEM_TEST`, `ELITEA_INDEX_BINDING_CROSS_PROCESS_TEST`,
  `ELITEA_INDEX_SDK_SERIALIZATION_GATE`, `ELITEA_COMPILED_SNAPSHOT_PG_REQUIRED` and `ELITEA_TEST_LLM_BASE_URL`.
- **Editor PostgreSQL fixture:** 4 elitea-main skips need `ELITEA_EDITOR_TEST_DATABASE_URL`.
- **pgvector subtest:** 1 elitea-main subtest skips because the installed-SDK process is absent.

CI provides the NATS and emulator services (`ci-go.yml:335-400`).

`golangci-lint` is not installed locally, so the lint gate was not run here. `ci-go.yml` and `ci-gateway.yml` run it.
This is an open item, not a pass.

## Performance

- **Budget:** no runtime change. The bump must not change latency, round trips, writes or allocations on any request
  path.
- **Measured:** stripped binary sizes, built with `-trimpath -ldflags="-s -w"` as the Containerfiles do:

| Binary | Before | After |
| --- | --- | --- |
| elitea-main | 87,785,234 B | 87,853,442 B (+68,208 B, cel-go) |
| elitea-scheduler | 21,604,626 B | 21,604,626 B (unchanged) |
| elitea-llm-gateway | 33,477,842 B | 33,546,082 B (+68,240 B, cel-go) |

- CEL compile happens once per rule write (main) or rule load (gateway), and the environment is built once
  (`routing_cel.go:50-55`). The new parser node counter is O(nodes) bookkeeping on that path only. The per-request
  `Program.Eval` path changes only where the upstream commits above say.
- Each guard test runs two `go list -deps` calls, one per architecture. Measured warm: 2.3 s (elitea-main), 0.2 s
  (elitea-scheduler) and 0.7 s (elitea-llm-gateway).

## Durability

Not applicable: the change touches no durable write, checkpoint, lease, receipt or migration, so it creates no crash
window. Routing-rule rows are unchanged. v0.30.0 refuses a stored rule above 100,000 nodes. That row fails to load with a
typed compile error and is never accepted silently.

## Resilience

- The new upstream bound (100,000 expression nodes, `cel-go@v0.30.0/parser`) caps parser and checker work on
  rule text. v0.29.0 already capped the source size in code points and the recursion depth, but not the number of
  nodes that macro expansion produces.
- Evaluation errors stay contained. The gateway skips an erroring rule and reports it through `onError`
  (`routing.go:197-213`); `TestErroringRuleIsSkippedNotMatched` proves it.
- Pre-existing gap, not changed here: the governance handlers in `internal/api/gateway/governance.go:204,219` decode
  request bodies without an `http.MaxBytesReader` bound. The routes require the platform `administration` permission.
  This is recorded as a follow-up.

## Security

- **Fixed:** GO-2026-6094 and GO-2026-5841 are gone from elitea-main, elitea-scheduler and elitea-llm-gateway.
  - Neither was reachable before the change.
  - `cel-go/ext` and `compress/s2` are absent from every module's `go list -deps ./...` package graph.
  - `klauspost/compress` reaches the binaries only as `compress/flate`, through nats.go's WebSocket transport
    (`nats.go@v1.53.1/ws.go:33`).
  - `flate` and `internal/le` are byte-identical between v1.18.5 and v1.18.7, so the compress bump changes no linked
    code.
- **Deferred, enforced in code:** GO-2026-6354/6355 (`x/crypto/ssh`) and GO-2026-5932 (`x/crypto/openpgp`).
  - The linked x/crypto packages are `blake2b`, `chacha20`, `chacha20poly1305`, `cryptobyte`, `curve25519`, `hkdf`,
    `nacl/box`, `nacl/secretbox`, `pkcs12`, `salsa20` and `internal/*`. `ssh` and `openpgp` are absent from every
    module's non-test and test graphs.
  - `TestNoBinaryLinksAdvisoryPackagesWithoutFix` in each affected module fails if a banned package or any of its
    subpackages is linked into a shipped binary.
  - It runs `go list -deps ./...` over the module root with `GOOS=linux CGO_ENABLED=0` for `amd64` and `arm64`.
    This is the graph the images build.
  - It also fails if the scan did not report the module's own `cmd` package.
  - Anchors in elitea-main and elitea-scheduler `cmd/<service>/linked_advisory_packages_test.go`: ban list `:20`,
    test `:29`, helper `:34`, build environment `:43`, coverage check `:61`.
  - Anchors in the elitea-llm-gateway copy: ban list `:17`, test `:25`, helper `:30`, build environment `:39`,
    coverage check `:57`.
  - Red proof: banning `golang.org/x/crypto/nacl`, which elitea-main links, made the test fail on both architectures.
    The failure named `nacl/secretbox` and `nacl/box`. Restoring the real list made it pass.
- **GO-2026-5932 exposure assessment:** the advisory is a blanket "unmaintained, unsafe by design" notice for
  `x/crypto/openpgp/*`, and it applies to every x/crypto version.
  - No Go code in the repository imports `openpgp` or `ssh`. `git grep` finds only the guard tests' own lists.
  - No dependency of elitea-main, elitea-scheduler or elitea-llm-gateway links either package.
  - Exposure is nil while the guard holds.
  - govulncheck keeps reporting it at module level until upstream withdraws or retires the package. Binary mode
    reports it too, because stripped binaries have no symbol table.
- **Supply chain:**
  - No new dependency; only the version moves listed above.
  - Versions are exact in `go.mod`, and the hashes in `go.sum` were verified by the Go checksum database during
    `go get`.

### govulncheck v1.1.4 (DB updated 2026-10-07), before → after

| Module | Toolchain | Source mode (`./...`) before | Source mode after | `-mode binary` (stripped) before | `-mode binary` after |
| --- | --- | --- | --- | --- | --- |
| elitea-main | go1.25.13 | 5 module-level: 6354, 6355, 6094, 5932, 5841; 0 called | 3 module-level: 6354, 6355, 5932; 0 called | 5 | 3 (6354, 6355, 5932) |
| elitea-scheduler | go1.25.13 | 4 module-level: 6354, 6355, 5932, 5841; 0 called | 3 module-level: 6354, 6355, 5932; 0 called | 4 | 3 (6354, 6355, 5932) |
| elitea-llm-gateway (`GOWORK=off`) | go1.26.6 | 2 module-level: 6094, 5932; 0 called | 1 module-level: 5932; 0 called | 2 | 1 (5932) |

No new findings were added.

On a stripped binary, binary mode cannot resolve symbols, so it lists module-level matches under "Symbol Results".
The source-mode result ("0 called", "0 in packages you import") is the authoritative reachability answer.

The local default toolchain is go1.26.5. With it, elitea-llm-gateway source mode also reports eight standard-library
advisories. go1.26.6 fixes all eight: GO-2026-6218, 6090, 6089, 6088, 5972, 5026, 6091 and 5942. These are pre-existing
and toolchain-dependent. The gateway image builds from the floating `golang:1.26-trixie`
(`services/elitea-llm-gateway/Containerfile:35`), which tracks the patch series. The go1.26.6 scan shows none of them.

## Recovery guarantees

No (component × phase) guarantee changes: the change alters no durable path.

| Component × phase | Class | Basis |
| --- | --- | --- |
| Main × admission (governance rule write: CEL validation) | unchanged | Validation is a pure function called before the write (`governance.go:208`, `:402`); `TestCompileRoutingCEL` |
| LLM gateway × model call, before first token (routing decision) | unchanged | Erroring rule skipped, never matched (`routing.go:197-213`); `TestErroringRuleIsSkippedNotMatched` |
| Scheduler × all phases | unchanged | Indirect `klauspost/compress` bump only; `compress/flate` is linked, `s2` is not |
| Worker, Sandbox supervisor, NATS, PostgreSQL, Web | not touched | — |

## Real-browser evidence

Not performed. The change has no behavior visible in the UI, and the rehearsal stack was not redeployed with these
images. The CEL write path is covered by its handler tests: `TestValidateCELAction*` exercises the
`POST /governance/validate-cel` handler. The browser acceptance for the governance editor stays with the next
rehearsal deploy that includes this commit. This is an open item, not a pass.

## Fixtures

No fixtures were created. PostgreSQL integration tests ran against a throwaway local `pgvector/pgvector:0.8.1-pg18`
container, set up with CI's database names (`ci-go.yml:346-366`). No UI or database fixtures were used.

## Follow-ups

1. Move elitea-main and elitea-scheduler to Go 1.26. This covers `go.mod`, `go.work`, the Containerfile builders, and
   the CI `go-version` pins, which need user approval. Then take `golang.org/x/crypto` ≥ v0.56.0 and drop the `ssh`
   entry from both guard tests.
2. Bound the governance request bodies (`internal/api/gateway/governance.go:204,219`) with `http.MaxBytesReader`, like
   the neighbouring handlers.
3. Keep `x/crypto/openpgp` banned permanently (GO-2026-5932 has no fix).
