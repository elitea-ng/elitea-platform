# Approved sensitive calls run once, whatever their effect class (2026-10-19)

Follow-ups FU-226 and FU-236. Branch `fix/approved-effectful-hitl-runs-once`, based on `origin/main` (merged up to
`f4e6157aa`). It changes main-wide behaviour: an Agent at the conversation root, saved Agents in pipeline `agent`
nodes, and Agents used as tools.

User decision (PLAN.md §8.6): HITL approval is not limited to read-only tools. The read-only restriction in
`direct_hitl.rs` `into_direct_replay` was a temporary gate, so it is deleted together with its refusal path and its
tests. No setting replaces it.

## Business behaviour

| Topic | Current platform (reference only) | Before this change | After this change |
|---|---|---|---|
| Approve a sensitive tool that changes data | The tool runs. | Refused. At the root: "Configuration type is not supported." Inside a pipeline: "The runtime operation failed." | The tool runs exactly once and the Agent continues. |
| Block / Reject | The tool does not run and the Agent continues. | The same. | The same. A blocked call never dispatches. |
| A Worker dies after an approved call was sent | The call is replayed ("lost, run again"). | Not reachable (approval refused). | The call is never repeated. With a recorded outcome, the run continues with that outcome. Without one, the run ends with the registered `RECOVERY_REFUSED` reason ("may have started … not repeated"). |
| Model call after any decision, and on later turns (FU-236) | It works. | The upstream refused some resumed and later requests with 403, shown as "The model service denied access". | The request has the shape of a fresh turn and is accepted: no runtime bookkeeping, and stored answers sent as one string. |
| Settlement row of a failed run | n/a | `execution_settlements.error_code` was always NULL. | It holds the registered runtime error code (for example `RECOVERY_REFUSED`, `UNSUPPORTED_CAPABILITY`). |

Deliberately **not ported**: the current platform's blind replay of an approved call after a crash.

## Root causes

**FU-226.** The live checkpoint of thread `0323f414-49ae-4195-817b-7dde517fb5ad` (stack `scoped`) showed a correct
graph-call receipt. The refusal came from `into_direct_replay`, which refused every APPROVED call to a tool not
admitted as read-only. The stack's MCP `echo`/`reverse` declare no `readOnlyHint`, so ADK admits them as effectful.

On the nested path, `application_tools.rs` `direct_hitl_execution_error` then collapsed the typed code into
`elitea_nested_agent.invalid_resume`. The pipeline node turned that into a generic node failure, which surfaced as
`INTERNAL`. At the root it surfaced as `UNSUPPORTED_CAPABILITY`.

**FU-236.** On pm1084 (main images, real models), every refused request was a nested child's model call in a
continuation turn. Fresh-turn child calls and root calls in the same continuation returned 200. The gateway mapped
the model, and its governance gates logged nothing, so the 403 came from the upstream provider.

The only difference in the refused requests was ADK's synthetic model-role text "Tool confirmation required for
'<tool>'. Provide approve/deny decision to continue." The replayed history sent it to the provider on every
continuation. It is runtime bookkeeping the model never produced. The request bodies were captured in the PG
harness: the continued request carried the text before the fix and does not after it.

On scoped (with this branch's images), the root continuation then worked, but two other requests still got 403:

- a Reject continuation;
- any later turn after a decided call.

The gateway refusal log (PR #1320) showed a bare upstream 403 with no type or code: an edge in front of the shared
model endpoint. The refused bodies were captured with a local-only diagnostic gateway build (not committed) and
replayed through the stack's authenticated `/llm` route, changing one element at a time.

The edge refuses combinations:

- the fixture tool description contains a backtick-quoted word (`` `echo` ``), and
- the request also contains either the platform's single-quoted label in the block result
  (`call to '<toolkit>.<tool>'; it was not executed`) or a stored answer sent as one content block per streamed
  chunk.

Each element alone passes. Two of the three are the Worker's own encoding, not conversation content. The same pass
also found the replay marker resent on every later turn.

## Mechanism

| Concern | Code | Notes |
|---|---|---|
| Approved call runs once (gate deleted) | `src/agents/direct_hitl.rs:1309` `into_direct_replay`, `:1335` | An approved call is refused only when an earlier continuation dispatched it without a recorded outcome and it may not re-run (`sensitive_tools.rs:102` `reruns_after_dispatch`: read-only tools, and the workspace command whose own run ledger serves its earlier result, ADR-0034 decision 9). |
| Root dispatch marker | `src/agents/direct_hitl.rs:2446` `persisted_replay_state_with_confirmation`, `:2514` | The Runner persists the replayed call through the fenced session append before ADK runs the tool. A replayed call with no result means "dispatched". A recorded result keeps the existing `ContinueAfterResult` (served, not re-run). Earlier cards' approved siblings are served from their recorded results. |
| Nested dispatch journal | `src/agents/dispatch_journal.rs:73,127,144,277`; wiring `src/agents/application_tools.rs:3654`, `src/agents/model_scope.rs:125,131`, `src/agents/session.rs:710` | Before an approved nested call runs, a `dispatched` record is appended to the ROOT session through the same fenced writer; a `completed` record carries the result. The resolver reads them (`direct_hitl.rs:954`). Records carry no content and no state delta. Malformed or forged records fail closed. |
| Decision after a dead continuation stays answerable | `src/agents/dispatch_journal.rs:127` `continuation_pause`; `src/agents/graph/resume.rs:634`; `src/agents/session.rs:1991` | A pause is answerable behind a later suffix only when that suffix holds dispatch records. An ordinary advance is still stale. |
| Typed refusal | `src/protocol/output.rs:133,731,800-803`; `src/agents/direct_hitl.rs:108` `assembly_error`; `src/agents/application_tools.rs:5141`; `src/agents/graph/application.rs:448,455`; `src/agents/graph/compiler.rs:1718`; `src/execution/native_agent_lifecycle.rs:1558`; `src/agents/graph/resume.rs:1257,1340`; `src/agents/session.rs:3362` | `RuntimeFailureKind::RecoveryRefused` ↔ proto `RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED = 34` (`libs/proto/elitea/runtime/v1/errors.proto`, Go/Python regenerated). Main policy: `services/elitea-main/internal/transport/runtimegrpc/output/server.go`; `testdata/proto/runtime/v1/model_failure_policies.json`. Typed direct-HITL codes are kept as error codes instead of `invalid_resume`. |
| Settlement error code | `services/elitea-main/internal/infra/db/repos/settlements.go` `settlementErrorCode` | Main decodes the verified terminal `RuntimeErrorV1` that it already selected inside the settlement transaction and stores only the registered code name. |
| FU-236: ADK confirmation notice | `src/agents/replay_history.rs:118,134` `is_confirmation_notice` | Only that exact text for a tool the history calls is removed from the provider request. Durable session events are unchanged. |
| FU-236: answered replay marker | `src/agents/replay_history.rs:119,153` `drop_answered_replay_markers`; `src/agents/direct_hitl.rs:2319` `is_replay_marker` | A later turn drops the runtime's replay marker once the replayed call follows it. That matches what the continuation already sent. The block result keeps any reviewer comment (`denial_reason`). A marker no call follows yet stays. Only text that parses exactly as a marker is dropped; a look-alike stays. |
| FU-236: one string per stored answer | `src/transport/openai_compatible_facade.rs:1054,1092` `openai_assistant_message` | A text-only assistant message is sent as one string instead of one block per streamed chunk. This also saves ~30 bytes per chunk on every later turn. |
| FU-236: block result wording | `src/agents/direct_hitl.rs:2638` `blocked_tool_result` | The action label is no longer single-quoted. The wording is otherwise unchanged. |

## Read-only gate inventory

| Gate | Path | Decision |
|---|---|---|
| Approved direct-HITL replay refused unless read-only (`direct_hitl.rs` `into_direct_replay`, decided call and approved siblings) | Root and nested Agents | **Removed** in this PR, replaced by the dispatch rules above. |
| Direct toolkit kernel refuses non-read-only tools (`libs/rust/agent-runtime/src/toolkits/direct_execution.rs:262`, mapped to `EffectRefused` in `execution/toolkit_delivery_processor.rs:1831`) | Toolkit "Test Tool" (`toolkit.call_tool.v1`) and `toolkit.execute.read.v1` jobs | **Follow-up.** These jobs have no HITL decision and no session to journal into. An effect there needs a command-level effect receipt (G-WORKER-10). |
| `ReadOnlyClaimPlatformWriter` refuses artifact/skill/context writes (`transport/platform_writer.rs:197-258`) | Same toolkit direct path | **Follow-up**, same reason. |
| Toolkit direct-path host lending (`toolkit_delivery_processor.rs:121-135`, SharePoint upload, imagegen) | Same toolkit direct path | **Follow-up**, same reason. |
| Effectful pipeline direct-tool node without a node-recovery authority (`agents/graph/direct_tool.rs:432-436`) | Nested saved child pipelines (`agents/pipeline.rs` binds no authority there) | **Follow-up.** A root pipeline is already protected by the node attempt journal (PR #1159). Saved child pipelines need that authority wired through, which is a node-recovery change rather than HITL. |
| Scoped Code node needs its fenced writer (`agents/graph/compiler.rs:1637-1640`) | Code nodes | Not a read-only gate; unchanged. |

## Tests

Worker: `cargo test --offline --locked --no-fail-fast --all-targets --all-features` against a disposable `postgres:18`
(`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`):
**2524 passed, 0 failed, 103 ignored** after the second review round (2514 / 0 / 103 after merging `origin/main`; 2507 / 0 / 93 before), with no PostgreSQL skips. The ignored tests are the supervisor/preparation/hydration `#[ignore]` suites that CI runs
separately. `cargo fmt --check`, `cargo clippy --all-targets --all-features -D warnings` and
`RUSTDOCFLAGS=-D warnings cargo doc` are clean. `libs/rust` workspace: 2739 passed, 0 failed, 1 ignored.
Main: `go test ./...` passes (215 packages). `claims_test.go` `TestExpiredClaimRecoversTerminalOutputAcrossProducerReplacement`
now scripts the verify query's three columns. `go vet` is clean. Counts are after merging `origin/main` at `ad4988406`.

Audits:

- `cargo deny --all-features check advisories` reports 11 findings (8 vulnerabilities, 3 unmaintained crates). All are
  pre-existing: this branch changes no `Cargo.lock`, `Cargo.toml`, `go.mod` or `go.sum`.
- `govulncheck ./...` on Main: no reachable vulnerabilities.

Real PostgreSQL. Each phase runs in its own OS process and claim, and the effectful fixture tool counts its calls:

| Scenario | Test | Asserts |
|---|---|---|
| Pipeline `agent` node: approve, then repeat | `pipeline_effect_pg_tests.rs` `postgres_approved_effectful_call_in_an_agent_node_runs_once_and_a_repeat_runs_nothing` | 1 tool call, 1 model call, answer; the repeat makes 0 tool and 0 model calls. The continued model request has the fresh-turn shape (FU-236): no confirmation text, `tool_calls` intact. |
| Block | `postgres_blocked_effectful_call_in_an_agent_node_never_runs` | 0 tool calls; the Agent continues with the blocked result. |
| Foreign decision | `postgres_decision_for_another_interrupt_never_runs_the_call` | Refused, 0 tool calls. |
| Process replaced before dispatch | `postgres_worker_replaced_before_dispatch_runs_the_approved_call_once` | The approving Worker dies while binding the tool; the replacement runs the call once. |
| Process replaced after dispatch | `postgres_worker_replaced_after_dispatch_refuses_as_may_have_started` | The Worker dies inside the tool; the replacement ends with `RecoveryRefused`, 0 tool calls, 0 model calls. |
| Process replaced after the recorded outcome | `postgres_worker_replaced_after_the_recorded_outcome_resumes_with_it` | The tool ran and its outcome was recorded, then the Worker died at the next model request; the replacement continues with the recorded outcome, 0 tool calls, and the model sees the stored result (class R). |
| Root Agent: approve, then repeat | `ordinary_effect_pg_tests.rs` `postgres_root_approved_effectful_call_runs_once_and_a_repeat_runs_nothing` | 1 tool call; the repeat makes 0. |
| Root: a later user turn after an approval | `postgres_root_later_turn_after_an_approval_sends_one_call_and_result` | The third process sends a new user turn. The provider request is `system, user, assistant(call), tool, assistant, user`: the earlier answer as one string, no replay marker, no confirmation notice. 0 tool calls. |
| Root: block / foreign decision | `postgres_root_blocked_effectful_call_never_runs`, `postgres_root_decision_for_another_interrupt_is_refused` | 0 tool calls. |
| Root: process replaced after dispatch | `postgres_root_worker_replaced_after_dispatch_refuses_as_may_have_started` | Refused at assembly with the recovery-refused cause, 0 tool calls. |
| Root: recorded outcome, two Workers replaced in a row | `postgres_root_recorded_outcome_survives_two_replaced_workers_without_a_rerun` | The tool ran and its result was persisted; the next two Workers die at the model request, and the second one's tool would exit if it were ever called (it is not); the third continues with 0 tool calls. |
| Pipeline: block after a dispatched approval | `postgres_block_after_a_dispatched_approval_is_refused_not_reported_as_blocked` | A block of a call that may have run is refused, never reported as "declined, not executed". |

Unit tests:
- `direct_hitl_tests.rs` `an_approved_effectful_call_runs_once_and_is_refused_after_its_dispatch` (never dispatched,
  marker-only, dispatched; a read-only call re-runs).
- `session_tests.rs` `an_approved_effectful_sensitive_call_runs_once_at_the_root`.
- `dispatch_journal.rs` record classification, forged and malformed records, and `continuation_pause`.
- `native_agent_lifecycle.rs` taxonomy tests.
- `replay_history.rs` `a_continuation_never_sends_the_confirmation_notice_to_the_provider` and
  `a_later_turn_drops_the_answered_replay_marker_and_keeps_user_text`. A typed look-alike stays, and a marker no call
  follows yet stays.
- `openai_compatible_facade_tests.rs` `a_streamed_answer_in_history_reaches_the_provider_as_one_string`.
  `ordinary_tests.rs` `application_and_adhoc_share_authorized_redemption_model_session_and_projection` now pins the
  one-string answer; it previously pinned one block per chunk.
- Main `settlements_test.go` `TestSettlementErrorCodeNamesOnlyARegisteredRuntimeFailureCode`.

The approve PG test was red before the fix with `agent.legacy` → `Internal`, which is the live symptom.

### Review findings fixed before the PR

An opus review of the first version found four defects. Each one is fixed and has a test:

1. A later continuation's replay marker reset the root's dispatch and result facts, so a second crash after a
   recorded outcome would have re-run the effect. The facts are now monotonic across markers
   (`persisted_replay_state_with_confirmation`).
2. A root sibling dispatched without a result was not refused when the decided call's result existed. The replayed
   message now marks every call it carries as dispatched.
3. A nested Block after a dispatched approval was accepted. It is now refused as stale.
4. Non-sensitive siblings of the replayed message were neither journaled nor served. Every call of the message is now
   journaled, and its recorded outcome is served.

### Second review round (coordinator, head `0a24f4d28`)

| # | Finding | Change | Proof |
|---|---|---|---|
| 1 | A root Deny continuation that stopped between the replayed call event and its local blocked result was refused as stale forever. | A replayed call that follows this decision's exact Deny marker is not a dispatch (`direct_hitl.rs` `persisted_replay_state_with_confirmation`): the blocked adapter has no effect, so the same Deny resumes and the blocked result is served again. | Real PG, process replacement: `postgres_root_block_interrupted_before_its_result_resumes_and_runs_nothing`. The Worker exits inside the blocked adapter (test-only `EXIT_IN_BLOCKED_REPLAY`); the replacement completes, 0 tool calls, and the model sees the blocked result. The test is red without the fix. |
| 2 | Under an ordinary root Agent, a nested "may have started" could reach the parent model as a tool error. | Investigation: at the ordinary root, a crash after the dispatch always leaves the card consumed by the dead continuation's replay batch, so the replacement was already refused at assembly before any nested or parent call, but as an untyped `InvalidConfiguration`. That refusal is now typed: when a replay batch consumed the cards and the root journal holds dispatch records for those decisions, assembly ends with `RECOVERY_REFUSED` (`application_tools.rs` `answered_by_a_dispatching_continuation`). | `ordinary_tests.rs` `a_nested_approved_call_that_may_have_started_ends_the_root_run_with_its_reason`: the approving Worker stops inside the approved nested tool; the replacement is refused at assembly with the recovery-refused cause; 0 repeated tool calls, 0 further model calls. |
| 3 | The `ScopedAgent` path dropped the refusal (`_ => failure()`). | `pipeline/scoped_runtime.rs` `run_child_draining` maps `agent.recovery_refused` to the registered reason, as the unscoped node does. | The scoped path is admitted by 2a-1, whose scoped real-PG proofs assert it. |
| 4 | Fan-out branches built their `ApplicationNode` without failure events. | Wired: `parallel_compiler.rs` and `map_compiler.rs` pass the branch/item event sender to `with_failure_events`. | Not separately proven here; Wave 2a-2 owns fan-out HITL journeys. |
| 5 | For nested decisions, `self.dispatched ||` refused siblings whose journal shows they never started. | Siblings are judged only by `dispatched_siblings` (at the root that set already holds every result-less sibling of a dispatched message). | `direct_hitl_tests.rs` `a_journaled_dispatch_of_the_decided_call_does_not_refuse_an_unstarted_sibling`. |
| 6 | A dispatched non-sensitive read-only sibling was refused (`reruns_after_dispatch` knew only sensitive tools). | The catalog records every enumerated tool's read-only flag (`sensitive_tools.rs` `other_tools`). A name counts as read-only only when it maps to exactly one tool. | `a_dispatched_non_sensitive_sibling_reruns_only_when_read_only` (read-only re-runs; data-changing or unknown is refused). |
| 7 | A failed inner tool skipped the `completed` record. | The failure is recorded as the `{"error": ...}` result the model is shown, so recovery serves it. | `dispatch_journal.rs` `a_failed_call_records_the_error_the_model_saw`. |
| 8 | No limit tests. | Added at limit and limit+1. | `a_record_at_the_byte_limit_reads_and_one_byte_more_fails_closed`, `a_record_at_the_byte_limit_writes_and_one_byte_more_is_refused`, `a_decision_reads_its_record_limit_and_refuses_one_more`. |
| 9 | Two extra fenced root writes plus a result copy per journaled call. | Only calls that may not re-run are journaled (`DirectHitlReplay::journaled_call_ids`). A read-only call re-runs (class I) and costs no writes. **The full result is kept, not a digest or reference:** when the `completed` record is written, the nested function response is not yet durable in the root session (child events reach it through asynchronous forwarding). A reference would turn the after-outcome window from R into C. | Budget: `a_journaled_call_costs_two_appends_and_any_other_call_none` (exactly 2 appends per journaled call, 0 otherwise); `only_calls_that_may_not_rerun_are_journaled`. |
| 10 | Cleanup. | The duplicated string branch in `openai_assistant_message` is folded; the redundant block in `graph/application.rs` is removed. | Existing facade tests. |


- One extra fenced append before, and one after, each call a nested replay executes. These are the only new writes; there is
  no write per chunk or per token.
- Records are bounded at 256 KiB each (`MAX_DISPATCH_RECORD_BYTES`) and at 2 × 64 per decision
  (`MAX_DISPATCH_RECORDS`).
- A larger result is still returned to the model, but it is not stored. A crash before the parent continues then
  ends as `RECOVERY_REFUSED`.
- The root path adds no write: the replayed call event already existed.

## Durability (crash windows)

| Window | Rule | Proof |
|---|---|---|
| Approved, before dispatch | No record exists, so the call runs once. | `…before_dispatch…` |
| After the durable dispatch record, before the outcome | The outcome is unknown. The call is never repeated and the run ends `RECOVERY_REFUSED`; an operator reconciles (C). | `…after_dispatch…` (pipeline and root) |
| After the recorded outcome, before the parent continues | The recorded outcome is served (R). | `…after_the_recorded_outcome…` |
| The `completed` append fails after the effect | The model still gets the result. Only a crash before the parent continues degrades to `RECOVERY_REFUSED`. | By construction (`dispatch_journal.rs:277`) |

## Resilience

- Every failure is typed: `RECOVERY_REFUSED` (not retryable), and typed direct-HITL codes instead of generic ones.
- Bounded records; malformed records fail closed (`CorruptSession`).
- The writer fencing is unchanged: records use the claim-fenced root session writer.

## Security

| Category | Applies | Check |
|---|---|---|
| Identity / trust | No new identity source | Records are written only through the claim-fenced root writer. A record with a forged author, content or state delta is refused. |
| Object authorization | Yes | A decision binds to the exact persisted call (invocation, call id, arguments digest). A foreign interrupt is refused (root and pipeline PG tests). |
| Input bounds | Yes | Record size and count limits; strict `deny_unknown_fields` record schema. |
| Injection | No string-built SQL | Main reads its typed code from proto; the existing parameterized insert gains one parameter. |
| Egress | No change | — |
| Secrets / logs | Yes | Records hold a tool result, which the session already stores in the call's function response. Nothing new is logged except a fixed-reason warning. Main stores only the registered code name, never the message. |
| Effects | Yes | No external effect is repeated without a receipt; block never dispatches; an unknown outcome is refused, not retried. |
| Supply chain | No new dependency | — |

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × tool call (effectful, approved), crash before dispatch | R | `dispatch_journal.rs`, `direct_hitl.rs:1335` | `…before_dispatch…` |
| Worker × tool call (effectful, approved), crash after dispatch | C (typed refusal, never repeated) | `direct_hitl.rs:1335`, `protocol/output.rs:800` | `…after_dispatch…` (pipeline and root) |
| Worker × tool call (effectful, approved), crash after the recorded outcome | R | `direct_hitl.rs` served result, `dispatch_journal.rs` | `…after_the_recorded_outcome…` |
| Worker × HITL decision (P07) | R / C (was F for an approved effect) | as above | as above; `recovery-guarantees.md` row updated |

## Browser evidence

Real Chrome (Claude in Chrome), stack `scoped`, signed in through the stack's mock OIDC as `admin@centry.user`.
The models are real: `gpt-5.4-mini` through the gateway to the shared model endpoint.

Images, with the §3b binary check run on the extracted binary:

- Worker `ghcr.io/elitea-ng/elitea-worker-rust:fu226-f28af19be` (`sha256:7e75a0f21f30…`). It contains
  `. It was not executed. The block is for THIS`, `drop_answered_replay_markers` and `elitea.direct_hitl.dispatch.v1`,
  and not `'; it was not executed`.
- Main `ghcr.io/elitea-ng/elitea-main:fu226-47879f8b4`. Main is unchanged since `47879f8b4`; its binary contains
  `RECOVERY_REFUSED`.
- Gateway `gwreason-d5b3e88dd` (PR #1320).

`stack.sh verify-profile --name scoped`: 49 passed, 1 failed. The failure is the shared migration head (ledger 169,
worktree 166). It is expected: the stack's compose worktree is the 2a-1 branch at `dd0bbc170`, and no migration
changes here.

Tool runs are counted from the MCP mock's `jsonrpc tools/call` lines. Each case was reloaded, and the answer
persisted.

| Case | Chat | Executions (settlement) | Tool calls | Result |
|---|---|---|---|---|
| Root Agent 157, Approve (image `47879f8b4`) | 903 | `1af4a49ece25…` pause, `691cef5deaf6…` continuation (both SUCCEEDED) | 1 → 2 | "reverse returned ESREVER-1C" |
| Root, later turn after the approval | 903 | `61235c1a95bc…` (SUCCEEDED, gateway 200) | 2 → 2 | "It returned ESREVER-1C." Before `f28af19be`, the same turn was `d9d2521fcacc…`: FAILED, `error_code = MODEL_ACCESS_DENIED`, upstream 403. |
| Root Agent 157, Reject | 907 | `7a60048f83dc…`, `4c0770ad830b…` (SUCCEEDED, gateway 200) | 2 → 2 | "The call was declined." Before `f28af19be`, the same flow was chat 906: upstream 403. |
| Root Agent 157, Approve (image `f28af19be`) | 910 | `2773a11653c7…`, `c2e1db551f0a…` (SUCCEEDED) | 3 → 4 | "reverse returned ESREVER-1C." |
| Pipeline 158, `agent` node running Agent 157, Approve | 908 | `432e019f4c3e…`, `49858418d319…` (SUCCEEDED; the nested child's continuation model call returned 200) | 2 → 3 | "reverse returned ESREVER-1C." |
| Pipeline 158, later turn | 908 | `fba0e3ed6997…` (SUCCEEDED, gateway 200) | 3 → 3 | answered without a tool |
| Pipeline 158, Reject | 909 | `167a03a975ec…`, `a4179dbc0951…` (SUCCEEDED, gateway 200) | 3 → 3 | "The call was declined." |

The gateway logged no `llm_provider_refusal` line during the runs on `f28af19be`.

Fixture state, set through the database (not the UI) and kept from earlier sessions:

- Agent 157 version 183 `llm_settings`: model `gpt-5.4-mini`, `model_project_id 1`.
- Configuration 18 `api_base`: `http://llm-mock:8090`.
- Agent 171 version 197: `agent_type` changed from `openai` to `agent`.
- Platform sensitive tools: `{"mcp": ["reverse"]}`.
- The coordinator copied the `centry.secrets_data` row so the real model can resolve its credential.

Chats 904 and 905 were diagnostic chats for the 403.

## Follow-ups

- The toolkit direct-path read-only gates and nested-pipeline direct-tool nodes (inventory above).
- A nested call whose recorded result exceeds 256 KiB degrades to `RECOVERY_REFUSED` on a crash before the parent
  continues.
- Gateway refusal log: PR #1320.
- Ordinary root Agent, nested approved call, crash after the recorded outcome: the replacement is refused with
  `RECOVERY_REFUSED` (class C) instead of serving the recorded result (R). The root's nested-resume derivation does
  not yet accept a dead continuation's suffix; pipeline `agent` nodes do. Fixing it means rebuilding the nested
  resume from the journal.
- Fan-out branch refusals (review item 4) are wired but proven only with 2a-2's fan-out HITL journeys.
- The upstream edge in front of the shared model endpoint refuses benign content combinations (for example backticks
  in a tool description, together with quoted text). That is an environment issue for the endpoint owner. User
  content can still trip it.
