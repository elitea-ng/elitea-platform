# Fan-out child node `settings:` and per-thread pending visit (`5b-5`, `FU-071`)

- **Date:** 2026-11-24
- **Slice:** Point 5 / Wave 1 — `5b-5` (Fan-out child `settings:` + `FU-071` per-thread pending visit)
- **Status:** `partial` (unit, shared-vector, `-race`, and in-memory journal component suites verified; live PostgreSQL 18 and disposable Docker/browser verification deferred until stack runs are re-enabled)
- **Rollout note:** [`docs/upgrading/2026-11-24-071-fanout-child-node-settings.md`](../../../../docs/upgrading/2026-11-24-071-fanout-child-node-settings.md)
- **Recovery row:** `Worker x P13` (fan-out child automatic `settings:` and per-`graph_thread` pending visits) in [`../recovery-guarantees.md`](../recovery-guarantees.md)

## 1. Business behavior & deliberate non-ports

### Delivered behavior

1. **Three-layer admission parity for fan-out child automatic `settings:`:**
   - Lifted the `fanout_owned` admission refusal in Worker (`node_settings_definition.rs`), Main (`pipelinelimits/node_settings.go`), and Web (`nodeSettings.helpers.ts`) for already-admitted node families (`state_modifier`, `code`, `llm`, `toolkit`, `mcp`, `router`, `split_out`, `aggregate`) in automatic mode when owned by a `parallel` branch or `map` worker.
   - Preserved scope isolation at save and compile time: an owned fan-out child may only target `END` with `settings.error_transition`; any target naming a root node, a sibling branch/worker, or a non-existent node is refused with `error_transition_target`. Root nodes likewise remain unable to target fan-out-owned nodes or dedicated `failure_handler` nodes.
   - Kept the `"fanout_owned"` refusal key in `libs/proto/contracts/pipeline-node-settings-v1.json` (and corresponding Rust/Go/Web refusal enums/tables) for backwards wire and contract compatibility.
   - Updated shared contract vectors in `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` to cover admitted `Map` worker `settings:` (`on_error: continue`, `on_error: error_output` with `error_transition: END`), refused escaping `error_transition`, admitted `Parallel` `llm` and `code` branch `settings:`, refused `Parallel` sibling `error_transition`, and refused `Parallel` `agent` branch (`family_not_yet`).

2. **Isolated per-child-thread runtime execution (`Parallel` & `Map`):**
   - `CompilerParallelBranches` (`parallel_compiler.rs`) now compiles admitted `settings:` on owned `StateModifier` (`compile_item`), `Llm` (`compile_llm_branch`), and `Code` (`compile_code_branch`) fan-out children, binding `execution_id`, clamped `execution_deadline` (`min(runtimes.execution_deadline, binding.deadline)`), and per-child `node_errors` (`Append` reducer) + `EXECUTED_ONCE_KEY` state channels inside each child's private `StateSchema`.
   - `code_child` explicitly filters out parent-owned `node_errors` (`!self.settings.owns_key(key)`) so `reads_all()` never leaks parent `node_errors` into a child thread.
   - `compile_llm_branch` seeds declared outputs with `typed_default(kind)` when `settings:` is admitted so `on_error: continue` without `always_output_data` projects valid typed outputs.
   - `owned_definition_digest` folds non-default admitted `settings:` into the branch/worker digest so editing `settings:` invalidates stale replays while pipelines without `settings:` (or with default-only `settings:`) keep byte-identical digests.
   - `Map` child pipelines (such as `scripts/acceptance/etl/pipelines/silver_batch.yaml`, whose `validate` Code node uses `settings: { on_error: error_output, error_transition: batch_failed }`) compile and run per-item with isolated `node_errors`, `error_transition` routing, and `execute_once` loop behavior.

3. **Authorized direct fan-out child threads in `PostgresCheckpointer::open` (`node_attempts.rs`):**
   - Lifted the root-thread-only journal restriction cleanly for canonical direct fan-out child threads (`p1:` or `m1:` followed by exactly 43 URL-safe base64 no-pad characters, no `/` suffix) via `is_direct_fanout_child_thread`.
   - Kept every existing safety fence intact: `activation.root_thread_id == self.scope.authority.thread_id`, `state_writer_lease.ensure_current()`, `activate_under_root(..., &self.run_root_thread_id, ...)`, and strict `unscoped_child` enforcement for saved-child (`/...`) threads.
   - `parallel_children.rs` (`for_branch`, `prepare_children`) lends each branch/item its own child-scoped `node_recovery` factory (`Some(child)`), ensuring retry attempts and journal activation IDs never collide across siblings.

4. **Per-`graph_thread` pending node-recovery visits (`FU-071`) + Main RBAC:**
   - Added shared migration `services/elitea-main/migrations/shared/0271_node_recovery_per_thread_visit.sql` replacing `node_recovery_one_pending_visit` on `(execution_id, generation)` with `(execution_id, generation, graph_thread)` (partial unique index `WHERE status IN ('SUSPENDED', 'AUTHORIZED')`) and adding `node_recovery_latest_visit_by_thread` on `(execution_id, generation, graph_thread, created_at DESC, journal_revision DESC)`.
   - Updated `manifest_test.go` (`Head(shared) == 271`).
   - Updated Main repository, application, and HTTP layers (`repos/node_recovery*.go`, `application/noderecovery/service.go`, `domain/noderecovery/contract.go`, `api/v2/agentexecution/node_recovery.go`) so:
     - Sibling fan-out threads can suspend concurrently (`desired_state IN ('RUNNING','SUSPENDED')` in `projectNodeRecovery`).
     - `GET /node_recovery` accepts optional single query parameter `?graph_thread=<valid-thread>` (strictly validated with `noderecovery.ValidGraphThread`; unknown/duplicate/control-char params fail 400; `POST /actions` still rejects any query params).
     - `Read`, `PollNodeRecovery`, and `loadLatestNodeRecoveryClaimVisit` prioritize pending visits (`AUTHORIZED` / `SUSPENDED`) over already-resolved threads while never leaking a mismatched `graph_thread`.
     - `AcknowledgeNodeRecovery` locks the latest visit for the exact `(execution_id, generation, activation_id, expected_revision)`'s `graph_thread`, preserves `desired_state = 'SUSPENDED'` via `CASE WHEN EXISTS(...)` whenever another sibling thread still has a pending visit, and supports idempotent multi-thread ack replay.
     - Existing wire JSON shapes (`NodeRecoveryControl`, `NodeRecoveryAck`, `NodeRecoveryState`) remain unchanged (`#[serde(deny_unknown_fields)]` compatible with Worker).

### Deliberate non-ports / out of scope

- **No `5b-4` (`agent` / child-pipeline node `settings:`):** `agent` nodes remain refused with `family_not_yet` (both top-level and inside fan-out), per the slice boundary.
- **No operator-mode `recovery:` on fan-out children:** `recovery:` blocks on fan-out-owned nodes remain refused; `settings:` + `recovery:` on the same node remains refused (`with_recovery`).
- **No Web UI component/settings-page changes:** Only the strict 3-layer admission parity fix in `nodeSettings.helpers.ts` and its unit test were touched.
- **No `http_action` / `dbactions` / `pipeline-node-admission-v1.json` edits:** Untouched per ownership boundary.

## 2. Changed paths

| Layer | Path | Purpose |
| --- | --- | --- |
| Contract | `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` | Update Map worker vectors; add Parallel LLM/Code, escaping `error_transition`, and Parallel Agent refusal vectors |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/node_settings_definition.rs` | Lift `FanoutOwned` refusal; refuse fan-out child `error_transition != "END"` |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/parallel_compiler.rs` | Wire `node_settings`, `execution_id`, `execution_deadline` into `CompilerParallelBranches` (`compile_item`, `compile_llm_branch`, `compile_code_branch`), child `node_errors`/`EXECUTED_ONCE_KEY` channels, parent `node_errors` exclusion, and `owned_definition_digest` folding |
| Worker (Rust) | `services/elitea-worker-rust/src/state/postgres_checkpointer/node_attempts.rs` | Add `is_direct_fanout_child_thread` (`p1:`/`m1:` + 43 URL-safe base64 chars), allow direct fan-out child threads in `PostgresCheckpointer::open`, plus 2 unit tests |
| Worker (Rust) | `services/elitea-worker-rust/src/state/postgres_checkpointer/parallel_children.rs` | Lend branch-scoped `node_recovery` (`Some(child)`) in `for_branch` and `prepare_children` |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/node_settings_fanout_tests.rs` | 5 end-to-end fan-out proof tests (admission/bounds/escaping/digest, Map StateModifier, Parallel LLM, Map Code, `silver_batch.yaml` Map child pipeline + `execute_once` loop) |
| Worker (Rust) | `services/elitea-worker-rust/src/agents/graph/mod.rs`, `node_settings_tests.rs`, `parallel_*_tests.rs` | Register new test module, update Map admission assertion, add new struct fields to 4 `CompilerParallelBranches` test fixtures |
| Main (SQL) | `services/elitea-main/migrations/shared/0271_node_recovery_per_thread_visit.sql` | Replace `node_recovery_one_pending_visit` with `(execution_id, generation, graph_thread)` (`FU-071`) + add `node_recovery_latest_visit_by_thread` |
| Main (Go) | `services/elitea-main/internal/infra/db/migrate/manifest_test.go` | Update `Head(shared)` from `270` to `271` |
| Main (Go) | `services/elitea-main/internal/domain/pipelinelimits/node_settings.go`, `node_settings_test.go` | Lift `fanout_owned`, refuse escaping `error_transition` on fan-out children, add unit test |
| Main (Go) | `services/elitea-main/internal/domain/noderecovery/contract.go` | Export `ValidGraphThread` |
| Main (Go) | `services/elitea-main/internal/application/noderecovery/service.go` | Optional validated `GraphThread` field on `Selector` |
| Main (Go) | `services/elitea-main/internal/api/v2/agentexecution/node_recovery.go`, `node_recovery_test.go` | Strict `?graph_thread=` parsing on `GET /node_recovery` + per-thread read and foreign-actor refusal test |
| Main (Go) | `services/elitea-main/internal/infra/db/repos/node_recovery{,_claim,_control,_current,_projection}.go`, `node_recovery_per_thread_test.go` | Per-`graph_thread` visit locking, priority ordering, concurrent projection, multi-thread `desired_state` preservation & replay, and `-race` + negative authz tests |
| Main (Go) | `services/elitea-main/internal/infra/storage/node_recovery_control_test.go` | Fan-out child thread ack & control-character rejection test |
| Web (TS) | `apps/elitea-web/src/features/pipelines/lib/nodeSettings.helpers.ts`, `nodeSettings.helpers.test.ts` | Lift `fanout_owned`, refuse escaping `error_transition`, add parity test |
| Docs | `docs/upgrading/2026-11-24-071-fanout-child-node-settings.md`, `services/elitea-worker-rust/docs/recovery-guarantees.md`, `services/elitea-worker-rust/docs/source-mapping/{README.md,node-settings-fanout-children-20261124.md}` | Upgrade guidance, `Worker x P13` recovery row, and source mapping |

## 3. Observed verification results

### Executed & passing

| Gate | Command | Result |
| --- | --- | --- |
| Rust formatting | `cargo fmt --all -- --check` (under `slot.sh`) | PASS |
| Rust clippy | `cargo clippy --offline --locked --all-targets -- -D warnings` (under `slot.sh`) | PASS (`0` warnings, `0` errors) |
| Rust targeted fan-out & checkpointer tests | `cargo test --offline --locked -p elitea-worker-rust --lib -- agents::graph::node_settings agents::graph::parallel_compiler agents::graph::fanout_code_child agents::graph::parallel_cards agents::graph::map_fanout state::postgres_checkpointer::node_attempts state::postgres_checkpointer::fanout_children` | PASS (`107 passed; 0 failed; 0 ignored`) |
| Rust full graph & checkpointer tests | `cargo test --offline --locked -p elitea-worker-rust --lib -- agents::graph:: state::postgres_checkpointer::` | PASS (`781 passed; 0 failed; 3 ignored`) |
| Rust advisory audit | `cargo deny --all-features check advisories` | PASS (`advisories ok`) |
| Go `-race` tests | `GOMAXPROCS=4 go test -p 2 -count=1 -race ./internal/infra/db/migrate/... ./internal/domain/pipelinelimits/... ./internal/domain/noderecovery/... ./internal/application/noderecovery/... ./internal/infra/storage/... ./internal/infra/db/repos/... ./internal/api/v2/agentexecution/...` | PASS across all 10 packages (`migrate`: 15, `pipelinelimits`: 42, `noderecovery`: 7, `storage`: 411 + azure/conformance/gcs/s3, `repos`: 545, `agentexecution`: 87; `1,107+` tests) |
| Go vet | `go vet` on all 7 touched Go package trees | PASS (`0` diagnostics) |
| Go vulnerability audit | `govulncheck ./...` | PASS (`0` vulnerabilities affecting called code; 1 pre-existing uncalled module entry) |
| Web typecheck | `npm run typecheck` (`tsc --noEmit`) | PASS |
| Web contract & helper tests | `npx vitest run src/features/pipelines/lib/nodeSettings.helpers.test.ts src/features/pipelines/lib/flow-editor/constants/runtimeContract.nodeSettings.test.ts src/features/pipelines/ui/settings/NodeSettings.test.tsx` | PASS (`3/3` files, `100/100` tests) |
| Upgrading notes CI check | `node scripts/ci/check-upgrading-notes.mjs` | PASS (`173` notes in `docs/upgrading/`) |
| Secret scan | Regex scan over `git diff origin/main` | PASS (`0` hits) |

### Skipped / deferred checks (reported honestly)

- **Live PostgreSQL 18 integration tests & disposable Docker/browser stack:** Not started per task instructions ("Do NOT spin up Docker stacks yet"). Pending before un-drafting: live PostgreSQL 18 migration `0271` application + concurrent sibling `node_recovery_visits` integration proof and real-browser verification of `silver_batch`-shaped Map child pipeline execution.
- **Migration numbering coordination:** Migration `0271_node_recovery_per_thread_visit.sql` uses contiguous `0271` so `manifest_test.go` passes against `origin/main` (`Head(shared) == 270`). If PR #3018 (`5e-2`, also `0271_db_action_rules.sql`) merges to `main` first, renumber to `0272` and update `manifest_test.go` at merge.

## 4. Performance, Durability, Resilience & Security

- **Performance:** Added partial index `node_recovery_one_pending_visit` on `(execution_id, generation, graph_thread)` and composite lookup index `node_recovery_latest_visit_by_thread` on `(execution_id, generation, graph_thread, created_at DESC, journal_revision DESC)` so per-thread reads, locks, and sibling pending checks avoid full-table scans. Bounded `node_errors` cap (`64` entries) and `max_tries` (`<= 5`) / `wait_between_tries_ms` (`<= 5000`) / `timeout_ms` ceilings remain unchanged per child thread.
- **Durability & Resilience:** Each fan-out child owns an independent `p1:`/`m1:` checkpoint thread and fenced `n1:` node-attempt journal rooted at `run_root_thread_id`; `UnknownExternalEffect` on Code children continues to stop fail-closed (`child_effect_unknown`, `0` blind repeats) even when `on_error: continue` or `error_output` is configured. Main preserves `desired_state = 'SUSPENDED'` atomically until every sibling thread's pending visit has settled.
- **Security (`rules/security.md` 12-category audit):**

| # | Category | Status | Evidence / Rationale |
| --- | --- | --- | --- |
| 1 | Authentication & identity | Checked — pass | `NewCurrentNodeRecoveryRoute` requires authenticated owning user (`user.OwningUserID()`); Worker control RPCs require verified mTLS peer cert (`workloadidentity.Certificate`) and live claim fence token (`lockRecoveryClaim`). |
| 2 | Authorization & multi-tenant isolation | Checked — pass | `GET /node_recovery?graph_thread=...` and `POST /actions` enforce project RBAC (`models.applications.task.get` / `models.chat.messages.create`) plus `lockPublicRecoveryScope` (`tenant_id`, `resource_project_id`, `projection_project_id`, `actor_id`, `conversation.author_id`); verified by `TestNodeRecoveryFanoutChildVisitRefusesForeignActorAndForeignProject` and `TestCurrentNodeRecoveryRoutePerThreadReadAndForeignActorRefusal`. |
| 3 | Input validation & injection | Checked — pass | Strict single-parameter query parser (`parseNodeRecoveryReadThread`) validated via `noderecovery.ValidGraphThread` (rejects control chars, empty, >128 B, unknown/duplicate params); all SQL uses parameterized `$1..$12`; `is_direct_fanout_child_thread` restricts child threads to exact `p1:`/`m1:` + 43 URL-safe base64 chars; escaping `error_transition` refused across all 3 layers. |
| 4 | Secrets & sensitive data handling | Checked — pass | Secret scan on diff returned `0` hits; child `node_errors` entries carry only registered error codes and deterministic support refs (no secrets, inputs, or stack traces); parent `node_errors` filtered out of Code child inputs. |
| 5 | Cryptography & randomness | N/A | No new cryptographic primitives; reuses existing SHA-256 `ring::digest` activation/config digests and receipt digest verification (`sha256.Sum256`). |
| 6 | Transport & egress safety | N/A | No new outbound HTTP/network clients or egress paths introduced (`http_action` untouched). |
| 7 | Concurrency, fencing & TOCTOU | Checked — pass | `PostgresCheckpointer::open` checks `state_writer_lease.ensure_current()` and activates child journals under `run_root_thread_id`; Main locks execution + visit rows `FOR UPDATE` inside Serializable/ReadCommitted transactions and enforces `(execution_id, generation, graph_thread)` partial uniqueness in PostgreSQL; Go `-race` tests pass across all touched packages. |
| 8 | Failure handling & fail-closed behavior | Checked — pass | `UnknownExternalEffect` never retries or swallows via `on_error`; missing fenced writer for Code child `settings:` fails compile-time (`Unsupported`); mismatched `receipt.GraphThread != selector.GraphThread` fails closed (`ErrNotAllowed`). |
| 9 | Resource bounds & DoS resistance | Checked — pass | `node_errors` capped at 64 per child thread; retry `max_tries` (`1..=5`), `wait_between_tries_ms` (`100..=5000`), and `timeout_ms` clamped to `min(execution_deadline, fanout_deadline)`; `POST /actions` capped by `http.MaxBytesReader(..., 8192)`. |
| 10 | Wire & schema compatibility | Checked — pass | No new JSON fields added to `storage.NodeRecoveryControl` (preserves Rust `#[serde(deny_unknown_fields)]`); `"fanout_owned"` refusal key retained in contract JSON; migration `0271` is online-compatible with Old Main. |
| 11 | Dependency & supply-chain hygiene | Checked — pass | Zero dependencies added or upgraded; `cargo deny --all-features check advisories` (`advisories ok`) and `govulncheck ./...` (`0` called vulnerabilities) pass. |
| 12 | Audit logging & observability | Checked — pass | Every operator authorization and Worker ack writes to `elitea_runtime.node_recovery_audit` with `actor_id`, `activation_id`, `journal_revision`, and `claim_id`; no user state values logged. |
