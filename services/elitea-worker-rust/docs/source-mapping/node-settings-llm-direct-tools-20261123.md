# Per-node settings: LLM & direct toolkit/MCP tools, FU-343, and FU-067 (5b-2, 5b-3)

Date: 2026-11-23.
Scope: Point 5 Wave 2 (`T-5B-node-settings.md` §4 `5b-2` + `FU-343` prerequisite and `5b-3` + `FU-067`).
Status: `partial` (all Rust, Go, Faults, and Web unit/component gates pass; live PostgreSQL `ELITEA_TEST_DATABASE_URL` crash suite and disposable Docker/browser stack rehearsal are deferred while local Docker builds/stacks remain paused for host EKG builds).

## 1. Business behavior and deliberate non-ports

Per-node `settings:` is new replatform resilience behavior with no current-platform reference implementation; visual controls reuse the existing `NodeSettings` panel from 5b-1.

- **Contract & parity admission:** `libs/proto/contracts/pipeline-node-settings-v1.json` admits all 8 `settings:` keys (`on_error`, `error_transition`, `always_output_data`, `execute_once`, `retry_on_fail`, `max_tries`, `wait_between_tries_ms`, `timeout_ms`) on `llm`, `toolkit`, and `mcp`. Worker, Main, and Web run the shared vector suite (`libs/proto/contracts/pipeline-node-settings-v1-vectors.json`) with identical verdicts and refusal text.
- **FU-343 typed model-gateway propagation:** `adk-graph 2.3.0` collapsed every `GraphError` into `AdkError::agent(...)` (`agent.legacy` / `ErrorCategory::Internal`). `EliteaGraphAgent::run` now restores typed `model_gateway.*` and `anthropic_gateway.*` errors from a static allowlist without echoing untrusted provider text, and preserves those codes across `LlmNode` (live stops AND journal restores), `ApplicationNode`, `scoped_runtime`, and `parallel::graph_error_code`. `model_gateway.too_many_events` maps to `RuntimeFailureKind::ModelResponseInvalid`. Local non-model errors (`InvalidInput`, `InvalidResult`) exit cleanly without emitting spurious `send_model_failure`.
- **Bounded `Retry-After` surfacing:** Shared `validate_response_head` in `openai_compatible_facade.rs` (used by both OpenAI-compatible and Anthropic facades) parses `retry-after-ms` and `Retry-After` (ASCII digits only, rejecting signs, and RFC 2822 HTTP-date) on `429` and `503` responses with a 64-byte header limit, duplicate-header rejection, and saturation at `MAX_WAIT_BETWEEN_TRIES_MS + 1` (`300_001` ms sentinel).
- **5b-2 LLM node settings:** `LlmNode` wires `AR/graph/llm_failure::classify_adk_error`, tracks pre- vs post-output via `event_has_visible_delta` (empty metadata/usage chunks do not flip the boundary; any non-empty text, thinking, tool call, media, server tool call/response, or embedded resource flips it), retries only before the first visible delta honoring `max(wait_between_tries_ms, Retry-After)` up to `300_000` ms (`300_001` ms stops typed `ElapsedLimit` with 0 wait and is never swallowed by `on_error: continue`), clamps per-node `timeout_ms` and retry waits to the run execution deadline (including inside saved child pipelines and pipeline tools), emits `send_model_failure` only on terminal failure (`FU-030`), and supports `on_error` (`stop`/`continue`/`error_output`), `always_output_data`, and `execute_once`. Tool-free LLM nodes run behind `RecoverableNode` (`SCHEMA_V5` journal codec); tool-bearing LLM nodes use the in-memory ledger so HITL confirmations (`LLM_TOOL_RESUME_STATE_KEY`) never poison a step journal and `MayHaveStarted` never retries.
- **5b-3 Direct toolkit/MCP settings & FU-067:** `DirectToolNode` admits retry only when platform-classified read-only (`DirectToolNodeKind::Toolkit && tool.is_read_only()`; remote MCP `readOnlyHint` is never trusted). Pre-dispatch execution deadline expiry stops with `0` calls (`NoExternalEffect`), whereas in-flight execution deadline expiry, timeout, or retryable failure on effectful toolkit and all MCP nodes records `UnknownExternalEffect` behind the fenced `RecoverableNode` journal and escalates to `Reconcile` (`pipeline_node_recovery` card, `0` repeats, even with `retry_on_fail` or `on_error: continue`). Settled `NoExternalEffect` failures match identically between live execution and journal restore; read-only settlement preserves `GraphError` (`pipeline.node_errors.exhausted`, `pipeline.node_settings.invalid`, `pipeline.node_recovery.cancelled`). Sensitive-tool denial/block and delegated-auth Skip perform `0` retries. `compiler.rs` binds `node_recovery_spec` and `DirectToolCommittedProjector` (`node_result_projector`) for direct tool nodes, enforcing strict domain separation between `DIRECT_TOOL_RECEIPT_AUDIENCE` (`elitea.runtime.direct-tool-receipt.v1`) and `CODE_RESULT_AUDIENCE` (`code_committed.rs:141`) on both `CommittedResult` and `VerifiedNoEffect`.
- **Deliberate non-ports / deferred scope:**
  - `5b-4` (`agent` / nested pipeline node settings), `5b-5` (fan-out member settings), `5d` (HTTP action), and `5e` (DB action) remain separate slices (`agent` stays `family_not_yet`).
  - Ordinary chat agents without pipeline node `settings:` retain default fail-fast behavior (G-GW-01 closed for pipeline `llm` nodes with `settings:` only).
  - Live PostgreSQL (`ELITEA_TEST_DATABASE_URL`) crash tests and live Docker/browser stack rehearsal are deferred until local Docker builds resume.

## 2. Changed paths (`path:line`)

| Area | File | Key symbols and line anchors |
| --- | --- | --- |
| Contract & vectors | `libs/proto/contracts/pipeline-node-settings-v1.json` | Admits `llm`, `mcp`, `toolkit` with all 8 keys |
| Contract vectors | `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` | Admitted LLM/toolkit/MCP vectors + boundary checks (`agent` kept for `family_not_yet`) |
| Fault registry | `libs/go/faults/registry/worker.json`, `libs/go/faults/codes_gen.go`, `docs/errors/README.md` | Registers 8 fault codes (`pipeline.llm.{failed,rate_limited,timeout,unavailable}`, `pipeline.mcp.{failed,timeout}`, `pipeline.toolkit.{failed,timeout}`) |
| Agent Runtime | `libs/rust/agent-runtime/src/graph/node_settings.rs`, `node_settings_tests.rs` | `SettingsFamily::{Llm, Toolkit, Mcp}` + contract parity |
| Agent Runtime journal | `libs/rust/agent-runtime/src/graph/node_recovery.rs:858`, `node_recovery_codec.rs:24,88,211` | `record_failure_with_retry_after` (`300_000` ok, `300_001` -> `Stop(ElapsedLimit)`), `SCHEMA_V5` (`FailureWithRetryAfter`) |
| Main limits | `services/elitea-main/internal/domain/pipelinelimits/node_settings.go` | Admits `llm`, `mcp`, `toolkit` in `settingsFamilies` |
| Web helpers & tests | `apps/elitea-web/src/features/pipelines/lib/nodeSettings.helpers.ts`, `ui/settings/NodeSettings.test.tsx` | Admits `llm`, `mcp`, `toolkit`; tests verify Retry/Timeout controls on all 3 families and hidden panel on `agent` |
| Worker FU-343 | `services/elitea-worker-rust/src/agents/graph/agent.rs:34,210,232`, `parallel.rs:2683`, `application.rs`, `pipeline/scoped_runtime.rs:694,780,975`, `protocol/output.rs` | Static allowlist restoration of typed `model_gateway.*`/`anthropic_gateway.*` classes across root, Agent, nested pipeline, and fan-out paths |
| Worker `Retry-After` | `services/elitea-worker-rust/src/transport/openai_compatible_facade.rs:1600,1638`, `openai_compatible_facade_tests.rs` | `parse_retry_after_ms` (strict ASCII digits + overflow saturation), `validate_response_head` (shared by OpenAI & Anthropic facades) |
| Worker 5b-2 LLM | `services/elitea-worker-rust/src/agents/graph/llm.rs:1427,1671,1918,2231,2255`, `node_events.rs`, `node_settings_definition.rs`, `node_settings_runtime.rs:51-75`, `node_recovery_runtime.rs:271,1207,1349`, `session.rs:3333`, `pipeline.rs:189,3213`, `application_tools.rs:3101`, `ordinary.rs:989` | `execute_with_settings`, `classify_llm_attempt_failure`, `restored_model_gateway_spec`, `event_has_visible_delta`, `run_model_agent_attempt`, terminal-only `send_model_failure`, execution deadline propagation into root and saved child pipelines |
| Worker 5b-3 & FU-067 | `services/elitea-worker-rust/src/agents/graph/direct_tool.rs:474,481,669,796,1385,1543,1576`, `compiler.rs:345,1434,1468`, `node_recovery_owner.rs:8,78,89,119`, `code_committed.rs:131,141`, `code_runtime.rs` | `is_platform_read_only`, `requires_effect_journal`, `DeadlineExceededBeforeDispatch` vs `DeadlineExceededInFlight`, live/restore parity, verbatim `GraphError` preservation, `DirectToolCommittedProjector`, `DIRECT_TOOL_RECEIPT_AUDIENCE` vs `CODE_RESULT_AUDIENCE`, thin `PipelineExecutionContext` |
| Worker test suite | `services/elitea-worker-rust/src/agents/graph/node_settings_llm_direct_tool_tests.rs`, `node_settings_tests.rs`, `node_recovery_runtime_tests.rs` | 20 dedicated unit/component tests covering 5b-2, 5b-3, FU-067, audience separation, live/restore parity, deadline enforcement, and digest stability |
| Upgrade & recovery docs | `docs/upgrading/2026-11-23-343-node-settings-llm-direct-tools.md`, `services/elitea-worker-rust/docs/recovery-guarantees.md` | Worker-before-Main rollout order, `SCHEMA_V5` rollback note, and P02/P03/P04/P05/gateway rows |

## 3. Observed verification results

All checks ran offline/locked on the final worktree state:

- **Agent Runtime (`libs/rust`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked -p elitea-agent-runtime --all-targets -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked -p elitea-agent-runtime --lib -- graph::`: **188 passed, 0 failed, 1 ignored** (pre-existing manual timing-budget benchmark `typed_update_budget_at_the_bounds`)
- **Worker (`services/elitea-worker-rust`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked --all-targets -- -D warnings` (default feature set): PASS (`0` warnings)
  - `cargo clippy --offline --locked --features sandbox-supervisor --all-targets -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked -p elitea-worker-rust --lib -- agents::graph::agent::fu343_tests agents::graph::node_settings agents::graph::node_recovery agents::graph::direct_tool agents::graph::llm agents::pipeline::scoped_runtime::tests transport::openai_compatible_facade_tests::retry_after`: **159 passed, 0 failed, 3 ignored** (pre-existing DB-gated tests)
- **Go Faults & Main:**
  - `GOMAXPROCS=4 go test -p 2 ./libs/go/faults/...`: PASS (`TestEveryRegisteredCodeIsUsed` green)
  - `services/elitea-main`: `GOMAXPROCS=4 go vet ./internal/domain/pipelinelimits/...` PASS; `GOMAXPROCS=4 go test -p 2 -race ./internal/domain/pipelinelimits/...` PASS
- **Web (`apps/elitea-web`):**
  - `npm run typecheck`: PASS (`0` errors)
  - `npm run lint -- --deny-warnings`: PASS (`0` warnings, `0` errors across 6,631 files)
  - `vitest run --maxWorkers=2 src/features/pipelines/lib/nodeSettings.helpers.test.ts src/features/pipelines/lib/flow-editor/constants/runtimeContract.nodeSettings.test.ts src/features/pipelines/ui/settings/NodeSettings.test.tsx`: **3/3 files, 94/94 tests passed**
- **CI scripts & hygiene:**
  - `node scripts/ci/check-upgrading-notes.mjs`: PASS
  - `node scripts/ci/check-worker-feature-sets.mjs`: PASS (`(default), sandbox-supervisor`)
  - Secret scan on full branch diff: `0` hits; AI/Claude commit trailers: `0`
- **Deferred (honestly noted):**
  - `cargo deny --offline --all-features check advisories` blocked in sandbox by read-only `~/.cargo/advisory-dbs/db.lock` (no `Cargo.toml` or `Cargo.lock` dependency changed in this PR; runs in CI).
  - Live PostgreSQL (`ELITEA_TEST_DATABASE_URL`) crash tests and live Docker/browser stack rehearsal deferred while local Docker builds/stacks remain paused for EKG.

## 4. Engineering and security self-audit (`replatform-delivery-gate.md §3a` / `rules/security.md`)

- **Performance:** `PipelineExecutionContext` packs `execution_id` and `execution_deadline` behind one thin `Option<Arc<_>>` pointer (`compiler.rs:345`), preserving `PipelineNodeRuntimes` and `NativePipelineApplicationParticipant` enum size with zero extra allocations on paths without settings. `DirectNodeFailureCause::Graph(Box<GraphError>)` keeps `DirectNodeFailure` compact. `parse_retry_after_ms` runs in O(1) bounded by `MAX_RETRY_AFTER_HEADER_BYTES` (64 bytes).
- **Durability & Resilience:** Pre-first-visible-delta retry prevents duplicate streamed text (`event_has_visible_delta`); post-output failures terminate as non-retryable `ModelOutputIncomplete`. Tool-free LLM retries persist through `SCHEMA_V5` (`FailureWithRetryAfter`, backwards-compatible with v1–v4) and preserve typed `model_gateway.*` errors across journal restores. Effectful toolkit and all MCP direct nodes journal `Started` before dispatch and escalate timeout, in-flight deadline expiry, or retryable failure to `Reconcile` (`0` blind repeats). Digest stability is proven: absent, empty (`{}`), and default (`{on_error: stop}`) `settings:` keep identical pipeline digests.

### 12-Category Security Matrix (`rules/security.md`)

| # | Category | Status | Evidence / Rationale |
| --- | --- | --- | --- |
| 1 | Trust boundaries and identity | PASS | Identity and claim authority remain bound solely to `ClaimBoundRuntimeContextAuthority` and `NodeAttemptAuthority`; YAML `settings:` and provider headers never supply actor or project identity. |
| 2 | Authorization (object level) | PASS | `DirectToolCommittedProjector::validate_owner_receipt` (`direct_tool.rs:1576`) and `CodeCommittedProjector::validate_owner_receipt` (`code_committed.rs:131,141`) bind exact execution, generation, dispatch activation, node digest, arguments/source, visit, receipt SHA-256, and domain-separated grant audience (`DIRECT_TOOL_RECEIPT_AUDIENCE` vs `CODE_RESULT_AUDIENCE`). Sensitive-tool denials and delegated-auth Skips never retry. |
| 3 | Input, parsing and amplification | PASS | `parse_retry_after_ms` (`openai_compatible_facade.rs:1600`) rejects duplicate headers, enforces a 64-byte header cap, requires strict ASCII digits on `retry-after-ms`/`Retry-After` delta seconds, and saturates overflow to the `300_001` ms cap+1 sentinel. Shared contract vectors prove limit and limit+1 bounds (`max_tries` 2..=5, `wait_between_tries_ms` 100..=300_000, `timeout_ms` 1_000..=900_000, `node_errors` 64-entry cap). `DirectToolOwnerReceipt` uses `#[serde(deny_unknown_fields)]`. |
| 4 | Injection and construction | N/A | No SQL, shell, HTML/Markdown, or outbound URL construction is added or altered. |
| 5 | Egress and SSRF | N/A | No outbound dialer, proxy, redirect, or egress-guard path is added or modified (`libs/rust/egress-guard/` untouched). |
| 6 | Secrets | PASS | `0` secrets, tokens, or private URLs in diff (`git diff origin/main..HEAD` scan = `0` hits). `model_gateway_error_spec` (`agent.rs:34`) maps only static allowlisted codes and fixed catalog messages; `node_errors` entries carry only registered fault codes without echoing provider payloads or tool arguments. |
| 7 | Supply chain and deployment | PASS (audit deferred) | `0` dependency or lockfile changes (`Cargo.toml`, `Cargo.lock`, `go.mod`, `package-lock.json` untouched). `cargo deny` advisory lock is read-only in sandbox and runs in CI. |
| 8 | Cross-layer differentials and version skew | PASS | Shared contract `pipeline-node-settings-v1{,-vectors}.json` verified across Rust AR, Worker, Go Main, and Web Vitest; `SCHEMA_V5` journal codec stays backwards-compatible with v1–v4; rollout order documented in `docs/upgrading/2026-11-23-343-node-settings-llm-direct-tools.md`; `DIRECT_TOOL_RECEIPT_AUDIENCE` vs `CODE_RESULT_AUDIENCE` separation proven by negative tests. |
| 9 | Agentic delegation and untrusted content isolation | PASS | Remote MCP `readOnlyHint` is never trusted for automatic retry (`DirectToolNode::is_platform_read_only` requires `DirectToolNodeKind::Toolkit && tool.is_read_only()`). Parent run `execution_deadline` propagates monotonically into saved child pipelines and pipeline tools (`pipeline.rs:189,3213`, `application_tools.rs:3101`, `ordinary.rs:989`). Tool-bearing LLM nodes never poison step journals across HITL confirmations. |
| 10 | TOCTOU, concurrency races, and effect replay | PASS | Effectful toolkit and all MCP direct tools append fenced `Started` before dispatch and record `UnknownExternalEffect` -> `Reconcile` (`0` repeats) on timeout, in-flight deadline expiry, or crash; tool-bearing LLM `MayHaveStarted` never retries or continues; live vs journal-restore settled failures match identically. |
| 11 | Sandbox escape and resource isolation | PASS | No sandbox/runner changes; `CodeCommittedProjector::verify_no_effect_receipt` rejects cross-family `DIRECT_TOOL_RECEIPT_AUDIENCE` proofs (`code_committed.rs:141`). Retry waits check cancellation every 100 ms and never sleep past `execution_deadline`. |
| 12 | AuthN, OAuth, PAT, and cryptographic hygiene | PASS | Domain-separated SHA-256 digests via `ring` (`elitea.graph.direct_tool.arguments.v1`, `elitea.pipeline.node-attempt-dispatch.v1`); no `unsafe` code; delegated-auth Skip stops with `0` retries. |

## 5. Recovery-guarantee rows and follow-ups

- Updated `services/elitea-worker-rust/docs/recovery-guarantees.md` for `Worker x P02, P03` (LLM with `settings:`), `Worker x P04, P05` (Direct `toolkit`/`mcp` with `settings:` + FU-067), and `gateway x P02` (G-GW-01 for pipeline LLM nodes).
- Follow-ups:
  - Run PostgreSQL 18 crash-recovery tests and disposable Docker/browser acceptance once consolidated stack testing resumes.
  - Land `5b-4` (`agent` / nested pipeline settings) and `5b-5` (fan-out member settings) in separate PRs.
