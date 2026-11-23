# SQL toolkit guarded TCP dial, TLS hostname separation, and egress guard (5e-0 + 5e-1, 2026-11-23)

Branch `feat/5e0-sql-guard-db-foundation` (based on `origin/main` `54718237f`).
Scope: PRs **5e-0** (guarded TCP dial with separated TLS hostname verification and
removal of the ambient PostgreSQL TLS-file seam) and **5e-1** (Rust egress guard
wiring and `ELITEA_DB_EGRESS_ALLOWLIST` Worker/Helm/compose configuration for
`toolkit-sql`).

Excluded by design: Main `db_action_rules` schema/API (`5e-2`), `type: db`
pipeline node (`5e-3`), and fan-out child DB nodes (`5e-4`).

## Business behaviour

Kept from the current platform: the `sql` toolkit preserves its two-tool catalog
(`execute_sql` and `list_tables_and_columns`), PostgreSQL and MySQL dialect
selection, bounded schema introspection, and bounded row/output limits.

Deliberately not ported from `elitea_sdk/tools/sql/api_wrapper.py`: unbounded
`fetchall()`, unverified TLS defaults, ambient host environment overrides, and
unguarded private-network dialing.

Changed on purpose:

- **Separated TCP dial IP and TLS hostname (5e-0):** Patched vendored copies of
  `sqlx-postgres 0.9.0` and `sqlx-mysql 0.9.0` add `connect_addr(IpAddr)` so TCP
  dials the egress-checked IP literal directly (zero DNS lookup in the driver)
  while TLS verifies `&options.host` (`PgSslMode::VerifyFull`,
  `MySqlSslMode::VerifyIdentity`).
- **Ambient PG TLS-file seam removed (5e-0):** `PgConnectOptions::new_without_env()`
  replaces `PgConnectOptions::new_without_pgpass()`. PostgreSQL connections never
  read `PGSSLROOTCERT`, `PGSSLCERT`, `PGSSLKEY`, or other `PG*` environment
  variables.
- **Rustls handshake errors stay non-retryable:** `SqlClient` detects
  `rustls::Error` wrapped inside `sqlx::Error::Io` so certificate and hostname
  mismatches return `InvalidConfiguration` (`sql.configuration.invalid`,
  non-retryable) and stop immediately instead of retrying another IP.
- **Egress guard on `toolkit-sql` (5e-1):** Every SQL dial resolves and filters
  targets through `elitea-egress-guard` (`resolve_checked_with`, shared
  `libs/go/egresslib/testdata/classify_vectors.json`, 5 s DNS timeout, max 32
  addresses). Public endpoints remain allowed unless narrowed by the run's
  `PublicPolicy`. Private endpoints require an explicit CIDR, IP[:port], or
  `localhost[:port]` entry in `ELITEA_DB_EGRESS_ALLOWLIST` (Helm
  `worker.dbEgressAllowlist`). Link-local and cloud metadata addresses are always
  refused. Malformed `ELITEA_DB_EGRESS_ALLOWLIST` values fail worker startup.
- **Stale doc cleanup:** `families/sql/mod.rs` drops the obsolete
  "capability-disabled" doc comment and `#![allow(dead_code)]`.

## Changed paths and enforcing code

| Area | Path and lines |
| --- | --- |
| Vendored `sqlx-postgres 0.9.0` (`new_without_env`, `connect_addr`, `has_ssl_*`) | `libs/rust/vendor/sqlx-postgres/src/options/mod.rs:21,83,108-126,181-185,617-635`, `src/connection/stream.rs:45-54` |
| Vendored `sqlx-mysql 0.9.0` (`connect_addr`) | `libs/rust/vendor/sqlx-mysql/src/options/mod.rs:67,99,139-143,540-543`, `src/connection/establish.rs:18-27` |
| Workspace patches, vendor notes, dependabot coverage | `libs/rust/Cargo.toml:67-72`, `libs/rust/Cargo.lock`, `services/elitea-worker-rust/Cargo.toml:227-230`, `services/elitea-worker-rust/Cargo.lock`, `libs/rust/vendor/README.md`, `scripts/ci/check-dependabot-coverage.sh` |
| SQL egress-guarded dial, `EgressRefused`, `new_without_env`, TLS IO classification | `libs/rust/agent-runtime/src/toolkits/families/sql/client.rs:260-369,733-746`, `mod.rs:1-14`, `tools.rs:86-105` |
| Policy & egress re-exports | `libs/rust/agent-runtime/src/toolkits/policy.rs:111,356-371`, `src/toolkits/mod.rs:230-234`, `src/egress.rs:39-42` |
| SQL guarded-dial TLS, DNS rebinding, and negative egress tests | `libs/rust/agent-runtime/src/toolkits/sql_tests.rs`, `libs/rust/agent-runtime/Cargo.toml:159-160` |
| Worker `ELITEA_DB_EGRESS_ALLOWLIST` fail-closed startup & permit propagation | `services/elitea-worker-rust/src/config.rs:170-176,825-905`, `src/execution/production.rs:133,400-405,454`, `src/agents/sensitive_tools.rs:65`, `src/toolkits/direct_runtime.rs:114-156,323-355`, `src/bootstrap.rs:828` |
| Helm, compose, and upgrade note | `deploy/helm/elitea/values.yaml:7121-7128`, `deploy/helm/elitea/templates/worker/deployment.yaml:234-238`, `deploy/helm/tests/render-worker.sh`, `deploy/docker-compose.standalone-rust-agent.yml:67-70`, `docs/upgrading/2026-11-23-078-sql-toolkit-egress-guard.md` |

## Tests

| Suite | Result | Notes |
| --- | --- | --- |
| `cargo test --offline --locked -p elitea-egress-guard` | **17 passed, 0 failed, 0 skipped** | 9 unit + 8 integration (`classify_matches_shared_vectors`) |
| `cargo test --offline --locked -p elitea-agent-runtime --features toolkit-sql sql_tests` | **23 passed, 0 failed, 0 skipped** | Includes PG `VerifyFull` & MySQL `VerifyIdentity` real `rcgen`+`tokio-rustls` handshakes on checked IP `127.0.0.1` with SNI verification, TLS hostname mismatch refusal, DNS rebinding, private/IPv4-mapped/NAT64/6to4/metadata matrix, port pinning, bare hostname non-lifting, oversized DNS, run `PublicPolicy`, MySQL non-TLS refusal, and `new_without_env` check |
| `go -C libs/go/egresslib test -v ./addrcheck/...` | **8 passed, 0 failed, 0 skipped** | Shared `classify_vectors.json` parity confirmed on Go side |
| `services/elitea-worker-rust` targeted tests (`config::tests::`, `sensitive_tools::tests::`, `execution::production::tests::`) | **55 passed, 0 failed, 0 skipped** | 34 config (incl. 2 new fail-closed allowlist tests) + 14 sensitive_tools (incl. guardrails permit preservation) + 7 production |
| `cargo clippy --offline --locked` (`elitea-egress-guard`, `elitea-agent-runtime` with and without `toolkit-sql`, `elitea-worker-rust` default) | **PASS (0 warnings, `-D warnings`)** | `cargo fmt --all -- --check` PASS on both workspaces |
| `cargo deny --offline --all-features check advisories` (`libs/rust`, `services/elitea-worker-rust`) | **advisories ok** | Zero new advisories |
| CI scripts (`check-upgrading-notes.mjs`, `check-worker-feature-sets.mjs`, `check-rust-toolchain.mjs`, `check-vendor-manifest-notes.mjs`, `check-dependabot-coverage.sh`, `helm lint deploy/helm/elitea`) | **PASS** | All green on committed `HEAD` |
| Live PostgreSQL 18 / MySQL 8 TLS container spike & `deploy/helm/tests/render-worker.sh` | **Skipped (deferred)** | Docker builds/stacks paused on host for EKG headroom; Helm `charts/` subcharts not downloaded offline. Covered in-process by real `tokio-rustls` PG/MySQL wire TLS handshakes and `helm lint` |

## Performance

- DNS resolution is bounded to 5 s (`DNS_LOOKUP_TIMEOUT`) and at most 32
  addresses (`MAX_RESOLVED_ADDRESSES`); TCP + TLS connect stays inside one 10 s
  `CONNECT_TIMEOUT` budget (`client.rs:18,327-369`).
- `PermitSet` is parsed once at worker startup (`config.rs:888`,
  `production.rs:400`) and shared via `Arc` across runs and direct toolkit
  calls, adding zero per-call parsing allocation.
- `sqlx` dials the pre-checked `IpAddr` literal directly through Tokio's
  `IpAddr` fast path with zero second DNS lookup.

## Durability

- No durable database schema, journal, or wire protocol state changes in 5e-0 or
  5e-1.
- Refused egress stops before opening any TCP socket or sending any SQL bytes.
- Existing `sql.effect.unknown_outcome` non-retryable post-dispatch semantics
  remain unchanged (`client.rs:344-418`, `sql_tests.rs`).

## Resilience

- Fail-closed startup: malformed or non-UTF-8 `ELITEA_DB_EGRESS_ALLOWLIST`
  refuses worker startup before connecting NATS, gRPC, or AgentState
  (`config.rs:829-904`, `production.rs:133`).
- Unknown `db_egress_allowlist` key in `runtime.json` fails strict
  `deny_unknown_fields` deserialization (`config.rs`).
- Multi-address failover tries remaining checked IPs only on non-TLS `Io`
  errors; TLS verification failures abort immediately as non-retryable
  `InvalidConfiguration` (`client.rs:334-339,357-362,733-746`).
- All error codes (`sql.egress.refused`, `sql.configuration.invalid`) stay
  data-free and redact hosts, IPs, and credentials (`sql_tests.rs`).

## Security

| `rules/security.md` category | Applies | How checked |
| --- | --- | --- |
| Egress and SSRF | Yes | Shared Go/Rust `classify_vectors.json` (Go 8/8, Rust 17/17); negative `SqlClient` matrix across loopback, RFC 1918, CGNAT, ULA, IPv4-mapped, NAT64, 6to4, link-local, metadata, multicast, unspecified; metadata stays refused even with `0.0.0.0/0,::/0`; port pinning; bare hostname non-lifting; DNS rebinding (single lookup + pinned `connect_addr`); run `PublicPolicy` honored |
| AuthN, OAuth, PAT, and cryptographic hygiene | Yes | Real `rcgen`+`tokio-rustls` tests prove `PgSslMode::VerifyFull` and `MySqlSslMode::VerifyIdentity` verify `&options.host` (SNI + cert SAN) while dialing `connect_addr`, refuse mismatched hostnames before auth bytes, and refuse non-TLS MySQL before auth bytes (keeping RUSTSEC-2023-0071 unreachable). Ambient `PGSSL*` env seam removed via `PgConnectOptions::new_without_env` |
| Secrets | Yes | Zero secrets in diff (secret scan: 0 hits); `SqlClientError` and `RuntimeConfigError` never retain passwords, hosts, or allowlist text |
| Input, parsing, and amplification | Yes | Oversized DNS answers (> 32 addresses) refused; `Allowlist::parse_csv` fails closed on any malformed entry; strict `deny_unknown_fields` on `RuntimeDeployConfig` |
| Supply chain and deployment | Yes | Minimal vendored patches on already-locked `sqlx-postgres 0.9.0` and `sqlx-mysql 0.9.0` (exact SHA-256 and commit recorded in `libs/rust/vendor/README.md`, `check-dependabot-coverage.sh`, `check-vendor-manifest-notes.mjs`); `cargo deny` advisories ok on both workspaces |
| Trust boundaries, authorization, injection, cross-layer, delegation, TOCTOU, sandbox | No / unchanged | No Main routes, authz, proto, pipeline compiler, or sandbox code changed; guardrails policy rebuild preserves deployment `db_egress_permits` (`sensitive_tools.rs:65`) |

## Recovery guarantees

| Component x phase | Class | Enforced by | Proven by |
| --- | --- | --- | --- |
| Worker x SQL toolkit dial (egress or TLS refusal) | F | `client.rs:307-369`, `client.rs:733-746` (refused before dispatch; zero SQL sent) | `sql_tests.rs` (23/23 pass) |
| Worker x startup (`ELITEA_DB_EGRESS_ALLOWLIST` malformed) | F | `config.rs:829-904`, `production.rs:133` (fail-closed before intake) | `config::tests::db_egress_allowlist_*` (2/2 pass) |

No L rows introduced.

## Real-browser evidence

Not applicable to 5e-0 / 5e-1 (no Web UI or pipeline node changes; Docker stacks
paused on host for EKG). Live container PG 18 / MySQL 8 TLS and browser
acceptance land with 5e-3 (`type: db` node admitted).

## Fixtures

All TLS certificates in `sql_tests.rs` are ephemeral in-memory `rcgen` test CAs
and leaf certificates scoped to `#[cfg(test)]`. No database rows or external
credentials are used.

## Follow-ups

- **5e-2:** Main `db_action_rules` schema, admin API, and credential freeze.
- **5e-3:** `type: db` pipeline node, typed binds, write-transaction marker
  reconciliation, live PG 18 / MySQL 8 TLS container conformance, and browser
  acceptance.
- **5e-4:** DB node inside fan-out children.
