# Graph fan-out child lineage, control stops and no polling (2026-10-08)

Track C1 of Point 5 Wave 1. Branch `fix/graph-fanout-child-lineage`, base `origin/main` (`604a49e1`).
It applies to the existing Parallel (`graph.parallel`) and Map (`graph.map`) runners. Both admission gates stay
`false` (`FIXED_PARALLEL_INTEGRATION_READY`, `MAP_INTEGRATION_READY`), so production behaviour changes only once
the gates flip. This fix is a mandatory gate-flip criterion.

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| Parallel sub-agent children across a HITL resume | The legacy async supervisor parks the parent, runs children as separate durable tasks, then reconciles and replays. It loses concurrent pauses, multi-round replay does not converge, and the UI shows phantom instances. | Children keep one identity for the life of the occurrence. A continuation restores completed children from their receipts and resumes a paused child from its own checkpoint. |
| Lease loss of a child | Not modelled. A lost task re-runs from scratch. | A typed control stop with no failure record. A later claim restores the same children. |
| Stop and deadline | Polled. | Event-driven, bounded by the deadline and a cleanup bound. |

Deliberately **not ported**: the park/reconcile/replay supervisor and the complete-set decision model. The existing
complete-set resume code (`parallel_published_resume.rs`) is left untouched and unwired, and Wave 2 replaces it.
`ParallelPublishedContinuation` is not wired.

## Defects fixed

1. **Lineage loss.** Child thread ids hashed the current claim's `execution_id` and `generation`. Every HITL
   continuation is a new execution with generation 1 (Main `agentexecution/continue.go`,
   `execution/submit_job.go`). After any resume, completed children ran again and paused children lost their
   checkpoint.
2. **Lease loss reported as a business failure.** `writer_not_current` became
   `ParallelBranchOutcome::Failed` and then `graph.parallel.branch_failed`. In Map it became a durably recorded
   `MapStop::Failed`. The lifecycle wrappers also wrote a durable `Failed` receipt for a cancelled child.
3. **Busy polling.** Parallel woke every 10 ms. Map's item cancel watcher polled every 50 ms.

## Changed paths (Worker, `services/elitea-worker-rust/src/`)

| Path | Change |
|---|---|
| `agents/graph/parallel.rs:93` | `ParallelChildOrigin` {execution_id, generation} with `validate()`. Factory trait gains `child_origin`, `branch_thread_id`, and `for_branch(.., origin)`. |
| `agents/graph/parallel.rs:457` | `mint_lineage` proposes origin and threads. Restore requires the frozen thread (`:572`). |
| `agents/graph/parallel.rs:924-1066` | `drain_branches` uses `select!` over the latch, deadline, cleanup deadline and children. Adds `ParallelBranchOutcome::LeaseLost` (`:438`, `:1008`) and `begin_stop` (`:1053`). |
| `agents/graph/parallel_checkpoint.rs:18-19,285,295` | Occurrence format v2, legacy v1 refusal, lineage validation. `freeze` keeps the existing lineage (`:86`). The lifecycle wrapper records nothing on lease loss or cancel (`:610`). |
| `agents/graph/map_reduce.rs:394,452` | Origin and threads minted before freeze; restore requires the frozen thread. |
| `agents/graph/map_reduce.rs:362,481,505,586,679` | `settle_stop` (cancel recorded nothing), sibling-stop latch on lease loss, latch `select!` in items. `wait_for_cancellation` deleted. |
| `agents/graph/map_reduce_checkpoint.rs:17,164,171,521` | Map occurrence v2, legacy refusal, lineage validation, lifecycle control stop. |
| `agents/graph/fanout_control.rs:22,46,72` | `FanoutCancellation` latch (lost-wakeup safe) and `is_lease_lost`. |
| `state/postgres_checkpointer/parallel_children.rs:21,37,116,160` | Origin-based derivation. Hash and domain unchanged; generation hashed as i64 big-endian. |
| `state/postgres_checkpointer/map_children.rs:34,70,114,140` | Same for Map, with one shared `check_map_item` validation. |
| `state/postgres_checkpointer/application_children.rs`, `agents/pipeline/scope_receipts_{parallel,map}.rs`, `agents/graph/map_turn.rs` | Delegation only. |
| `agents/graph/mod.rs` | `fanout_control` module; test-only re-exports for the PostgreSQL proofs. |

## Performance

| Rule / budget | Mechanism | Proving test | Measured |
|---|---|---|---|
| No busy polling: zero timer wakes while children run | `parallel.rs:973-986`, `map_reduce.rs:671-684`. The only timers are the absolute deadline and the cleanup bound. The 10 ms `sleep` and 50 ms `wait_for_cancellation` are deleted. | `latch_cancel_stops_inflight_branches_within_the_cleanup_bound_and_admits_nothing_new`, `latch_cancel_stops_pending_items_promptly_without_recording_a_stop` (4 worker threads, channels, no sleeps) | — |
| Cancel latency ≤ 100 ms | Latch `select!` arm | Same tests, each asserting `elapsed ≤ 100 ms` | Parallel: 21.8 / 22.2 / 24.2 ms (20 ms cleanup bound for non-cooperative children). Map: 0.18 / 0.20 / 5.4 ms. |
| Deadline honoured | `sleep_until(deadline)` arm | `deadline_stops_non_cooperative_branches_at_the_deadline_plus_cleanup` (≥ 50 ms, ≤ 50+20+100 ms), `deadline_stops_pending_items_at_the_deadline_and_is_still_recorded` | Parallel: 74.0 / 75.1 / 97.7 ms (50 ms deadline + 20 ms cleanup). Map: 52.2 / 52.8 / 61.3 ms. |
| Freeze cost | 1 `child_origin` read plus N SHA-256 derivations (N ≤ 16 branches / 64 items) per `prepare`. No extra database round trip: the lineage rides inside the existing single occurrence append. | Covered by the lineage tests. Not benchmarked separately (pure hashing). | — |
| Parent writes per activation | Unchanged: 1 freeze append, plus the existing pause or decision appends. | Existing parallel and map tests | — |

## Durability

| Rule | Mechanism | Proving test (real PostgreSQL unless noted) |
|---|---|---|
| Child identity is frozen once per occurrence and reused by every continuation, reclaim and generation bump | `parallel_checkpoint.rs:86-93`, `parallel.rs:572`, `map_reduce_checkpoint.rs:54`, `map_reduce.rs:452` | `parallel_children_survive_a_new_execution_and_resume_from_their_own_checkpoint`: E1 freezes 4 branches (2 completed, 1 paused, 1 never admitted), E2 restores via a later claim, E3 resumes. 0 re-runs of the completed branches; `prep` of the paused branch ran exactly once. `map_items_completed_before_a_takeover_by_a_new_execution_do_not_run_again`: calls `[0,1,2,2]`. **Both failed on main** (`2 != 1`; `[0,1,2,0,1,2]`). |
| Regenerate or rewind gets fresh children | A new occurrence (new step or state) mints from the current claim | `loop_visits_get_distinct_child_checkpoint_threads` (existing, in memory) |
| Writer fencing on every child write; a stale claim cannot overwrite | The unchanged takeover rule `postgres_checkpointer.rs:367-368`; activation under the current claim `parallel_children.rs:104` | `a_superseded_claim_cannot_overwrite_the_later_claims_completed_children` (C1e two-claim race): claim A blocks mid-branch, claim B restores and completes, A then gets `checkpoint.writer_not_current` (not `branch_failed`). The latest child checkpoint is B's, and there are 0 failed receipts. |
| Old format refused by type, never silently re-derived | `parallel_checkpoint.rs:285`, `map_reduce_checkpoint.rs:164` | `a_legacy_occurrence_without_frozen_child_identity_is_refused_by_type`, `a_legacy_map_occurrence_without_frozen_child_identity_is_refused_by_type` (in memory): no child is admitted |

### Recovery guarantee (replatform delivery gate §2): component × fan-out child phase

| Component down during | Before | After | Enforcing code | Proof |
|---|---|---|---|---|
| Worker, after some children completed (process loss) | L: completed children ran again under the new execution | **R**: completed children replay from receipts | `parallel.rs:572`, `map_reduce.rs:452` | Parallel and Map lineage PG tests (crash = node future dropped with a child in flight) |
| Worker, while a child is paused for HITL, then a continuation | L: paused child checkpoint orphaned | **R**: the paused child resumes from its own checkpoint (`prep` not re-run) | Same | Parallel lineage PG test (E3) |
| Worker lease lost mid-child (takeover or lease expiry) | F, wrongly: durable `branch_failed` / `MapStop::Failed` that a replacement could not undo | **R**: control stop, nothing recorded; the replacement claim restores and completes | `parallel.rs:438,1008,1039`, `parallel_checkpoint.rs:610`, `map_reduce.rs:505,586`, `map_reduce_checkpoint.rs:521` | `parallel_lease_loss_mid_branch_is_a_control_stop_a_later_claim_completes`, `map_lease_loss_mid_item_records_no_stop_and_a_later_claim_completes`, two-claim race |
| User Stop / cancel | Parallel: no record. Map: durable `MapStop::Cancelled`. | No record in either runner (cancel ends the turn; nothing is resumed from it) | `map_reduce.rs:362` | `latch_cancel_stops_pending_items_promptly_without_recording_a_stop`, `a_branch_failing_after_cancellation_writes_no_failed_receipt` |
| Effectful tool inside a child, crash between dispatch and the child's next checkpoint | I/C, unchanged (Gate 6 effect receipts) | Unchanged; fan-out does not widen it | — | Out of scope (Gate 6) |
| Main / NATS / PostgreSQL / sandbox supervisor | Unchanged by this PR | Unchanged | — | — |

## Resilience

| Rule | Mechanism | Proving test |
|---|---|---|
| Lease loss is a typed control stop, never a business failure | `fanout_control.rs:72`, `parallel.rs:438,1008,1039`, `map_reduce.rs:586`, lifecycle wrappers `parallel_checkpoint.rs:610`, `map_reduce_checkpoint.rs:521` | `a_lease_lost_branch_is_a_control_stop_not_a_recorded_failure`, `a_lease_lost_item_is_a_control_stop_and_records_nothing`, `only_writer_not_current_is_classified_as_lease_loss`, and the PG lease-revoke tests (`TestStateWriterLease::revoke` mid-branch and mid-item) |
| Lease loss stops admitted siblings | Parallel `cancel_signal` via `begin_stop` `parallel.rs:1053`; Map sibling latch `map_reduce.rs:481,505,679` | `a_lease_lost_item_stops_its_admitted_siblings` (red without the fix: 5 s timeout) |
| No admission after a stop | `begin_stop`, `admission_open && !stopping` | Cancel-latency tests assert the third branch / item never entered |
| Bounded cleanup of non-cooperative children | `cleanup_deadline` arm `parallel.rs:984` | Cancel and deadline tests assert `DropObserver` counts (futures dropped) |
| Bounded lineage data | `validate_lineage` / `valid_lineage`: thread ≤ 512 B, no control characters, distinct, not the root, count = branches/items; origin execution id ≤ 256 B, generation ≥ 1 | Legacy and lineage tests; `occurrence_from` rejection paths |

## Security

| Threat | Mechanism | Proving test |
|---|---|---|
| Frozen identity reused across tenant, project, capability or definition | The origin carries only {execution_id, generation}. Tenant, projects, capability and definition digest always come from the restoring claim's authority (`parallel_children.rs:133-160`, `map_children.rs:114-140`). The stored thread is only compared, never used as an address (`parallel.rs:572`, `map_reduce.rs:452`). | `frozen_child_identity_cannot_be_reused_across_tenant_project_or_definition`: the same origin gives 4 distinct threads in 4 scopes, and a second definition on the same root freezes disjoint children |
| A stale claim writes after takeover | Writer fence on every write (`postgres_checkpointer.rs:367-368`). Even reads through a superseded writer fail. | Two-claim race and lease-revoke PG tests |
| Checkpoint payloads, thread ids, execution ids or checkpoint ids in logs | The fan-out code emits no log lines. Error strings are static codes. | `fanout_restore_and_lease_loss_log_no_payload_or_identity` (TRACE capture over freeze, restore and lease loss). Mutation-checked: logging a child thread id makes it fail. |
| Tampered occurrence metadata | `deny_unknown_fields` on the new types; bounds above; v1 refused | Legacy refusal tests |

**Reviews.**
- `code-review` (high): 6 findings. 2 fixed (Map siblings on lease loss; legacy refusal scoped to the own step). 1
  no change needed (string-prefix classifier, pinned by a unit test). 3 skipped with reasons:
  - Parallel and Map: the parent ADK cancel is no longer polled; see follow-up 1.
  - The duplicated origin types are unified in Wave 2.
- `security-review`: no finding with confidence ≥ 8.

**`cargo deny --all-features check advisories`** (cargo-deny 0.20.2). `Cargo.toml` and `Cargo.lock` are unchanged
from main.
- Pre-existing, identical on main: `h2` RUSTSEC-2026-0258, `rsa` RUSTSEC-2023-0071 (via `sqlx-mysql`), and yanked
  `chacha20`.
- New: none.

## Tests

- New tests: 19.
  - 7 real-PostgreSQL tests in `state/postgres_checkpointer_tests/fanout_lineage.rs`.
  - 5 Parallel and 5 Map tests (in memory, 4-thread Tokio for latency).
  - 2 `fanout_control` unit tests.
- Full suite `cargo test --locked --all-targets --all-features`, with a local disposable PostgreSQL 18 and
  `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`: lib **2038 passed, 0 failed, 63 ignored**; every other target passes.
  - The 63 ignored are pre-existing `#[ignore]` tests: sandbox and hydration deadline suites run by separate CI
    steps, NATS live, and the opt-in load tests. None of them is new.
  - One earlier run had a single failure in `reverse_completion_collects_original_order_and_preserves_structured_values`.
    That pre-existing test orders items by 8/7/6/5 ms real sleeps; it passed 5/5 in isolation and in the two
    following full runs.
- `cargo fmt --check` and `cargo clippy --locked --all-targets --all-features -D warnings`: clean.
- Fixtures: every PG test builds its own isolated database through `IsolatedPostgres`, plus claims via `writer()`
  and `writer_at()`. No UI was involved.

## Real-browser regression

Parallel and Map cannot run in a browser until their Wave 2 composition, so positive browser proof of this fix is
the gate-flip criterion in `PLAN.md` §4. This PR runs the regression set over the unchanged checkpoint paths.

**Stack.** Rust rehearsal, `http://localhost:18084` (user's choice).
- Main: `elitea-main:gate5-platform-completion-v2-20261005`.
- Web: `elitea-web:gate5-ci-ca8fee8f4-20261006`.
- Command bus: Redis Streams.
- Sign-in: the stack's local mock OIDC, as `admin@centry.user`, project "Private" (id 2).

**Worker image.** `elitea-worker-rust:c1-lineage-rehearsal-2ba248a0d-20261008` (`sha256:1fe79847ed6b…`).
- Revision `2ba248a0d`: the stack's own Worker base `d6568bae8` (on the #1084 branch) plus the five C1 commits
  cherry-picked, conflict-free.
- Why not the PR head: `main` moved the command bus to NATS and deleted the Redis transport (#1081, #1085), so a
  main-based Worker cannot attach to this Redis stack.
- Built from the repository `Containerfile` with a private BuildKit cache id. The shared `/cargo-target` cache held
  a Debian 13 `aws-lc-sys` object that fails to link on this bookworm base. This was a local, uncommitted build
  setting only.
- Deployment: the running Worker container was cloned with only the image changed (same network aliases, mounts,
  env, user, entrypoint, command and restart policy). The original container was kept as
  `elitea-rust-rehearsal-elitea-worker-1-pre-c1` and restored after the run.
- No response mocks. The model is the stack's configured `vllm/CONTINUATION-REPAIR-FIXTURE` route through its LLM
  gateway.

**Fixtures.** All are pre-existing entities created through the UI by earlier sessions; none was created or edited
from the database in this run. The database was only read to find them and to confirm settlement.

| # | Flow | Chat / execution | Result | After reload |
|---|---|---|---|---|
| 2 | Saved Agent "Full Name Resolver" (app 9) making two model-selected parallel Application calls with different input ("Elena" → Name Resolver, "Kowalski" → Surname Resolver) | chat 828, execution `e046ff38…` | Pass. The Worker log shows `tool_execution_mode="parallel_applications"`, with the two nested model requests started 5 ms apart (16:07:20.283 and .288). One combined answer; `executed_settled_acked`; Main `SUCCEEDED`, claim attempt 1. | Same single answer |
| 3 | Pipeline "Gate5 typed parent" (app 136) calling the saved child pipeline "Gate5 typed child" (app 135) with typed values | chat 829, execution `bb2e5ac2…` | Pass. Output `4\|2\|3\|for orders\|2\|True` (child count 3 → parent `+1`; 2 items; label "for orders"). `SUCCEEDED`. | Same output |
| 4 | Worker restart mid-run: a second Full Name Resolver turn ("Marco Rossi") with `docker restart` at 16:09:40.8 while 3 nested model requests were in flight | chat 828, execution `0ffaef9a…` | Pass: **R**. The run finished inside the 10 s graceful stop. After restart, the un-ACKed Redis command was redelivered every 5 s: first `retained_no_ack` (`agent_delivery.checkpoint_output_reclaim`), then 5 × `agent_output.invalid_durable_state` no-ACK. At 16:10:31 the replacement claim (attempt 2) published the terminal once and settled `SUCCEEDED` (`executed_settled_acked` → `executed_retired`), and redelivery stopped. About 50 s to settle. | One answer, with both sub-agent sections |
| 1 | Pipeline with a nested saved Agent pausing on a sensitive-tool HITL, approve → complete | — | **Not provable on this stack.** (a) The guardrail policy is `sensitive_tools = {}` (`centry.platform_config`), and the only `delete_file` toolkit (artifact) is skipped as `unsupported_toolkit_family` in this runtime. (b) No MCP server is attached to the rehearsal network. (c) Main refuses every HITL continuation here: pipeline 130 "Gate5 HITL history" paused correctly (chat 830, execution `40f5bf1c…`, card survives reload), but Approve returned `422 unsupported_agent_execution` from Main twice, before any Worker involvement. | Card still pending (unchanged) |

Flow 1 was not provable on the rehearsal stack. It is proven on the fresh stack below.

### Fresh full stack built from this branch

**Stack.** A disposable standalone stack, built from scratch from this branch at `e1c5762db` with
`deploy/scripts/standalone-stack.sh` (`certs`, `build`, `up`, `seed`, `seed-llm`) and `STANDALONE_WORKER=rust`.
- Compose project `elitea-c1-20261008`, NATS command bus, private image tags `:c1-20261008`.
- Worker `elitea-worker-rust:c1-20261008` (`sha256:360f70ede7ea…`), Main `sha256:9fe66483b71d…`, Web
  `sha256:f47688080b75…`, gateway `sha256:60b0ad4e8036…`, mock LLM `sha256:087294bac2c1…`, mock MCP
  `sha256:4d8bd51dbcd5…`.
- Browser host `http://c1.localhost:18097`. The built-in browser profile is shared with other local stacks, and
  `elitea_session` is a fixed cookie name, so `localhost` sessions were overwritten (`session_unknown`). A local
  overlay sets only `OIDC_REDIRECT_URI` to the `c1.localhost` host. Sign-in: the stack's mock OIDC, as
  `e2e-admin@autotest.local`, project "Default Project" (id 1).
- Model `vllm/E2E-MOCK-MODEL` (the `seed-llm` route through the LLM gateway to the stack's mock LLM). The
  `E2E-MOCK-MODEL` row from `seed` has no provider credential: the gateway answers `could not auto resolve a
  provider`. The project default and the agents were set to the `vllm/` row in the UI.

**Fixtures.**
- Created in the UI:
  - OpenAPI toolkit 1 `c1_mock_tools`, using the mock's own spec: `mock_tool_status` is GET (read-only),
    `mock_tool_create_item` is POST (effectful).
  - MCP toolkits 2 `c1_mcp_echo` (`/mcp`) and 3 `c1_mcp_auth` (`/mcp-auth`, which always answers 401).
    `selected_tools` was set in the Raw Json view, because Main's tool discovery is refused by its
    private-destination guard on this stack.
  - Agents: 1 "c1 sensitive agent", 5 "c1 echo child", 6 "c1 parallel parent".
  - Pipelines: 2 nested agent, 3 direct toolkit → MCP, 4 direct MCP.
- Set through the admin API with the page's own session: guardrails `sensitive_tools` `{"*": [mock_tool_status,
  mock_tool_create_item, echo, get_issues]}`. The admin "Add toolkit" button rendered no row. The guardrails
  were reset to `{}` after the run.
- Prompts use the mock LLM's `[[mock:call_tool …]]` marker.

| # | Flow | Chat / execution | Result | After reload |
|---|---|---|---|---|
| 1 | Pipeline 2 runs the saved Agent 1 as a nested node; the Agent calls sensitive `mock_tool_status` | chat 2 | **Pass.** Card "Sensitive Action Authorization Required, c1_mock_tools.mock_tool_status" under the `C1 SENSITIVE AGENT` node. Approve resumed the nested Agent and the run completed. | Card kept; the streamed nested-node section is not shown after reload (UI only) |
| 5 | Parent Agent 6 calls two child Agents in parallel; child 5 completes, child 1 pauses on the sensitive card. `docker restart` of the Worker while paused, then reload, then Approve | chat 5, pause `e5cd0651…`, resume `1917b8f1…` | **Pass: C1 lineage across a new execution and a Worker restart.** `tool_execution_mode="parallel_applications"`, pause `stream_disposition="paused"`. The resume (a new execution, claim accepted after the restart) used `direct_hitl_resume` and settled `executed_settled_acked`. The completed child's model was called **once** in total (mock journal count 1 before restart and 1 after resume), so it did not run again. One combined answer holds both child results. | Card kept across the restart |
| 6 | Agent 1 in chat: sensitive read-only tool, Approve | chat 1 | Pass. Card, reload, Approve, resume. | Card kept |
| 7 | Agent 1 in chat: effectful `mock_tool_create_item`, Reject | chat 1 | Pass. Result `sensitive_tool_blocked`, `denied_reason: denied by user`; the tool did not run. | — |
| 8 | Agent 1 in chat: auth guard on `c1_mcp_auth` through its `mcp_authorize_<id>` proxy, Skip Auth | chat 1, `d6f440a7…` | Pass. `mcp_authorization_required` pause, card "Authorization required … The protected tool has not run". Skip gives `user_declined`; nothing ran. Authorize is disabled: Main cannot read the mock's OAuth metadata through its egress guard. | Card kept |
| 9 | Pipeline 3 direct `toolkit` node on sensitive `mock_tool_status` | chat 3 | Pass for the HITL: card without a model turn, reload, Approve, resume. | Card kept |
| 10 | Pipeline 4 direct `mcp` node on `c1_mcp_auth` `echo` (sensitive and auth-guarded) | chat 4 | Sensitive card → Approve → auth card (both pass, both survive reload). **Skip Auth fails** with "The execution input is invalid". The same failure occurs without the sensitive step. See the defect below. | Cards kept |

Every approved `mock_tool_status` call reached the tool with `read_only=true` and then returned
`tool.execution.unavailable`. The cause is the environment, not the HITL path: the Worker's OpenAPI client is
HTTPS-only, and the mock LLM serves `/tool` on plain HTTP. The repository's e2e spec
(`chat.tail-sensitive-tools.spec.ts`) asserts only `tool mock_tool_status said`, which this answer meets. For the
same reason, pipeline 3 stopped after its first node ("Later nodes did not run").

**Defect found, outside this PR (pre-existing on `main`).** Skip Auth or Authorize on a pipeline's direct MCP node
always fails. Main sends `hitl_resume=true` with an `mcp_auth` decision
(`services/elitea-main/internal/application/agentexecution/continue.go`). `agents/runtime.rs:448-465` therefore
routes it to `PipelineContinuationDecision::Sensitive`, which returns `corrupt()` at `agents/graph/resume.rs:539`.
`PipelineMcpAuthorizationContinuation` accepts only a payload without `hitl_*` fields, which Main never sends. Its
unit-test fixture `mcp_resume_payload` strips those fields, so the tests did not see it. This PR touches none of
these files. A separate task tracks the fix.

Observation outside this PR's code: in flow 4 the `invalid_durable_state` no-ACK retries before the old claim
expired add about 40 s of recovery latency on the Redis transport. This path is unchanged here, and Redis is
removed on main.

## Open limits and follow-ups

1. **Wire the latch before any gate flip.** No production code calls `with_cancellation` yet.
   - Without the latch, a parent ADK Stop is seen only by children that check `is_cancelled` cooperatively, or
     when a branch settles.
   - Before this change, Parallel polled every 10 ms. Map polled every 50 ms, but only when a deadline was set.
   - Wave 2 must fire `FanoutCancellation` from the claim lease probe on a durable Stop.
2. Wave 2 unifies `ParallelChildOrigin` and `MapExecutionIdentity` in the single `FanoutRunner` lineage module.
3. `is_lease_lost` classifies by the `checkpoint.writer_not_current` code because ADK `GraphError` has no typed
   lease variant. A unit test pins the format.
4. Track C2 (observability, O(1) parent writes, batched activation, PG load test) follows this PR.
