# Node settings for Code, retry on journal, effective timeout, FU-309, FU-347, FU-348, FU-351 (5b-1 PR B)

Branch `feat/5b1-node-settings-code`, cut from `origin/main` `5f21740a8` (which includes `#2247` FU-335 `ce7c4c20d` and `#2240` `93fa99fd1`).
Builds directly on 5b-1 PR A ([`node-settings-core-20261111.md`](node-settings-core-20261111.md)) under approved plan `handoffs/point5-wave1-20261008/5b1/PLAN.md` and review decisions D1–D15 / binding conditions C1–C7 (`5b1/REVIEW-PLAN.md`).

## 1. Business behavior and deliberate non-ports

### Admitted behavior on Code nodes

| Setting | Contract range / default | Runtime behavior on Code |
|---|---|---|
| `retry_on_fail` | boolean, default `false` | Compiles into `NodeRecoveryPolicy::admit` on the existing attempt journal (`node_settings.rs:484`); ordinal dispatch identity from attempt 1 (`compiler.rs:1382-1388`, `code_sandbox_intent.go:681`) |
| `max_tries` | integer `2..=5`, default `3` (requires `retry_on_fail: true`) | Total tries on the journal (`max_attempts = max_tries`), plus `MAX_RUNTIME_LOST_RETRIES` for platform-proven `RuntimeLost` on non-platform-client Code |
| `wait_between_tries_ms` | integer `100..=300000`, default `1000` (requires `retry_on_fail: true`) | Fixed backoff (`initial_ms = maximum_ms = wait`, `multiplier = 1`), clamped against the run deadline by `record_failure_within` (`node_recovery.rs:840`) |
| `timeout_ms` | integer `1000..=900000` | Tightens effective sandbox timeout to `min(profile.timeout_seconds, ceil(timeout_ms / 1000)).max(1)` (`code_remote.rs:1072`) across `PreparedJob`, deadline admission, and observation deadline |
| `on_error` | `stop` (default) \| `continue` \| `error_output` | On a committed, effect-settled Code failure (`Failed(Stop(r))` for `r in {NotRetryable, RetryDisabled, AttemptsExhausted}` excluding control stops, denials, and `DeadlineInsufficient`), records one `node_errors` entry (`pipeline.code.failed` or `pipeline.code.timeout`) and continues or routes to `error_transition` identically on live and restore paths (`node_recovery_runtime.rs:960-1004`) |
| `error_transition` | node id or `END` (requires `on_error: error_output`) | Target taken on settled failure; Web Code node wires the error output handle exclusively to `settings.error_transition` (C6) |
| `always_output_data` | boolean, default `false` | Writes declared-type neutral values (`""`, `[]`, `{}`, `0`, `false`, `null`) on settled failure or explicit null output on success (risk 4 opt-in) |
| `execute_once` | boolean, default `false` ("Run once per execution") | On revisit in the same execution turn (including after process replacement), skips dispatch without opening a journal (`node_recovery_runtime.rs`) and takes the normal transition |

### Folded-in items in this PR

- **FU-309 (`NodeFailureClass::DeadlineInsufficient`, D7):** Dedicated failure class (`node_recovery.rs:54`) persisted in journal schema v4 (`node_recovery_codec.rs:21-38`) and Main index 15 (`code_failure_route.go`). Never retried, never error-routed (`NodeErrorRoute::admit`, `MatchCodeFailureRoute`), and never continued by `on_error`. Both live and restored terminals map to `RuntimeFailureKind::CodeDeadlineInsufficient` (`runtime.code_deadline_insufficient`, `libs/go/faults/registry/runtime.json:95`).
- **FU-347 (performance guard):** 128-node no-settings compile guard and settings wrapper micro-benchmark (`benches/node_settings.rs`, `node_settings_bench.rs`) proving `p99 <= 50 us` on no-settings and deterministic wrapped paths.
- **FU-348 (saved-child recovery, D15):** Real-PostgreSQL verification of a deterministic settled failure inside a saved child (preserving C4 private `node_errors`) and a Code retry inside a saved child resuming cleanly at `claim+1`.
- **FU-351 (State Modifier-only transitional edge, D12):** Emits legacy-parity `agent_on_transitional_edge` (`{next_step, state}` with `langgraph_node`) per visit exclusively for a `state_modifier` node that declares a `transition` (`events.rs:1590`, `state_modifier_trace.rs`). Bounded in byte size, redacts secret keys and secret value patterns, omits internal keys (`__*`), and preserves `node_errors` without setting `saw_pipeline_node_events` (`events.rs:1502-1507`) so data-only pipelines retain their synthetic completion answer.

### Deliberate non-ports

- **Execute once (D8):** Skips the body on revisit without copying or replaying a stored payload (unlike n8n's "first input item only").
- **Always output data (risk 4):** Opt-in only; never emits synthetic success when `always_output_data` is `false`.
- **Mutual exclusion with `recovery:` / `failure_handler:` (D1):** A Code node with `settings:` cannot also declare `recovery:` or `failure_handler:`. Enforced across Worker (`node_settings_definition.rs`), Main (`pipelinelimits/node_settings.go`, `runtime_saved_code_policy.go:366`), and Web (`nodeSettings.helpers.ts`).
- **Non-State-Modifier transitional edges (FU-352):** Router, Aggregate, and Split Out do not emit `agent_on_transitional_edge` in this PR.
- **Other node families:** LLM, Agent, Toolkit, MCP, Decision, Parallel, Map, and Pause families remain refused for `settings:` (later slices).
- **Durable failure details (`#2240` / `93fa99fd1`):** Preserved intact across settings retries, `on_error`, and per-node timeouts; zero changes to `agentstate` migration `0016_sandbox_run_failure_detail.sql`.

---

## 2. Changed paths and exact `path:line` anchors

| Layer | Path & anchor | Role |
|---|---|---|
| Contracts | `libs/proto/contracts/pipeline-node-settings-v1.json` | Admits `"code"` with all 8 keys |
| Contracts | `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` | 52 shared admission vectors (up from 45) verified identically by Rust, Go, and Web |
| Faults | `libs/go/faults/registry/worker.json` (+ generated `libs/go/faults/codes_gen.go`, `docs/errors/README.md`) | Registers `pipeline.code.failed` and `pipeline.code.timeout` (alongside existing `runtime.code_deadline_insufficient` at `libs/go/faults/registry/runtime.json:95`) |
| Agent Runtime | `libs/rust/agent-runtime/src/graph/node_recovery.rs:54,59-78,95,193,344-445,840-891` | `DeadlineInsufficient`, `retry_condition`, `deadline()`, `NodeErrorRoute::admit`, `plan_after_failure`, `record_failure_within` |
| Agent Runtime | `libs/rust/agent-runtime/src/graph/node_recovery_codec.rs:21,31,38` | `SCHEMA_V4` for `DeadlineInsufficient` and `FailureWithin` journal events |
| Agent Runtime | `libs/rust/agent-runtime/src/graph/node_settings.rs:484-524` | `SettingsFamily::Code`, `to_recovery_policy` (C2) |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_settings_definition.rs` | Admits Code settings; rejects combination with `recovery:`/`failure_handler:` |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_settings_runtime.rs` | Shared `node_errors` entry builder, neutral outputs, run-once bookkeeping |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_recovery_runtime.rs:960,985` | `can_settle_stop` and `settle_code_failure` applied identically on live and restore paths; `execute_once` skip before opening a journal |
| Worker | `services/elitea-worker-rust/src/agents/graph/compiler.rs:1382-1392,2211` | Compiles Code `settings:` into `NodeRecoveryPolicy`, threads `timeout_ms` to `CodeNode`, sets `legacy_first_attempt = recovery.is_none() && !settings.retry_on_fail` |
| Worker | `services/elitea-worker-rust/src/agents/graph/code_remote.rs:346,388,836,1072` | `effective_timeout_seconds`, `admit_fresh`, `recheck_after_preparation`, `retry_deadline_budget` |
| Worker | `services/elitea-worker-rust/src/agents/graph/code_attempt_remote.rs:127,410,711-732,941-953` | `DeadlineInsufficient` recording, `sandbox.deadline_exceeded` -> `AttemptTimeout`, C1 `effect_possible` preservation for platform-client Code |
| Worker | `services/elitea-worker-rust/src/agents/graph/code_runtime.rs:63,158` | Maps `NodeFailureClass::DeadlineInsufficient` -> `ToolCodeError::DeadlineInsufficient` |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_recovery_receipt.rs:80` | Restored terminal receipt mapping for `DeadlineInsufficient` |
| Worker | `services/elitea-worker-rust/src/agents/graph/state_modifier_trace.rs`, `src/agents/events.rs:1502-1507,1590` | FU-351 bounded secret-redacted `agent_on_transitional_edge` for State Modifier with `transition` |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_settings_code_pg_tests.rs`, `node_recovery_code_fanout_pg_tests.rs` | 6 real-PostgreSQL crash/C1/execute-once/FU-348 integration tests |
| Worker | `services/elitea-worker-rust/src/agents/graph/node_settings_bench.rs`, `benches/node_settings.rs` | FU-347 128-node compile + settings wrapper benchmark (`node-settings-bench` feature) |
| Main | `services/elitea-main/internal/domain/pipelinelimits/node_settings.go` | Admits `"code"` with all 8 keys (52/52 shared vectors) |
| Main | `services/elitea-main/internal/infra/storage/code_failure_route.go:27` | Index 15 `deadline_insufficient`, explicitly rejected in `MatchCodeFailureRoute` |
| Main | `services/elitea-main/internal/infra/storage/runtime_saved_code_policy.go:328,361,483` | Strips `"settings"` in `MatchOriginalSavedCodeConfiguration`; admits and strictly validates `"settings"` in `ReadSavedCodeDeclaration` (`parseSavedCodeSettings`, exact JSON integer decoding, key allowlist, bounds, couplings, mutual exclusion with `recovery`/`failure_handler`) |
| Main | `services/elitea-main/internal/infra/db/repos/code_sandbox_intent.go:681` | `matchOriginalCodeDispatch` admits ordinal attempts `1..=maxTries+MaxRuntimeLostRetries` (non-platform-client) or `1..=maxTries` (platform-client) under `d.SettingsRetryOnFail`; refuses attempt 0, legacy try 1, and `maxTries+1` |
| Main | `services/elitea-main/internal/infra/db/repos/code_registration_real_path_postgres_integration_test.go` | `TestOriginalCodeSettingsRetryDispatchIdentityPostgres` real-PG integration test |
| Web | `apps/elitea-web/src/features/pipelines/lib/nodeSettings.helpers.ts` | Admits Code keys; drops `max_tries`/`wait_between_tries_ms` when `retry_on_fail` toggles off |
| Web | `apps/elitea-web/src/features/pipelines/ui/settings/NodeSettings.tsx`, `ui/nodes/CodeNode.tsx`, `src/shared/i18n/en.json` | Renders Retry / Max tries / Wait / Timeout rows only on admitted families (`code`) + wires Code error handle + 7 i18n keys |
| Docs | `docs/upgrading/2026-11-20-0-node-settings-code.md`, `services/elitea-worker-rust/docs/recovery-guarantees.md`, `services/elitea-worker-rust/docs/source-mapping/README.md` | Rollout order (C7), R/I/C/F/L guarantee row, source-mapping index |

No new third-party dependencies were added in Rust, Go, or Web.

---

## 3. Binding conditions verification (C1, C2, C7) and timeout spike

### Timeout spike result (Commit 5)

Read-only verification before writing code confirmed that compiled-snapshot bindings do **not** pin `timeout_seconds`:
- Worker: `services/elitea-worker-rust/src/sandbox/compiled_snapshot.rs:41-61,246-260,295-311`
- Main: `services/elitea-main/internal/infra/sandbox/compiled_snapshot.go:21-41,156-177,235-250`

Therefore the fallback (refusing `timeout_ms` on Code) was **not** triggered. `effective_timeout_seconds` (`code_remote.rs:1072`) clamps `min(profile.timeout_seconds, ceil(timeout_ms / 1000)).max(1)` consistently across `PreparedJob`, `admit_fresh`, `recheck_after_preparation`, `retry_deadline_budget`, and `observation_deadline`.

### Binding condition C1 — Node `timeout_ms` vs run deadline and unknown external effects

| Failure origin | `NodeFailureClass` | `ReplaySafety` | `plan_after_failure` / journal decision | Settings `on_error` (`continue` / `error_output`) | Code anchor |
|---|---|---|---|---|---|
| Pure isolated Code exceeds `timeout_ms` | `AttemptTimeout` | `NoExternalEffect` | `RetryAt` (if `retry_on_fail` and tries/budget remain) else `Stop(AttemptsExhausted\|RetryDisabled\|ElapsedLimit)` | Allowed on `Stop(AttemptsExhausted\|RetryDisabled)` (`pipeline.code.timeout`); refused on `Stop(ElapsedLimit)` | `code_attempt_remote.rs:711-732,953`; `node_recovery.rs:394-444`; `node_recovery_runtime.rs:960-985` |
| Platform-client / egress-capable Code exceeds `timeout_ms` | `AttemptTimeout` | `UnknownExternalEffect { effect_id }` | **`Reconcile`** (never retried blindly; 0 re-dispatches) | **Refused** (`can_settle_stop` requires `Stop`, never `Reconcile`) | `code_attempt_remote.rs:711-732,953`; `node_recovery.rs:365-373`; `node_recovery_runtime.rs:960-985` |
| Run deadline too short before dispatch (FU-309) | `DeadlineInsufficient` | `NoExternalEffect` | `Stop(NotRetryable)` (never retried or error-routed) | **Refused** (`failure.class.deadline()`) | `code_attempt_remote.rs:127,410`; `node_recovery.rs:54,76,95,193`; `node_recovery_runtime.rs:960-981` |
| Retry backoff would exceed run deadline (`record_failure_within`) | Original class | Original replay | Persisted `Stop(ElapsedLimit)` (`SCHEMA_V4` `FailureWithin`) | **Refused** (`StopReason::ElapsedLimit` is excluded from `can_settle_stop`, C1) | `node_recovery.rs:840-891`; `node_recovery_codec.rs:21-38`; `node_recovery_runtime.rs:960-981` |
| Run cancelled or lease lost | `Cancelled` / `LeaseLost` | `Unclassified` | `Stop(ControlDecision)` | **Refused** (`ControlDecision` is never continued) | `node_recovery.rs:89-91,358-360`; `node_recovery_runtime.rs:960-975` |

Proven by unit test `c1_sandbox_deadline_exceeded_is_attempt_timeout_and_platform_client_keeps_reconcile` (`code_attempt_remote.rs:1262`) and real-PostgreSQL test `real_pg_c1_platform_client_timeout_with_retry_on_fail_reconciles_zero_redispatch` (`node_settings_code_pg_tests.rs`).

**C1 reconciliation (`PLAN.md:198` vs `REVIEW-PLAN.md:44`):** `REVIEW-PLAN.md:44` permits a node's own `timeout_ms` expiry to settle under `on_error` (`continue` / `error_output`) once effects are known absent. For `platform_client: true` Code nodes, however, a sandbox deadline expiry may follow a committed platform call whose completion cannot be disproved; per `PLAN.md:198` and Delivery Gate §2 Class C (unknown external effect), platform-client timeouts intentionally keep `UnknownExternalEffect` and escalate to `Reconcile` even when `on_error: continue` is configured, while pure isolated Code timeouts (`NoExternalEffect`) continue or route via `error_output` as specified.

### Binding condition C2 — Complete `RetryCondition` table (proving `settings:` never widens `recovery:`)

`NodeSettings::to_recovery_policy` (`libs/rust/agent-runtime/src/graph/node_settings.rs:484-507`) populates `retry_on` with the exact 4-element `RetryCondition` enum (`node_recovery.rs:101-106`) admitted by `NodeRecoveryPolicy::admit`. Every `NodeFailureClass` variant (`node_recovery.rs:32-55`) maps through the single `NodeFailureClass::retry_condition` function (`node_recovery.rs:59-78`) and passes through `plan_after_failure` (`node_recovery.rs:344-445`):

| `NodeFailureClass` | `retry_condition()` (`node_recovery.rs:59-78`) | Admitted by explicit `recovery:`? | Retried by `settings.retry_on_fail`? | Guard / reason |
|---|---|---|---|---|
| `DependencyUnavailable` | `Some(DependencyUnavailable)` | Yes (when listed) | Yes (repeatable only) | Requires `NoExternalEffect` or `IdempotentEffectNotCommitted` (`node_recovery.rs:364-393`) |
| `RateLimited` | `Some(RateLimited)` | Yes (when listed) | Yes (repeatable only) | Same `ReplaySafety` gate |
| `AttemptTimeout` | `Some(AttemptTimeout)` | Yes (when listed) | Yes (repeatable only) | Platform-client/egress timeout carries `UnknownExternalEffect` -> `Reconcile` (`code_attempt_remote.rs:711-732`) |
| `WorkerInterrupted` | `Some(WorkerInterrupted)` | Yes (when listed) | Yes (repeatable only) | Same `ReplaySafety` gate |
| `InvalidConfiguration` | `None` | Never | Never | Deterministic user/config error |
| `InvalidInput` | `None` | Never | Never | Deterministic input error |
| `InvalidResult` | `None` | Never | Never | User code returned invalid output/non-zero exit |
| `ModelOutputIncomplete` | `None` | Never | Never | Non-Code / non-retryable here |
| `AuthenticationDenied` | `None` (`denial()`) | Never | Never | AuthN denial never retried (`node_recovery.rs:81-86,361-363`) |
| `AuthorizationDenied` | `None` (`denial()`) | Never | Never | AuthZ denial never retried |
| `SensitiveRejected` | `None` (`denial()`) | Never | Never | Policy denial never retried |
| `Cancelled` | `None` (`control_stop()`) | Never | Never | `Stop(ControlDecision)` (`node_recovery.rs:89-91,358-360`) |
| `LeaseLost` | `None` (`control_stop()`) | Never | Never | `Stop(ControlDecision)` |
| `Unknown` | `None` | Never | Never | Unclassified failure never retried |
| `RuntimeLost` | `None` | Platform allowance only (`plan_runtime_lost`) | Platform allowance only (`plan_runtime_lost`, non-platform-client) | YAML cannot name it (`node_recovery.rs:47-50,466-495`) |
| `DeadlineInsufficient` | `None` (`deadline()`) | Never | Never | FU-309 / D7 (`node_recovery.rs:54,76,95,193`) |

Because `NodeSettings::to_recovery_policy` constructs a standard `NodeRecoveryPolicy` and leaves `on_error`, `error_transition`, `always_output_data`, `execute_once`, and `timeout_ms` outside the policy, `settings:` cannot retry any class or replay safety that an explicit `recovery:` policy cannot retry, and `journal_activation_id` remains byte-identical for `recovery:` pipelines.

### Binding condition C7 — Rollout order and version-skew proof

- Upgrade note [`docs/upgrading/2026-11-20-0-node-settings-code.md`](../../../docs/upgrading/2026-11-20-0-node-settings-code.md) leads with **"Main ships before the Worker"**.
- **Old Main + new Worker fails closed:** Before this PR, Main's `MatchOriginalSavedCodeConfiguration` stripped only `recovery` and `failure_handler` (`runtime_saved_code_policy.go:334-335`) and `ReadSavedCodeDeclaration` rejected any key outside its 14-key allowlist (`runtime_saved_code_policy.go:366`). A saved Code node carrying `settings:` therefore fails closed on an older Main (`ErrContentUnauthorized` / `ErrContentRejected`) rather than silently stripping or widening dispatch identity.
- **New Main + old Worker:** Old Worker rejects `settings:` on Code at compile time (`pipeline.node_settings.invalid`), and rejects `SCHEMA_V4` journal payloads as `corrupt_journal` (`node_recovery_codec.rs:21-38`). No silent misinterpretation occurs in either direction.

---

## 4. Performance (FU-347)

Measured via `cargo bench --manifest-path services/elitea-worker-rust/Cargo.toml --offline --locked --features node-settings-bench --bench node_settings` on Apple Silicon (release profile, 128-node graph and deterministic `SettingsNode` paths):

| Benchmark | Measured `p50` | Measured `p99` | Budget | Result |
|---|---:|---:|---:|---|
| `compile 128-node no-settings` | 2,467.29 us | 3,067.21 us | <= 50,000 us | PASS (16.3x headroom) |
| `state_modifier unwrapped (no-settings)` | 2.04 us | 2.58 us | <= 50 us | PASS |
| `state_modifier wrapped success` | 2.04 us | 2.67 us | <= 50 us | PASS (+0.08 us delta vs unwrapped) |
| `state_modifier wrapped continue (settled failure)` | 4.21 us | 5.33 us | <= 50 us | PASS |
| `state_modifier wrapped execute_once skip` | 2.25 us | 2.92 us | <= 50 us | PASS |

---

## 5. Durability and resilience

### Recovery-guarantee matrix (`R/I/C/F/L`)

Added to [`services/elitea-worker-rust/docs/recovery-guarantees.md`](../recovery-guarantees.md) directly below the PR A row:

- **R (Reclaim / process replacement):** Killing the Worker after `Started` before receipt reconciles the started attempt (0 extra dispatches); killing mid-wait (`RetryAt`) resumes the remaining wait on the same try with stale-claim zombie fencing; killing between journal failure append and step checkpoint produces a byte-identical `node_errors` entry on restore; saved-child Code retries resume cleanly at `claim+1` (FU-348).
- **I (Idempotent / run-once):** `execute_once` across a 3-visit Router loop with mid-loop process replacement executes exactly 1 sandbox job.
- **C (Check / reconcile):** C1 platform-client Code timeout persists `UnknownExternalEffect` -> `Reconcile` with 0 re-dispatches even when `retry_on_fail: true` and `on_error: continue`.
- **F (Fail closed):** `DeadlineInsufficient` (FU-309) is never retried, error-routed, or continued on live or restore; Main's `matchOriginalCodeDispatch` refuses `attempt == 0`, legacy try 1 under `settings.retry_on_fail`, try `max_tries + 1`, and foreign/forged dispatch identities; saved-child deterministic failures keep `node_errors` private to the child (C4).
- **L (Limit):** `node_errors` remains capped at 64 entries / 1 KiB per entry; `agent_on_transitional_edge` state payload is byte-bounded and secret-redacted (`state_modifier_trace.rs`).

### Real-PostgreSQL integration suite (PostgreSQL 17, disposable container)

| Test | Suite | Outcome |
|---|---|---|
| `postgres_node_settings_survive_process_replacement` | Worker (`node_settings_pg_tests`) | PASS (1/1, 2.42s) |
| `kill_after_started_before_receipt_reconciles_without_extra_try` | Worker (`node_settings_code_pg_tests`) | PASS |
| `kill_mid_wait_retry_at_resumes_remainder_and_fences_zombie` | Worker (`node_settings_code_pg_tests`) | PASS |
| `crash_between_journal_failure_and_checkpoint_matches_live_and_refuses_deadline_insufficient` | Worker (`node_settings_code_pg_tests`) | PASS |
| `c1_platform_client_timeout_reconciles_with_zero_redispatches` | Worker (`node_settings_code_pg_tests`) | PASS |
| `execute_once_code_across_router_loop_and_replacement_dispatches_once` | Worker (`node_settings_code_pg_tests`) | PASS |
| `fu348_saved_child_deterministic_continue_and_code_retry_resume_at_claim_plus_one` | Worker (`node_settings_code_pg_tests`) | PASS (6/6 total in 9.47s) |
| `real_pg_sec_5b1b_01_retry_then_succeed_keeps_failure_detail_empty` | Worker (`node_settings_code_pg_tests`) | PASS |
| `real_pg_sec_5b1b_01_continued_failure_keeps_failure_detail_empty` | Worker (`node_settings_code_pg_tests`) | PASS |
| `real_pg_sec_5b1b_01_error_output_failure_keeps_failure_detail_empty` | Worker (`node_settings_code_pg_tests`) | PASS |
| `real_pg_sec_5b1b_01_saved_child_continued_code_failure_keeps_parent_detail_empty` | Worker (`node_settings_code_pg_tests`) | PASS (10/10 total in 8.80s) |
| `a_failed_programs_output_is_kept_for_the_terminal_and_durably_for_a_restart` | Worker (`node_recovery_code_wiring_tests`, `#2240`) | PASS (1/1, 0.35s) |
| `TestOriginalCodeSettingsRetryDispatchIdentityPostgres` | Main (`repos`, `-race`) | PASS (1/1, 0.90s) |

---

## 6. Security self-audit (`security.md`)

| Category | Verification & code evidence |
|---|---|
| Trust boundaries and identity | Main derives `SavedCodeDeclaration` strictly from stored, digest-verified configuration (`ReadSavedCodeDeclaration`, `runtime_saved_code_policy.go`), never from Worker request claims or YAML strings. Dispatch ordinal identity binds `(activation, attempt, dispatch)` inside Main's DB transaction (`code_sandbox_intent.go:681`). **`SEC-5B1B-05`:** Main retains `SettingsTimeoutSeconds` (`ceil(timeout_ms / 1000)`) on `SavedCodeDeclaration` and enforces `job.TimeoutSeconds <= declaration.SettingsTimeoutSeconds` inside `matchCodePreparedSemantics` (`runtime_code_intent.go`, proven by `TestMatchCodePreparedEnforcesSettingsTimeoutCeiling`). |
| Authorization (object level) | `matchOriginalCodeDispatch` authorizes every retry attempt against the stored declaration inside the dispatch transaction and fails closed on foreign activation, forged dispatch key, attempt 0, legacy try 1, or `attempt > max_tries` (`TestOriginalCodeSettingsRetryDispatchIdentityPostgres`). **`SEC-5B1B-03`:** Dedicated `failure_handler` nodes are isolated from `settings.error_transition` across Worker (`node_settings_definition.rs`, `node_recovery_definition.rs`), Main save/runtime (`pipelinelimits/node_settings.go`, `code_failure_route.go` `codeNodeSuccessRoutesContain`), and Web (`nodeSettings.helpers.ts`), proven by negative tests in all three layers. |
| Input, parsing and amplification | `parseSavedCodeSettings` (`runtime_saved_code_policy.go`) uses `json.Decoder.UseNumber()` for exact integer validation, enforces a strict 8-key allowlist, rejects unknown fields and self-transitions, bounds `max_tries` (`2..=5`), `wait_between_tries_ms` (`100..=300000`), and `timeout_ms` (`1000..=900000`), and enforces all key couplings identically across Rust, Go, and Web (52/52 shared vectors). **`SEC-5B1B-02`:** `state_modifier_trace.rs` bounds traversed JSON nodes (`MAX_SANITIZE_NODES = 2_048`) to short-circuit oversized arrays/maps immediately and computes `_truncated` `node_errors` byte budgets in a single O(N) pass (`short_circuits_oversized_numeric_array_and_bounds_64_node_errors`). **`SEC-5B1B-04`:** `decodeSavedCodePolicy` validates YAML `!!int` tags before JSON marshaling and normalizes `settings.error_transition` identically to `pipelinelimits` (`TestSavedCodeSettingsYAMLFloatLabelNormalizationAndSelfTransition`), while Web guards `NodeSettingsFamilies` with `Object.hasOwn` (`nodeSettingsFamilyKeys`). |
| Injection and construction | Zero raw SQL string construction, shell construction, or unsandboxed template evaluation added. Web renders Code settings through React controlled inputs and i18n keys only. |
| Egress and SSRF | No changes to egress guard or preparation network policy; platform-client/egress Code timeouts are forced to `effect_possible = true` -> `Reconcile` (`code_attempt_remote.rs:711-732`). |
| Secrets | Diff scanned with `/usr/bin/grep` for credential/key patterns: **0 hits**. **`SEC-5B1B-02`:** FU-351 `state_modifier_trace.rs` normalizes keys by stripping non-alphanumeric separators (matching `events.rs:6396-6416`), reuses `elitea_agent_runtime::memory_tools::looks_like_secret` alongside multi-URI credential scanning without pre-truncation lowercase copies, and strips internal `__*` keys (`redacts_secret_keys_secret_values_and_omits_internal_keys`, `redacts_platform_tokens_separator_keys_and_later_credentialed_uris`). **`SEC-5B1B-01`:** `#2240` failure detail is held only in memory until a claim-fenced, non-continued terminal stop commits (`node_recovery_runtime.rs`), never retained after retries or `on_error` continuations. |
| Supply chain and deployment | 0 new or upgraded dependencies. All scanners clean: `cargo deny --offline --all-features check advisories` (`advisories ok` on both `libs/rust` and `services/elitea-worker-rust`), `govulncheck ./internal/infra/storage ./internal/infra/db/repos ./internal/domain/pipelinelimits` (0 vulnerabilities in code or imported packages), `npm audit --omit=dev` (0 vulnerabilities). |
| Cross-layer differentials and version skew | 52 shared vectors pass identically in Rust, Go, and TypeScript (`node_settings_tests.rs`, `pipelinelimits`, `nodeSettings.helpers.test.ts`). **`SEC-5B1B-04`:** YAML `!!int` tag validation, `error_transition` label/numeric normalization, self-transition refusal, and own-property Web family lookup close cross-layer differentials. Mixed-version rollout fails closed in both directions (C7 proof above); `SCHEMA_V4` journal payloads fail closed on older Workers. |
| Agentic delegation and untrusted content isolation | Saved-child `node_errors` remains private to the child (C4, proven on real PG in FU-348). **`SEC-5B1B-01`:** Handled saved-child Code failures never write into the parent run's `terminal_detail` slot or `sandbox_run_failure_details` (`real_pg_sec_5b1b_01_saved_child_continued_code_failure_keeps_parent_detail_empty`). **`SEC-5B1B-03`:** Dedicated `failure_handler` nodes remain unreachable from `settings.error_transition`. Code `stdout`/`stderr` and failure messages stay bounded and cannot forge control frames, widen grants, or bypass `Reconcile` / `DeadlineInsufficient`. |
| TOCTOU, concurrency races, and effect replay | Claim-fenced ledger commits reject stale-claim zombies across mid-wait replacement (`real_pg_code_kill_mid_wait_resumes_same_try_fences_zombie_and_races_cleanly`). Non-idempotent/platform-client timeouts never re-execute (`real_pg_c1_platform_client_timeout_with_retry_on_fail_reconciles_zero_redispatch`). **`SEC-5B1B-01`:** `record_terminal_failure_detail` executes strictly after `journal.ensure_current()` confirms fencing on a non-continued terminal stop (`real_pg_sec_5b1b_01_retry_then_succeed_keeps_failure_detail_empty`, `real_pg_sec_5b1b_01_continued_failure_keeps_failure_detail_empty`, `real_pg_sec_5b1b_01_error_output_failure_keeps_failure_detail_empty`, and `#2240` `a_failed_programs_output_is_kept_for_the_terminal_and_durably_for_a_restart`). |
| Sandbox escape and resource isolation | Effective timeout strictly tightens (`min(profile.timeout_seconds, ceil(timeout_ms / 1000)).max(1)`) in Worker and is enforced server-side in Main `MatchCodePrepared` (**`SEC-5B1B-05`**); user YAML can never exceed the runtime profile's timeout ceiling. |
| AuthN, OAuth, PAT, and cryptographic hygiene | No OAuth/PAT/crypto primitives changed; `settings_digest` and `journal_activation_id` retain domain-separated SHA-256 hashing (`node_settings.rs:530`). |

---

## 7. Verification ledger and fixture honesty

| Suite / Gate | Command / Scope | Result |
|---|---|---|
| `libs/rust` fmt & clippy | `cargo fmt --all -- --check` + `cargo clippy -p elitea-agent-runtime --all-targets --all-features -- -D warnings` | PASS (0 warnings) |
| `libs/rust` feature matrix | `cargo test -p elitea-agent-runtime --features toolkit-sql` and `--features test-preserve-order,toolkit-sql` | PASS (59/59 each) |
| `libs/rust` advisories | `cargo deny --offline --all-features check advisories` | PASS (`advisories ok`) |
| Worker fmt & clippy | `cargo fmt --all -- --check` + `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` | PASS (0 warnings) |
| Worker targeted unit suites | `node_settings_tests`, `node_recovery_runtime::`, `code_remote`, `code_attempt_remote`, `code_runtime`, `code_slots`, `state_modifier_trace`, `pipeline_tests::` | PASS (485/485 pass: 481 in sandbox + 4 loopback tests verified outside sandbox) |
| Worker real-PG crash suite | `node_settings_pg_tests::` (1/1) + `node_settings_code_pg_tests::` (10/10) + `#2240` `a_failed_programs_output_is_kept_for_the_terminal_and_durably_for_a_restart` (1/1) against PostgreSQL 17 | PASS (12/12) |
| Worker benches (FU-347) | `cargo bench --offline --locked --features node-settings-bench --bench node_settings` | PASS (all 5 benchmarks well within budget) |
| Worker advisories | `cargo deny --offline --all-features check advisories` | PASS (`advisories ok`) |
| Go faults & Main | `go generate ./libs/go/faults`, `go test -race ./libs/go/faults/...`, `go vet`, `go test -race` on `pipelinelimits`, `storage` (`TestSavedCode\|TestCodeFailureRoute\|TestCodeFailureClass\|TestMatch`), `repos` (`TestOriginalCode\|TestRuntimeLostRetry`), real-PG `TestOriginalCodeSettingsRetryDispatchIdentityPostgres` | PASS (all touched packages green; untouched S3/Azure/TLS loopback tests skipped under sandbox network restriction) |
| Main advisories | `govulncheck ./internal/infra/storage ./internal/infra/db/repos ./internal/domain/pipelinelimits` | PASS (0 vulnerabilities in code/imports) |
| Web (Node 24) | `vitest` (4 files, 114/114 including 52 shared vectors), `typecheck`, `lint -- --deny-warnings` (0 warnings), `theme-gate`, `check-budgets`, `check-testid-namespace`, `check-layer-cycle`, `check-dead-code`, `i18n-backfill --check`, `check-handlers`, `npm audit --omit=dev` | PASS (all green) |
| Secret scan | `/usr/bin/grep` on full `git diff origin/main..HEAD` | PASS (0 hits) |
| **Live browser proof (cases 2–6)** | Docker image build + `stack.sh up` + Chrome DevTools verification | **DEFERRED** pending coordinator confirmation that Chrome DevTools is connected (no Docker images built yet) |

### Fixture creation honesty

- Real-PostgreSQL Worker and Main tests run against a real PostgreSQL 17 container with full migrations applied; sandbox execution outcomes inside Worker unit/PG tests use the repository's existing fake Supervisor transport (`FakeRemoteCode` / `CodeSandboxRuntime`) to inject deterministic timeouts, process kills, and mid-wait crashes without requiring a live container daemon inside unit tests.
- End-to-end live-stack + Chrome DevTools browser proof (cases 2–6) is explicitly deferred until the coordinator clears the browser phase.
