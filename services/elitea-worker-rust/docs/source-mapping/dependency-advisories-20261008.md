# Dependency advisories — Go standard-library floor, h2, chacha20, rsa (2026-10-08)

Branch `fix/dependency-advisories-20261008`, based on `origin/main` `fcf86c31`. Brief:
`handoffs/point5-wave1-20261008/00-COMMON.md` plus the 2026-10-08 audit findings. No CI workflow was changed (user
decision 2026-10-08).

## Business behaviour

No product behaviour changes. This slice changes which toolchain and crate versions the shipped images are built
with, and proves one existing security property of the SQL toolkit.

- The current platform has no equivalent: Pylon images take whatever Python and library versions their base images
  carry. Nothing was ported.
- Deliberately not done:
  - Bumping the Go series. Go 1.25 still has a fixed patch (1.25.13); the workspace stays on the 1.25 series, as
    the gateway Containerfile already requires.
  - Bumping third-party Go modules. govulncheck reports them as present but not called; see Open limits.
  - An accepted-risk `ignore` for RUSTSEC-2023-0071 (user decision 2026-10-08: mitigation only, so the finding stays
    visible to every future scan).

## Source changes

| Path | Change |
| --- | --- |
| `go.work:1` | `go 1.25.8` → `go 1.25.13` |
| `services/elitea-main/go.mod:3` | `go 1.25.8` → `go 1.25.13` |
| `services/elitea-scheduler/go.mod:3` | `go 1.25.0` → `go 1.25.13` |
| `services/elitea-subapp-host/go.mod:3` | `go 1.25.0` → `go 1.25.13` |
| `services/elitea-llm-gateway/go.mod:3` | `go 1.26.5` → `go 1.26.6` |
| `services/elitea-main/tests/buildcontext/toolchain_floor_test.go:36-37,82` | Floors `stdlibFloorGo125`, `stdlibFloorGo126` and the gate test (versions compared with the standard library's `go/version`) |
| `scripts/ci/check-gateway-toolchain.sh` (section 4) | Reads `stdlibFloorGo126` from the Go test and fails when the gateway's `go` directive is below it. ci-gateway runs this script; ci-go's paths do not cover the gateway. |
| `services/elitea-llm-gateway/Containerfile:5`, `deploy/docker-compose.standalone-full.yml:506` | Comments no longer name 1.25.8 |
| `services/elitea-worker-rust/Cargo.lock:676,1420` | `chacha20` 0.10.1 → 0.10.2, `h2` 0.4.15 → 0.4.16 (version and checksum only) |
| `services/elitea-worker-rust/src/toolkits/families/sql/client.rs:30-34,244` | `MYSQL_TLS_MODE` constant (`VerifyIdentity`), used by `mysql_options` |
| `services/elitea-worker-rust/src/toolkits/sql_tests.rs:696-860` | Fake MySQL server and two protocol tests |

### Why the `go` directive and not an image pin

The builders stay on series tags (`golang:1.25-trixie`, `golang:1.26-trixie`). An exact patch pin freezes the standard
library; the gateway Containerfile documents that decision (#433, #506). A series tag alone guarantees nothing, though:
a stale cached builder compiles with whatever patch it holds. The `go` directive turns the floor into a guarantee.
- The golang image sets `GOTOOLCHAIN=local`, so an older toolchain refuses the module (measured below).
- With `GOTOOLCHAIN=auto` (setup-go, developer machines), Go fetches the floor toolchain instead.

Either way, no binary is built on a standard library below the floor. Library modules (`libs/go/*`, `gen/go`,
`tools/uictl`) keep `go 1.25.0`: they ship no image, and their consumers' directives set the toolchain.

### Why the lockfile was edited by entry

`cargo update -p h2 --precise 0.4.16` on cargo 1.97.1 also rewrote six unrelated Windows-only `windows-sys` edges. A
no-op `cargo update -p h2 --precise 0.4.15` does the same, so the committed lockfile is not a fixed point for this
cargo's resolver. Only the two package entries were changed, using the versions and registry checksums cargo
resolved. `cargo metadata --locked` and `cargo fetch --locked` accept the result and verify both checksums. The
vendored `vendor/adk-*` crates are path dependencies and are untouched.

## Audits: before and after

### govulncheck v1.1.4 (source mode, `./...`)

The local toolchain is go1.26.5, so each module was scanned with `GOTOOLCHAIN` set to the toolchain that matters.

| Module | Before | After |
| --- | --- | --- |
| elitea-main | 7 reachable stdlib findings (go1.26.5); the same 7 at go1.25.12 | 0 at go1.25.13 and at go1.25.14 |
| elitea-scheduler | 7 reachable stdlib findings (go1.26.5) | 0 at go1.25.13 |
| elitea-subapp-host | 6 reachable stdlib findings (go1.26.5) | 0 at go1.25.13 |
| elitea-llm-gateway (`GOWORK=off`) | 6 reachable stdlib findings at go1.26.5 (`origin/main` checkout) | 0 at go1.26.6. go1.26.5 now refuses the module: `go.mod requires go >= 1.26.6` |

- The 7 IDs are GO-2026-5026 (net/http), GO-2026-5972 (encoding/asn1), GO-2026-6088 (encoding/xml), GO-2026-6089
  (net/http), GO-2026-6090 (crypto/tls), GO-2026-6091 (html/template) and GO-2026-6218 (net/url).
  - subapp-host and the gateway do not reach GO-2026-6091.
  - The OSV ranges fix all 7 in 1.25.13 and 1.26.6.
- Pre-existing and unchanged:
  - These findings are present in required modules but not called (module level):
    - golang.org/x/crypto v0.55.0: GO-2026-6354 and GO-2026-6355 (fixed in v0.56.0), and GO-2026-5932 (no fix);
    - github.com/google/cel-go v0.29.0: GO-2026-6094 (main only);
    - github.com/klauspost/compress v1.18.5: GO-2026-5841.
  - Binary mode on the stripped `elitea-main` binary lists the same 5, both before and after this change. It finds no
    stdlib finding: the binary was built with go1.25.14.
  - **No new findings.**

### cargo-deny 0.20.2 (`cargo deny --all-features check advisories`, Worker)

| Advisory | Before | After |
| --- | --- | --- |
| RUSTSEC-2026-0258, `h2` 0.4.15 (via hyper: bollard/adk-sandbox, reqwest, tonic, kube) | error | gone (`h2` 0.4.16) |
| yanked `chacha20` 0.10.1 (via async-nats → rand) | warning | gone (0.10.2) |
| RUSTSEC-2023-0071, `rsa` 0.9.10 (via sqlx-mysql; no fixed release) | error | **still reported**: pre-existing, mitigated in code (see Security), deliberately not ignored |

## Tests

| Suite | Result |
| --- | --- |
| `go test -race ./tests/buildcontext/` (elitea-main) | 2 passed: replace gate, floor gate |
| `go test ./...`: elitea-main | 13,037 passed, 0 failed, 1,987 skipped |
| `go test ./...`: elitea-scheduler | 140 passed, 0 failed, 15 skipped |
| `go test ./...`: elitea-subapp-host | 257 passed, 0 failed, 11 skipped |
| `go test ./...`: elitea-llm-gateway (`GOWORK=off`) | 1,628 passed, 0 failed, 9 skipped |
| `go vet ./...` on all four modules | clean |
| `bash scripts/ci/check-gateway-toolchain.sh` | passes (`the standard-library security floor is go 1.26.6`); with the gateway's go.mod mutated to `go 1.26.5` it fails: `go.mod needs go 1.26.5, below the standard-library security floor go 1.26.6` |
| `cargo fmt --all -- --check` | clean |
| `cargo clippy --locked --all-targets --all-features -- -D warnings` | clean |
| `cargo test --locked --all-features toolkits::sql_tests` | 14 passed, 0 failed |
| `cargo test --locked` (Worker) | lib 1,833 passed, 0 failed, 2 ignored; 9 integration binaries: 93 passed, 0 failed |

- **Go skips.** The skips are pre-existing environment skips, mostly PostgreSQL integration tests without a test
  database URL. This change touches no database code.
- **Worker ignores.** The 2 ignored Worker tests are `#[ignore]` tests that need local Docker or
  `ELITEA_TEST_DATABASE_URL`. They are not related to this change.

**TDD evidence.**
- `TestEveryShippedGoBuildIsAtOrAboveTheStdlibSecurityFloor` was written first and failed for all five entries
  (`go.work declares go 1.25.8, below the standard-library security floor 1.25.13`, and so on).
- After the review rewrite onto `go/version`, the gate was mutated again:
  - main's go.mod set to `go 1.25.12` failed;
  - the gateway pinned to `golang:1.26.5-trixie` failed (`pins golang 1.26.5, below the security floor 1.26.6`);
  - a `FROM --platform=$BUILDPLATFORM golang:1.25-trixie` builder was accepted.
- For the MySQL guard, `MYSQL_TLS_MODE` was mutated to `MySqlSslMode::Preferred`.
  `mysql_without_tls_is_refused_before_any_authentication_byte` then failed with `client sent 123 bytes`, which is
  the HandshakeResponse. It was restored to `VerifyIdentity`.
- The literal `tls == "verify-identity"` assertion in `catalog_and_nested_configuration_preserve_source_contract`
  (`sql_tests.rs:179`) still matches `MySqlSslMode::VerifyIdentity` directly, not the constant. Weakening the
  constant therefore fails two independent tests.

## Performance

- **Budget:** no runtime cost; image build time unchanged ±10%.
- **Go side.** The `go` directive changes no code path. The overlay `elitea-main` binary reports `go1.25.14`, the
  same toolchain as the image it replaced, because the series tag had already moved. The floor test reads 9 small
  files and runs in about 0.01 s.
- **Worker.** `h2` 0.4.16 adds a bound on queued empty DATA frames. It does nothing on well-formed traffic.
  `chacha20` 0.10.2 is a yank replacement with the same API. The SQL constant compiles to the same value as before.
  The new tests are loopback-only and finish in milliseconds; the SQL suite takes 0.03 s.
- **Result:** within budget. No hot path changed.

## Durability

Not applicable: no durable state, schema, checkpoint, claim or spool format changes.
- The browser run below shows each execution claimed, settled, publishing terminal output and retiring its command
  on the new images.
- No crash window is created or touched.

## Resilience

- **Fail-closed builds.** A builder below the floor refuses the module:
  - `golang:1.25.12-trixie`: `go: go.mod requires go >= 1.25.13 (running go 1.25.12; GOTOOLCHAIN=local)`;
  - `golang:1.25-trixie` (go1.25.14): accepted.

  Both were measured by running `go list -m` on `services/elitea-subapp-host` in each image.
- **h2.** RUSTSEC-2026-0258 was unbounded memory growth, or a length-overflow panic, from empty DATA frames that a
  peer sends to any hyper/h2 client or server in the Worker. 0.4.16 bounds it.
- **MySQL.**
  - A non-TLS server is refused before any authentication byte. The error is typed `InvalidConfiguration`; it is not
    an unknown outcome, so nothing was dispatched and nothing is retried.
  - Every fake-server step in the tests is bounded by a 5 s timeout. No sleeps.

## Security

**Go standard library.**
- Threats: seven reachable advisories in TLS, HTTP, URL, ASN.1, XML and html/template handling.
- Mechanism: the `go` directive floors (`go.work:1`, each `go.mod:3`). Their gate is
  `services/elitea-main/tests/buildcontext/toolchain_floor_test.go:82`, plus section 4 of `scripts/ci/check-gateway-toolchain.sh` for gateway-only changes. The gate fails when any shipped module,
  `go.work`, an exact builder pin or a `toolchain` line falls below `stdlibFloorGo125`/`stdlibFloorGo126`, or when a
  builder leaves the floor's series.
- Proof: the test above, the fail-closed container run, and govulncheck after (0 reachable).

**`h2` / `chacha20`.** Supply-chain hygiene. Both are lockfile-only updates, with checksums verified by
`cargo fetch --locked`. No new dependency was added.

**`rsa` (RUSTSEC-2023-0071, Marvin), no upstream fix.**
- Exposure assessment, from reading `sqlx-mysql` 0.8.6 `src/connection/auth.rs`:
  - The Worker reaches `rsa` only through `encrypt_rsa`. That function encrypts the password with the server's
    public key (OAEP), for `sha256_password` and for `caching_sha2_password` full authentication.
  - It runs only when `stream.is_tls` is false.
  - Marvin is a timing side channel in RSA *private-key decryption*. The Worker never holds an RSA private key, so
    the advisory's attack has nothing to time.
- The code-level mitigation keeps even the encryption path unreachable:
  - `MYSQL_TLS_MODE = MySqlSslMode::VerifyIdentity` (`client.rs:34`).
  - sqlx's `maybe_upgrade` (`src/connection/tls.rs`) returns `Error::Tls` before writing anything when the server
    does not advertise `CLIENT_SSL`, and refuses outright if TLS support is not compiled in.
  - So `is_tls` is always true at authentication time.
- Proof, with a fake MySQL server on 127.0.0.1 that sends a real HandshakeV10 without `CLIENT_SSL`:
  - **Control:** `mysql_non_tls_control_reaches_the_rsa_public_key_request` (`sql_tests.rs:794`). Raw sqlx with
    `Disabled` sends a HandshakeResponse. After `AuthMoreData [0x01, 0x04]` it sends the public-key request `[0x02]`.
    So the fake handshake is real, and the RSA path exists without TLS.
  - **Mitigation:** `mysql_without_tls_is_refused_before_any_authentication_byte` (`sql_tests.rs:832`). Both
    production tools (`list_tables_and_columns`, `execute_sql`) send **zero** bytes after the handshake: no
    HandshakeResponse, no password scramble, no key request. The call fails with `InvalidConfiguration`, and neither
    `Display` nor `Debug` contains the password.
- PostgreSQL is unaffected: `PgSslMode::VerifyFull`, and `rsa` is not in its dependency path.

**Secrets.**
- No secrets, prompts or tool arguments are in logs, tests or this file. The fake-server test asserts the password
  is absent from errors.
- Container swaps cloned the existing config through the Docker API without printing environment values.

## Recovery guarantees

This change adds no runtime phase and alters no checkpoint, claim, delivery or settlement path. The touched
(component × phase) rows:

| Component × phase | Class | Enforcing code | Proof |
| --- | --- | --- | --- |
| Worker × tool call (SQL toolkit, MySQL), server without TLS | **F**: typed pre-dispatch failure `InvalidConfiguration` | `client.rs:34,244` (`MYSQL_TLS_MODE`) | `sql_tests.rs:832`: zero bytes sent, typed error, password absent |
| Worker × model call, tool call, output delivery over hyper/h2 (reqwest, tonic, kube, bollard) | Unchanged | `h2` 0.4.16 (`Cargo.lock:1420`); no Worker code changed | Full Worker suite; browser runs settle and retire (below) |
| Main, scheduler, subapp-host, gateway × build | Not a runtime phase | `go` directives; `toolchain_floor_test.go:82`; `check-gateway-toolchain.sh` section 4 | Floor gate and the fail-closed container run |

**Why F for the MySQL row.**
- R, I and C do not apply: nothing was dispatched, so there is no effect to resume, retry or reconcile.
- The cause is the server's configuration (no TLS), and a retry cannot change it.
- The error is typed and readable, and it carries no secret.

## Security categories (`rules/security.md`)

| Category | Applies? | How it was checked |
| --- | --- | --- |
| Trust boundaries and identity | No | No identity, header or edge code changed |
| Authorization (object level) | No | No route, RPC or repository changed |
| Input, parsing and amplification | Indirectly | `h2` 0.4.16 bounds queued empty DATA frames inside the library; no project parser changed |
| Injection and construction | No | No SQL, template, shell or URL construction changed. SQL execution and parameters are unchanged. `check-gateway-toolchain.sh` reads only a repo-committed file, with a digits-and-dots regex. |
| Egress and SSRF | Yes (TLS) | MySQL keeps `VerifyIdentity` (certificate and hostname verified). The test proves a non-TLS server is refused before authentication. PostgreSQL keeps `VerifyFull`. |
| Secrets | Yes | Secret scan of the branch diff: 0 hits on added lines. The test asserts the password is absent from the error's `Display` and `Debug`. No environment values were printed during the container swaps. |
| Supply chain and deployment | Yes | govulncheck, `cargo deny` and `Cargo.lock` checksum verification (before and after above); no new dependency |

**Open item on supply chain.** The rule asks for "base images pinned to exact versions or digests; no floating tags
for shipped images."
- The Go builder stages keep their series tags (`golang:1.25-trixie`, `golang:1.26-trixie`). This is the
  repository's documented choice (#433, #506) so the stdlib takes patch releases. The `go` directive floor now
  guarantees the patch level.
- The builder is not the shipped image. The shipped runtime bases (`gcr.io/distroless/static-debian13:nonroot`,
  `cc-debian13:nonroot`) are also floating, and that predates this change.
- Pinning digests needs a bump process (for example Dependabot on Containerfiles), so it is left for a user
  decision.

## Browser evidence

**Stack and images.**
- Local NATS candidate stack (`http://localhost:18094`): real backend, no response mocks, persistent chats, signed in
  as `admin@centry.user` through the local OIDC emulator.
- Only Main and Worker were replaced. Each was built from the exact revisions running there plus #1140 plus this
  branch, so neither #1084 nor #1140 was dropped:
  - `elitea-main:current-nats-efa7213e803d-hotfix1140-deps20261008` (`sha256:852c5c48…`) = `efa7213e` + `024e6881`
    (#1140) + this branch's Go commit. Its binary is `go1.25.14`.
  - `elitea-worker-rust:frozen-cargo-c53ab7d5496a-hotfix1140-deps20261008` (`sha256:1c381997…`) = `c53ab7d5` +
    `f1334945` (#1140) + this branch's two Worker commits.
    - At this revision the SQL test hunk conflicted: `every_tool_keeps_the_sdk_contract` exists only on `main`. The
      new tests were kept and that main-only test was dropped.
    - The three `mysql_` tests pass at the overlay revision.
- The previous containers were kept as `…-rollback-predeps`. After the run they were restored, and the new ones were
  kept stopped as `…-deps20261008`.

**Fixtures.** The fixtures are the ones created for #1140, through the UI and the database as documented in
`pipeline-size-bounds-http-snapshot-20261008.md`. Every run and reload in this check went through the browser.

| Case | Pipeline | Chat / execution | Result |
| --- | --- | --- | --- |
| Small saved child (regression) | 136 "Gate5 typed parent 20260930" | 857 / `efa194bd…` | `4\|2\|3\|for orders\|2\|True`, identical to the pre-swap answer. After reload: one answer, attributed to the pipeline. |
| 103,598-byte YAML pipeline | 155 "PR1140 over 64 KiB" | 862 / `095cafb2…` | `third-ok`, identical to the pre-swap answer. After reload: one answer. |
| Normal LLM chat | — (new chat, model `vllm/CONTINUATION-REPAIR-FIXTURE`) | 864 / `1754bd75…` | **Not completed: environment.** The answer was "The model service could not be reached"; see below. |

**Chat 864.**
- The candidate stack has no `elitea-llm-gateway` container on its network. Main's `llmproxy` logs
  `lookup elitea-llm-gateway … server misbehaving`.
- This is pre-existing topology; earlier evidence on this stack used LLM-free pipelines. It does not depend on the
  images.
- The Worker path up to the model call behaved correctly: admission, assembly, the typed
  `model_gateway.unavailable` (retryable) failure, settlement and command retirement.
- All three executions logged claim, settlement, terminal publication and command retirement.

**Observation.** In chat 857 the live answer was labelled "Elitea" until reload, then the pipeline name. That is a
Web live-label detail, unrelated to these images.

## Open limits and follow-ups

1. **Normal LLM chat in the browser is not yet proven on these images.** It needs an LLM gateway (or the mock) wired
   into the candidate stack, or another stack. This is an open gate item.
2. **Go 1.25 support window.** Go 1.27.0 is released (current 1.27.1 / 1.26.8). Under Go's two-release policy, 1.25
   receives no further security patches once that window closes. 1.25.14 is the last 1.25 release listed. Moving the
   workspace to the 1.26 series needs the CI `go-version: "1.25"` pins changed, which is a user decision.
3. **Third-party Go modules** (x/crypto, cel-go, klauspost/compress): present but not called. They are a separate
   follow-up; GO-2026-5932 has no fix.
4. **RUSTSEC-2023-0071** remains in `cargo deny` output until `sqlx-mysql` drops `rsa` or `rsa` ships a constant-time
   fix.
5. **Audits in CI.** Neither govulncheck nor `cargo deny` runs in CI, and CI was not changed (user decision). The Go
   floor is enforced by the `go test` gate above (ci-go) and by `check-gateway-toolchain.sh` (ci-gateway). The Rust advisory state is enforced only by this document and the
   image scans.
