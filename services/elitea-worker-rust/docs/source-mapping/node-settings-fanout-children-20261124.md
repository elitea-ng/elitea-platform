# Fan-out child node `settings:` and per-thread pending visit (`5b-5`, `FU-071`)

- **Date:** 2026-11-24
- **Slice:** Point 5 / Wave 1 — `5b-5` (Fan-out child `settings:` + `FU-071` per-thread pending visit)
- **Status:** `partial` (unit, shared-vector, `-race`, and in-memory journal component suites verified; live PostgreSQL 18 and disposable Docker/browser verification deferred until stack runs are re-enabled)
- **Rollout note:** [`docs/upgrading/2026-11-24-071-fanout-child-node-settings.md`](../../../../docs/upgrading/2026-11-24-071-fanout-child-node-settings.md)
- **Recovery row:** `Worker x P13` (fan-out child automatic `settings:` and per-`graph_thread` pending visits) in [`../recovery-guarantees.md`](../recovery-guarantees.md)

## 1. Business behavior & deliberate non-ports

### Delivered behavior

1. **Three-layer admission parity for fan-out child automatic `settings:`:**
   - Lifted the `fanout_owned` admission refusal in Worker (`node_settings_definition.rs`), Main (`pipelinelimits/node_settings.go`, `limits.go`), and Web (`nodeSettings.helpers.ts`) for supported automatic-mode families:
     - Direct inline fan-out children: `Parallel` (`llm`, `code`) and `Map` (`state_modifier`, `llm`, `code`).
     - Saved child pipelines invoked by `Parallel`/`Map` `agent` workers: all automatic-mode families (`state_modifier`, `code`, `llm`, `toolkit`, `mcp`, `router`, `split_out`, `aggregate`).
   - Preserved scope isolation at save and compile time:
     - An owned fan-out child may only target `END` with `settings.error_transition`; any target naming a root node, a sibling branch/worker, or a non-existent node is refused with `error_transition_target`. Root nodes likewise remain unable to target fan-out-owned nodes or dedicated `failure_handler` nodes.
     - Extended C4 (`node_errors_nested_mapping`) across all three layers so `Map` `broadcast`/`outputs` and fan-out-owned child `input`/`output`/`input_mapping` cannot pass `node_errors` across the parent/child fan-out boundary (`SEC-5B5-02`).
   - Kept the `"fanout_owned"` refusal key in `libs/proto/contracts/pipeline-node-settings-v1.json` (and corresponding Rust/Go/Web refusal enums/tables) for backwards wire and contract compatibility.
   - Updated shared contract vectors in `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` (`84` Web contract tests + Worker/Go vector parity suites) to cover admitted `Map` worker `settings:`, refused escaping `error_transition`, admitted `Parallel` `llm`/`code` branches, refused `Parallel` `agent` branch (`family_not_yet`), and refused cross-boundary `node_errors` mappings (`Map` `broadcast`, `Parallel` `code` and `llm` `input`).

2. **Isolated per-child-thread runtime execution (`Parallel` & `Map`):**
   - `CompilerParallelBranches` (`parallel_compiler.rs`) compiles admitted `settings:` on owned `StateModifier` (`compile_item`), `Llm` (`compile_llm_branch`), and `Code` (`compile_code_branch`) fan-out children, binding `execution_id`, clamped `execution_deadline` (`min(runtimes.execution_deadline, binding.deadline)`), and per-child `node_errors` (`Append` reducer) + `EXECUTED_ONCE_KEY` state channels inside each child's private `StateSchema`.
   - `project_input` (`parallel_compiler.rs`) and `LlmNode::freeze_parallel_input` (`llm.rs`) strip `NODE_ERRORS_KEY` from parent state before seeding child inputs, and `code_child` filters out `self.settings.owns_key(key)` so neither explicit inputs nor `reads_all()` leak parent `node_errors` across the fan-out boundary (`SEC-5B5-02`).
   - `project_input` stamps a deterministic `APPLICATION_VISIT_STATE_KEY` (`llm_child_visit`) on inline `OwnedBranch::Llm` children (matching `OwnedBranch::Code`'s `code_child_visit`), and `compile_llm_branch` includes `APPLICATION_VISIT_STATE_KEY` in the child schema (`SEC-5B5-01`).
   - `LlmNode::execute_journaled` (`llm.rs`) handles restored `CHILD_EFFECT_UNKNOWN_CODE` (and defensively any `pipeline_node_recovery` interrupt on a visit-scoped child) by failing closed as `may_have_started` with `0` re-dispatches (`SEC-5B5-01`).
   - `PipelineDefinition::result_policy()` (`compiler.rs`) skips `parallel_owned_nodes` and `map_owned_nodes` so fan-out children declaring `settings: { on_error: error_output, error_transition: END }` never inject child-local output keys into root `terminal_data_keys` (`CR-5B5-02`).
   - `owned_definition_digest` folds non-default admitted `settings:` into the branch/worker digest so editing `settings:` invalidates stale replays while pipelines without `settings:` (or with default-only `settings:`) keep byte-identical digests.
   - `Map` child pipelines (such as `scripts/acceptance/etl/pipelines/silver_batch.yaml`, whose `validate` Code node uses `settings: { on_error: error_output, error_transition: batch_failed }`) compile and run per-item with isolated `node_errors`, `error_transition` routing, and `execute_once` loop behavior.

3. **Strict visit-scoped child threads in `PostgresCheckpointer::open` (`node_attempts.rs`):**
   - Keeps `in_child != activation.visit.is_some()` unconditionally fail-closed (`pipeline.node_recovery.unscoped_child`) for all child threads (saved child, direct Code child, direct LLM child) while validating canonical 32-byte URL-safe base64 no-pad decoding on direct `p1:`/`m1:` threads (`is_direct_fanout_child_thread`, rejecting non-canonical trailing bits with `pipeline.node_recovery.invalid_scope`) (`SEC-5B5-01`).
   - `parallel_children.rs` (`for_branch`, `prepare_children`) lends each branch/item its own child-scoped `node_recovery` factory (`Some(child)`), ensuring retry attempts and journal activation IDs never collide across siblings.

4. **Per-`graph_thread` pending node-recovery visits (`FU-071`) + Main RBAC:**
   - Added shared migration `services/elitea-main/migrations/shared/0271_node_recovery_per_thread_visit.sql` replacing `node_recovery_one_pending_visit` on `(execution_id, generation)` with `(execution_id, generation, graph_thread)` (partial unique index `WHERE status IN ('SUSPENDED', 'AUTHORIZED')`) and adding `node_recovery_latest_visit_by_thread` on `(execution_id, generation, graph_thread, created_at DESC, journal_revision DESC)`.
   - Updated `manifest_test.go` (`Head(shared) == 271`).
   - Updated Main repository, application, and HTTP layers (`repos/node_recovery*.go`, `repos/code_sandbox_recovery.go`, `application/noderecovery/service.go`, `domain/noderecovery/contract.go`, `api/v2/agentexecution/node_recovery.go`) so:
     - Sibling fan-out threads can suspend concurrently (`desired_state IN ('RUNNING','SUSPENDED')` in `projectNodeRecovery`).
     - `GET /node_recovery` accepts optional single query parameter `?graph_thread=<valid-thread>`, pre-bounding raw query length at `2048` bytes, rejecting `&`/`;` delimiters prior to parsing, and validating via `noderecovery.ValidGraphThread` (`<= 512` bytes, ASCII printable, no control/whitespace/Unicode separators); `POST /actions` rejects any query params (`SEC-5B5-03`).
     - `CodeIntentRepository.recoveryRecord` (`code_sandbox_recovery.go`) locks per-thread latest visit via `lockLatestNodeRecoveryVisitForActivation`, and `NodeRecoveryRepository.ReadNodeRecoveryResult` (`node_recovery_result.go`) locks per-thread latest visit by result proof (`lockLatestNodeRecoveryVisitForResult`) while permitting `RESUMED` reads when `desired_state == 'SUSPENDED'` strictly if another sibling thread has a pending visit (`CR-5B5-01`).
     - All multi-thread visit queries (`node_recovery.go`, `node_recovery_claim.go`, `node_recovery_control.go`, `node_recovery_current.go`) include deterministic tie-breakers (`graph_thread ASC, activation_id DESC`) and consistent status ordering (`AUTHORIZED` > `SUSPENDED` > `STOPPED` > `RESUMED`/others) (`CR-5B5-03`).
     - `AcknowledgeNodeRecovery` preserves `desired_state = 'SUSPENDED'` via `CASE WHEN EXISTS(...)` whenever another sibling thread still has a pending visit, and supports idempotent multi-thread ack replay.
     - Existing wire JSON shapes (`NodeRecoveryControl`, `NodeRecoveryAck`, `NodeRecoveryState`) remain unchanged (`#[serde(deny_unknown_fields)]` compatible with Worker).

### Deliberate non-ports / out of scope

- **No `5b-4` (`agent` / child-pipeline node `settings:`):** `agent` nodes remain refused with `family_not_yet` (both top-level and inside fan-out), per the slice boundary.
- **No operator-mode `recovery:` on fan-out children:** `recovery:` blocks on fan-out-owned nodes remain refused; `settings:` + `recovery:` on the same node remains refused (`with_recovery`).
- **No Web UI component/settings-page changes:** Only the strict 3-layer admission parity fix in `nodeSettings.helpers.ts` and its unit test were touched.
- **No `http_action` / `dbactions` / `pipeline-node-admission-v1.json` edits:** Untouched per ownership boundary.

## 2. Changed paths

| Layer | Path | Purpose |
| --- | --- | --- |
| Contract | `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` | Update Map worker vectors; add Parallel LLM/Code, escaping `error_transition`, Parallel Agent refusal, and C4 fan-out `node_errors_nested_mapping` vectors |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/node_settings_definition.rs` | Lift `FanoutOwned` refusal; refuse fan-out child `error_transition != "END"`; extend C4 `node_errors_nested_mapping` across `Map` and owned fan-out children |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/compiler.rs` | Skip `parallel_owned_nodes` and `map_owned_nodes` in `result_policy()` (`CR-5B5-02`) |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/llm.rs` | Exclude `NODE_ERRORS_KEY` in `freeze_parallel_input`; fail closed on restored `CHILD_EFFECT_UNKNOWN_CODE` / child `pipeline_node_recovery` interrupt in `execute_journaled` (`SEC-5B5-01`, `SEC-5B5-02`) |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/parallel_compiler.rs` | Wire `node_settings`, `execution_id`, `execution_deadline`, child `node_errors`/`EXECUTED_ONCE_KEY` channels, `llm_child_visit` `APPLICATION_VISIT_STATE_KEY` stamping, parent `node_errors` stripping, and `owned_definition_digest` folding |
| Worker (Rust) | `services/elitea-worker-rust/src/state/postgres_checkpointer/node_attempts.rs` | Enforce unconditional child visit scoping (`unscoped_child`) and canonical 32-byte base64url validation on direct `p1:`/`m1:` child threads (`SEC-5B5-01`) |
| Worker (Rust) | `services/elitea-worker-rust/src/state/postgres_checkpointer/parallel_children.rs` | Lend branch-scoped `node_recovery` (`Some(child)`) in `for_branch` and `prepare_children` |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/node_settings_fanout_tests.rs` | 10 end-to-end fan-out proof tests (admission/bounds/escaping/digest, Map StateModifier, Parallel LLM, Map Code, `silver_batch.yaml` Map child pipeline, LLM crash/restore `MayHaveStarted`, Code `UnknownExternalEffect`, parent/child `node_errors` isolation, 64/65 cap + 512 B + redaction, `result_policy` exclusion) |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/mod.rs`, `node_settings_tests.rs`, `parallel_*_tests.rs` | Register new test module, update Map admission assertion, add new struct fields to 4 `CompilerParallelBranches` test fixtures |
| Main (SQL) | `services/elitea-main/migrations/shared/0271_node_recovery_per_thread_visit.sql` | Replace `node_recovery_one_pending_visit` with `(execution_id, generation, graph_thread)` (`FU-071`) + add `node_recovery_latest_visit_by_thread` |
| Main (Go) | `services/elitea-main/internal/infra/db/migrate/manifest_test.go` | Update `Head(shared)` from `270` to `271` |
| Main (Go) | `services/elitea-main/internal/domain/pipelinelimits/{limits.go,node_settings.go,node_settings_test.go}` | Capture `input`/`broadcast`/`outputs` in `nodeFields`; lift `fanout_owned`; refuse escaping `error_transition` and cross-boundary `node_errors_nested_mapping`; add unit tests |
| Main (Go) | `services/elitea-main/internal/domain/noderecovery/contract.go` | Export `ValidGraphThread` (`<= 512` B) |
| Main (Go) | `services/elitea-main/internal/application/noderecovery/service.go` | Optional validated `GraphThread` field on `Selector` |
| Main (Go) | `services/elitea-main/internal/api/v2/agentexecution/node_recovery.go`, `node_recovery_test.go` | Pre-bounded (`<= 2048` B), delimiter-hardened `?graph_thread=` parsing + full §3a Authorization Matrix (owner, member, foreign-project, other user, unauthenticated `401`, token vs session, 512/513 & smuggling boundaries) |
| Main (Go) | `services/elitea-main/internal/infra/db/repos/{node_recovery,node_recovery_claim,node_recovery_control,node_recovery_current,node_recovery_projection,node_recovery_result,code_sandbox_recovery}.go`, `{node_recovery_result_test,node_recovery_per_thread_test}.go` | Per-`graph_thread` visit locking (incl. Code verification & result read), deterministic tie-breakers & `STOPPED` precedence, concurrent projection, multi-thread `desired_state` preservation/replay, SQL scope predicate assertions, and `-race` tests |
| Main (Go) | `services/elitea-main/internal/infra/storage/node_recovery_control_test.go` | Fan-out child thread ack & control-character rejection test |
| Web (TS) | `apps/elitea-web/src/features/pipelines/lib/nodeSettings.helpers.ts`, `nodeSettings.helpers.test.ts` | Lift `fanout_owned`, refuse escaping `error_transition`, extend C4 `node_errors_nested_mapping`, add parity test |
| Docs | `docs/upgrading/2026-11-24-071-fanout-child-node-settings.md`, `services/elitea-worker-rust/docs/recovery-guarantees.md`, `services/elitea-worker-rust/docs/source-mapping/{README.md,node-settings-fanout-children-20261124.md}` | Upgrade guidance, `Worker x P13` recovery row, and source mapping |

## 3. Observed verification results

### Executed & passing

| Gate | Command | Result |
| --- | --- | --- |
| Rust formatting | `cargo fmt --all -- --check` (under `slot.sh`) | PASS |
| Rust clippy | `cargo clippy --offline --locked --all-targets -- -D warnings` (under `slot.sh`) | PASS (`0` warnings, `0` errors) |
| Rust full graph & checkpointer tests | `cargo test --offline --locked -p elitea-worker-rust --lib -- agents::graph:: state::postgres_checkpointer::` | PASS (`786 passed; 0 failed; 3 ignored`, incl. all `10` `node_settings_fanout_tests` and shared vector parity) |
| Rust advisory audit | `cargo deny --all-features check advisories` | PASS (`advisories ok`) |
| Go `-race` tests | `GOMAXPROCS=4 go test -p 2 -count=1 -race ./internal/infra/db/migrate/... ./internal/domain/pipelinelimits/... ./internal/domain/noderecovery/... ./internal/application/noderecovery/... ./internal/infra/storage/... ./internal/infra/db/repos/... ./internal/api/v2/agentexecution/...` | PASS across all 10 packages (`migrate`, `pipelinelimits`, `noderecovery`, `storage/...`, `repos`, `agentexecution`) |
| Go vet | `go vet` on all 7 touched Go package trees | PASS (`0` diagnostics) |
| Go vulnerability audit | `govulncheck ./...` | PASS (`0` vulnerabilities affecting called code; 1 pre-existing uncalled module entry) |
| Web typecheck | `npm run typecheck` (`tsc --noEmit`) | PASS |
| Web contract & helper tests | `npx vitest run src/features/pipelines/lib/nodeSettings.helpers.test.ts src/features/pipelines/lib/flow-editor/constants/runtimeContract.nodeSettings.test.ts src/features/pipelines/ui/settings/NodeSettings.test.tsx` | PASS (`3/3` files, `103/103` tests) |
| Upgrading notes CI check | `node scripts/ci/check-upgrading-notes.mjs` | PASS (`173` notes in `docs/upgrading/`) |
| Secret scan | Regex scan over `git diff origin/main` | PASS (`0` hits) |

### Review hardening closed (neutral summary)

- `SEC-5B5-01`: Stamped deterministic `APPLICATION_VISIT_STATE_KEY` on inline fan-out LLM branches, restored unconditional `in_child != activation.visit.is_some()` enforcement with canonical 32-byte base64url validation in `node_attempts.rs`, hardened `LlmNode::execute_journaled` restore path, and added crash/restore proving test.
- `SEC-5B5-02`: Extended C4 `node_errors_nested_mapping` across Worker/Main/Web + shared vectors for `Map` and fan-out-owned children, stripped `NODE_ERRORS_KEY` at fan-out input projection, and added proving tests for Code `UnknownExternalEffect`, parent/child `node_errors` isolation, 64/65 cap, 512 B error-object bound, and sentinel redaction.
- `SEC-5B5-03`: Pre-bounded raw query length (`2048` B) and rejected `&`/`;` delimiters in `parseNodeRecoveryReadThread`; completed §3a Authorization Matrix and SQL scope-predicate/argument assertions.
- `CR-5B5-01`: Updated `code_sandbox_recovery.go` and `node_recovery_result.go` to per-thread visit locking and multi-thread `desired_state` handling with proving tests.
- `CR-5B5-02`: Excluded `parallel_owned_nodes` and `map_owned_nodes` from `compiler.rs::result_policy()` with unit test.
- `CR-5B5-03`: Added deterministic tie-breakers (`graph_thread ASC, activation_id DESC`) and consistent `STOPPED` > `RESUMED` precedence across multi-thread Main queries with unit tests.
- `CR-5B5-04`: Corrected table name (`elitea_runtime.node_recovery_visits`), contract bounds, family scope, 12 `rules/security.md` category titles, and removed unused test import.

### Skipped / deferred checks (reported honestly)

- **Live PostgreSQL 18 integration tests & disposable Docker/browser stack:** Not started per task instructions ("Do NOT spin up Docker stacks yet"). Pending before un-drafting: live PostgreSQL 18 migration `0271` application + concurrent sibling `node_recovery_visits` integration proof and real-browser verification of `silver_batch`-shaped Map child pipeline execution.
- **Migration numbering coordination:** Migration `0271_node_recovery_per_thread_visit.sql` uses contiguous `0271` so `manifest_test.go` passes against `origin/main` (`Head(shared) == 270`). If PR #3018 (`5e-2`, also `0271_db_action_rules.sql`) merges to `main` first, renumber to `0272` and update `manifest_test.go` at merge.

## 4. Performance, Durability, Resilience & Security

- **Performance:** Added partial index `node_recovery_one_pending_visit` on `(execution_id, generation, graph_thread)` and composite lookup index `node_recovery_latest_visit_by_thread` on `(execution_id, generation, graph_thread, created_at DESC, journal_revision DESC)` so per-thread reads, locks, and sibling pending checks avoid full-table scans. Bounded `node_errors` cap (`64` entries, `<= 512` B error object / `<= 1024` B entry), `max_tries` (`2..=5`), `wait_between_tries_ms` (`100..=300_000`), and `timeout_ms` ceilings remain enforced per child thread.
- **Durability & Resilience:** Every fan-out child owns an independent `p1:`/`m1:` checkpoint thread and visit-scoped fenced `n1:` node-attempt journal rooted at `run_root_thread_id`; `UnknownExternalEffect` on Code and `MayHaveStarted` on inline LLM children stop fail-closed (`0` blind repeats, no `pipeline_node_recovery` interrupt escape) even when `retry_on_fail` / `on_error: continue` is configured. Main preserves `desired_state = 'SUSPENDED'` atomically until every sibling thread's pending visit has settled.
- **Security (`rules/security.md` 12-category audit):**

| # | `rules/security.md` Category | Status | Evidence / Rationale |
| --- | --- | --- | --- |
| 1 | Trust boundaries and identity | Checked — pass | `NewCurrentNodeRecoveryRoute` requires authenticated owning user (`user.OwningUserID()`); Worker control RPCs require verified mTLS peer cert (`workloadidentity.Certificate`) and live claim fence token (`lockRecoveryClaim`); `graph_thread` is an object selector never treated as identity. |
| 2 | Authorization (object level) | Checked — pass | `GET /node_recovery?graph_thread=...` and `POST /actions` enforce project RBAC (`models.applications.task.get` / `models.chat.messages.create`) plus `lockPublicRecoveryScope` (`tenant_id`, `resource_project_id`, `projection_project_id`, `actor_id`, `conversation.author_id`); verified by full §3a matrix in `TestCurrentNodeRecoveryRoutePerThreadReadAndAuthorizationMatrix` and SQL predicate/arg assertions in `TestNodeRecoveryFanoutChildVisitRefusesForeignActorAndForeignProject` (`SEC-5B5-03`). |
| 3 | Input, parsing and amplification | Checked — pass | `parseNodeRecoveryReadThread` pre-bounds raw query at `2048` B, rejects `&`/`;` before parsing, and validates `noderecovery.ValidGraphThread` (`<= 512` B, rejects control/Unicode line separators/malformed encoding); `node_errors` enforces `64`/`65` (`pipeline.node_errors.exhausted`), `512` B error-object / `1024` B entry bounds (`SEC-5B5-02`, `SEC-5B5-03`); `POST /actions` capped at `8192` B. |
| 4 | Injection and construction | Checked — pass | All SQL in `node_recovery*.go`, `code_sandbox_recovery.go`, and migration `0271` uses parameterized `$1..$12`; no shell, raw HTML, or unsandboxed template sinks introduced. |
| 5 | Egress and SSRF | N/A | No new outbound HTTP/network clients or egress paths introduced (`http_action` untouched). |
| 6 | Secrets | Checked — pass | Secret scan on diff returned `0` hits; `fanout_child_node_errors_enforces_64_65_cap_truncation_and_secret_redaction` proves sentinel secrets are absent from `node_errors`, emitted events, and returned outputs (`SEC-5B5-02`). |
| 7 | Supply chain and deployment | Checked — pass | Zero dependencies added or upgraded; `cargo deny --all-features check advisories` (`advisories ok`), `govulncheck ./...` (`0` called vulns), and `check-upgrading-notes.mjs` (`173` notes) pass; migration `0271` is online-compatible. |
| 8 | Cross-layer differentials and version skew | Checked — pass | 3-layer admission parity verified across Rust, Go (`limits.go`/`node_settings.go`), and Web (`nodeSettings.helpers.ts`) on all shared vectors (incl. C4 fan-out `node_errors_nested_mapping`); `result_policy()` excludes fan-out owned outputs (`CR-5B5-02`); wire JSON shapes unchanged (`#[serde(deny_unknown_fields)]`). |
| 9 | Agentic delegation and untrusted content isolation | Checked — pass | Parent and fan-out child `node_errors` are strictly isolated at admission (C4 across all 3 layers) and runtime (`project_input`, `freeze_parallel_input`, `code_child`), verified by `parent_and_fanout_children_isolate_node_errors_across_parallel_and_map` (`SEC-5B5-02`). |
| 10 | TOCTOU, concurrency races, and effect replay | Checked — pass | Unconditional visit scoping on all child threads + fail-closed `MayHaveStarted` crash/restore (`SEC-5B5-01`); Code `UnknownExternalEffect` `0`-retry refusal (`SEC-5B5-02`); per-thread locking in `code_sandbox_recovery.go` / `node_recovery_result.go` (`CR-5B5-01`), deterministic tie-breakers & `STOPPED` precedence (`CR-5B5-03`), `FOR UPDATE` locks, partial unique index per `graph_thread`, and Go `-race` clean. |
| 11 | Sandbox escape and resource isolation | Checked — pass | Fan-out Code child sandbox isolation unchanged; `owned_definition_digest` binds non-default settings; `max_tries` (`2..=5`), `wait_between_tries_ms` (`100..=300_000`), and `timeout_ms` clamped to `min(execution_deadline, fanout_deadline)`. |
| 12 | AuthN, OAuth, PAT, and cryptographic hygiene | Checked — pass | `is_direct_fanout_child_thread` validates canonical 32-byte URL-safe base64url decode (`SEC-5B5-01`); route §3a matrix covers unauthenticated `401` and both session and bearer-token AuthN paths (`SEC-5B5-03`); existing SHA-256 activation/config/receipt digests preserved. |
