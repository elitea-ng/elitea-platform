# Per-node settings: LLM & direct toolkit/MCP tools, FU-343, and FU-067 (5b-2, 5b-3)

Date: 2026-11-23.
Scope: Point 5 Wave 2 (`T-5B-node-settings.md` §4 `5b-2` + `FU-343` prerequisite and `5b-3` + `FU-067`).
Status: `partial` (all Rust, Go, Faults, and Web unit/component gates pass; live PostgreSQL `ELITEA_TEST_DATABASE_URL` crash suite and disposable Docker/browser stack rehearsal are deferred while local Docker builds/stacks remain paused for host EKG builds).

## 1. Business behavior and deliberate non-ports

Per-node `settings:` is new replatform resilience behavior with no current-platform reference implementation; visual controls reuse the existing `NodeSettings` panel from 5b-1.

- **Contract & parity admission:** `libs/proto/contracts/pipeline-node-settings-v1.json` admits all 8 `settings:` keys (`on_error`, `error_transition`, `always_output_data`, `execute_once`, `retry_on_fail`, `max_tries`, `wait_between_tries_ms`, `timeout_ms`) on `llm`, `toolkit`, and `mcp`. Worker, Main, and Web run the shared vector suite (`libs/proto/contracts/pipeline-node-settings-v1-vectors.json`) with identical verdicts and refusal text.
- **FU-343 typed model-gateway propagation:** `adk-graph 2.3.0` collapsed every `GraphError` into `AdkError::agent(...)` (`agent.legacy` / `ErrorCategory::Internal`). `EliteaGraphAgent::run` now restores typed `model_gateway.*` and `anthropic_gateway.*` errors from a static allowlist without echoing untrusted provider text, and preserves those codes across `LlmNode`, `ApplicationNode`, `scoped_runtime`, and `parallel::graph_error_code`. `model_gateway.too_many_events` maps to `RuntimeFailureKind::ModelResponseInvalid`.
- **Bounded `Retry-After` surfacing:** Shared `validate_response_head` in `openai_compatible_facade.rs` (used by both OpenAI-compatible and Anthropic facades) parses `retry-after-ms` and `Retry-After` (delta seconds and RFC 2822 HTTP-date) on `429` and `503` responses with a 64-byte header limit, duplicate-header rejection, and saturation at `MAX_WAIT_BETWEEN_TRIES_MS + 1` (`300_001` ms sentinel).
- **5b-2 LLM node settings:** `LlmNode` wires `AR/graph/llm_failure::classify_adk_error`, tracks pre- vs post-output via `event_has_visible_delta` (empty metadata/usage chunks do not flip the boundary; any non-empty text, thinking, tool call, media, server tool call/response, or embedded resource flips it), retries only before the first visible delta honoring `max(wait_between_tries_ms, Retry-After)` up to `300_000` ms (`300_001` ms stops typed `ElapsedLimit` with 0 wait and is never swallowed by `on_error: continue`), clamps per-node `timeout_ms` and retry waits to the run execution deadline, emits `send_model_failure` only on terminal failure (`FU-030`), and supports `on_error` (`stop`/`continue`/`error_output`), `always_output_data`, and `execute_once`. Tool-free LLM nodes run behind `RecoverableNode` (`SCHEMA_V5` journal codec); tool-bearing LLM nodes use the in-memory ledger so HITL confirmations (`LLM_TOOL_RESUME_STATE_KEY`) never poison a step journal and `MayHaveStarted` never retries.
- **5b-3 Direct toolkit/MCP settings & FU-067:** `DirectToolNode` admits retry only when platform-classified read-only (`DirectToolNodeKind::Toolkit && tool.is_read_only()`; remote MCP `readOnlyHint` is never trusted). Effectful toolkit nodes and all MCP nodes run behind the fenced `RecoverableNode` journal and escalate timeout/unknown outcomes to `Reconcile` (`pipeline_node_recovery` card, `0` repeats, even with `retry_on_fail` or `on_error: continue`). Sensitive-tool denial/block and delegated-auth Skip perform `0` retries. `compiler.rs` binds `node_recovery_spec` and `DirectToolCommittedProjector` (`node_result_projector`) for direct tool nodes, verifying `DirectToolOwnerReceipt` (`CommittedResult` and `VerifiedNoEffect`) fail-closed under domain-separated audience `elitea.runtime.direct-tool-receipt.v1`.
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
| Worker `Retry-After` | `services/elitea-worker-rust/src/transport/openai_compatible_facade.rs:1600,1636`, `openai_compatible_facade_tests.rs` | `parse_retry_after_ms`, `validate_response_head` (shared by OpenAI & Anthropic facades) |
| Worker 5b-2 LLM | `services/elitea-worker-rust/src/agents/graph/llm.rs:1427,1671,2205,2229`, `node_events.rs`, `node_settings_definition.rs`, `node_settings_runtime.rs:51-75`, `node_recovery_runtime.rs`, `session.rs:3333` | `execute_with_settings`, `classify_llm_attempt_failure`, `event_has_visible_delta`, `run_model_agent_attempt`, terminal-only `send_model_failure`, execution deadline propagation |
| Worker 5b-3 & FU-067 | `services/elitea-worker-rust/src/agents/graph/direct_tool.rs:474,481,776,1510,1543`, `compiler.rs:345,1429,1463`, `node_recovery_owner.rs:8,78,89,119`, `code_runtime.rs` | `is_platform_read_only`, `requires_effect_journal`, `dispatch_read_only_with_settings`, `DirectToolCommittedProjector`, `DIRECT_TOOL_RECEIPT_AUDIENCE`, thin `PipelineExecutionContext` |
| Worker test suite | `services/elitea-worker-rust/src/agents/graph/node_settings_llm_direct_tool_tests.rs`, `node_settings_tests.rs`, `node_recovery_runtime_tests.rs` | 12 dedicated unit/component tests covering 5b-2, 5b-3, FU-067, and digest stability |
| Upgrade & recovery docs | `docs/upgrading/2026-11-23-343-node-settings-llm-direct-tools.md`, `services/elitea-worker-rust/docs/recovery-guarantees.md` | Worker-before-Main rollout order, `SCHEMA_V5` rollback note, and P02/P03/P04/P05/gateway rows |

## 3. Observed verification results

All checks ran offline/locked on the final worktree state:

- **Agent Runtime (`libs/rust`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked -p elitea-agent-runtime --all-targets -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked -p elitea-agent-runtime --lib -- graph::`: **188 passed, 0 failed, 1 ignored** (the 1 ignored test is the pre-existing manual timing-budget benchmark `typed_update_budget_at_the_bounds`)
- **Worker (`services/elitea-worker-rust`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked --all-targets -- -D warnings` (default feature set): PASS (`0` warnings)
  - `cargo clippy --offline --locked --features sandbox-supervisor --all-targets -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked -p elitea-worker-rust --lib -- agents::graph::agent::fu343_tests agents::graph::node_settings agents::graph::node_recovery agents::graph::direct_tool agents::graph::llm agents::pipeline::scoped_runtime::tests transport::openai_compatible_facade_tests::retry_after`: **151 passed, 0 failed, 3 ignored** (all 3 ignored are pre-existing DB-gated tests)
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

## 4. Engineering and security self-audit (`rules/security.md`)

- **Performance:** `PipelineExecutionContext` packs `execution_id` and `execution_deadline` behind one thin `Option<Arc<_>>` pointer (`compiler.rs:345`), preserving `PipelineNodeRuntimes` and `NativePipelineApplicationParticipant` enum size with zero extra allocations on paths without settings. `parse_retry_after_ms` runs in O(1) bounded by `MAX_RETRY_AFTER_HEADER_BYTES` (64 bytes).
- **Durability & Resilience:** Pre-first-visible-delta retry prevents duplicate streamed text (`event_has_visible_delta`); post-output failures terminate as non-retryable `ModelOutputIncomplete`. Tool-free LLM retries persist through `SCHEMA_V5` (`FailureWithRetryAfter`, backwards-compatible with v1–v4). Effectful toolkit and all MCP direct nodes journal `Started` before dispatch and escalate unknown/timeout outcomes to `Reconcile` (`0` blind repeats). Digest stability is proven: absent, empty (`{}`), and default (`{on_error: stop}`) `settings:` keep identical pipeline digests.
- **Security 12-category check (neutral summary):**
  1. *Authentication & authorization:* `DirectToolCommittedProjector` validates execution id, generation, dispatch activation, node digest, toolkit alias, tool name, argument digest, visit activation/attempt/revision/thread/step, receipt SHA-256, and domain-separated grant audience `DIRECT_TOOL_RECEIPT_AUDIENCE` (`direct_tool.rs:1543-1615`). Sensitive-tool denials and delegated-auth Skips never retry.
  2. *Input validation & bounds:* `Retry-After` headers enforce single-value presence, 64-byte cap, and `300_000` ms wait cap (`300_001` ms sentinel stops typed with 0 wait). Shared contract vectors enforce limit and limit+1 bounds (`max_tries` 2..=5, `wait_between_tries_ms` 100..=300_000, `timeout_ms` 1_000..=900_000).
  3. *Untrusted metadata / effect safety:* Remote MCP `readOnlyHint` is never trusted for automatic retry (`DirectToolNode::is_platform_read_only` requires `DirectToolNodeKind::Toolkit && tool.is_read_only()`).
  4. *Error & log redaction:* `model_gateway_error_spec` (`agent.rs:34`) maps only allowlisted static codes and fixed catalog messages; untrusted upstream error text is never echoed (`elitea_graph_agent_preserves_every_typed_model_gateway_class_without_echoing_text`). `node_errors` entries use registered fault codes only.
  5. *Injection / SSRF / deserialization / crypto / secrets / supply chain / concurrency / DoS:* No SQL, shell, HTML, or outbound URL construction added; strict `#[serde(deny_unknown_fields)]` on `DirectToolOwnerReceipt`; SHA-256 digests via `ring`; no new dependencies; no `unsafe` code; retry waits check cancellation every 100 ms and respect run execution deadlines.

## 5. Recovery-guarantee rows and follow-ups

- Updated `services/elitea-worker-rust/docs/recovery-guarantees.md` for `Worker x P02, P03` (LLM with `settings:`), `Worker x P04, P05` (Direct `toolkit`/`mcp` with `settings:` + FU-067), and `gateway x P02` (G-GW-01 for pipeline LLM nodes).
- Follow-ups:
  - Run PostgreSQL 18 crash-recovery tests and disposable Docker/browser acceptance once consolidated stack testing resumes.
  - Land `5b-4` (`agent` / nested pipeline settings) and `5b-5` (fan-out member settings) in separate PRs.
