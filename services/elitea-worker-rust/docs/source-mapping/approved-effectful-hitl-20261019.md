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
| Model call after any decision (FU-236) | It works. | The real provider refused the resumed request with 403, shown as "The model service denied access". | The resumed request has the shape of a fresh turn and is accepted. |
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

## Mechanism

| Concern | Code | Notes |
|---|---|---|
| Approved call runs once (gate deleted) | `src/agents/direct_hitl.rs:1309` `into_direct_replay`, `:1335` | An approved call is refused only when an earlier continuation dispatched it without a recorded outcome and it may not re-run (`sensitive_tools.rs:102` `reruns_after_dispatch`: read-only tools, and the workspace command whose own run ledger serves its earlier result, ADR-0034 decision 9). |
| Root dispatch marker | `src/agents/direct_hitl.rs:2423` `persisted_replay_state_with_confirmation`, `:2491` | The Runner persists the replayed call through the fenced session append before ADK runs the tool. A replayed call with no result means "dispatched". A recorded result keeps the existing `ContinueAfterResult` (served, not re-run). Earlier cards' approved siblings are served from their recorded results. |
| Nested dispatch journal | `src/agents/dispatch_journal.rs:73,127,144,277`; wiring `src/agents/application_tools.rs:3654`, `src/agents/model_scope.rs:125,131`, `src/agents/session.rs:710` | Before an approved nested call runs, a `dispatched` record is appended to the ROOT session through the same fenced writer; a `completed` record carries the result. The resolver reads them (`direct_hitl.rs:954`). Records carry no content and no state delta. Malformed or forged records fail closed. |
| Decision after a dead continuation stays answerable | `src/agents/dispatch_journal.rs:127` `continuation_pause`; `src/agents/graph/resume.rs:634`; `src/agents/session.rs:1991` | A pause is answerable behind a later suffix only when that suffix holds dispatch records. An ordinary advance is still stale. |
| Typed refusal | `src/protocol/output.rs:133,731,800-803`; `src/agents/direct_hitl.rs:108` `assembly_error`; `src/agents/application_tools.rs:5141`; `src/agents/graph/application.rs:448,455`; `src/agents/graph/compiler.rs:1718`; `src/execution/native_agent_lifecycle.rs:1558`; `src/agents/graph/resume.rs:1257,1340`; `src/agents/session.rs:3362` | `RuntimeFailureKind::RecoveryRefused` ↔ proto `RUNTIME_ERROR_CODE_V1_RECOVERY_REFUSED = 34` (`libs/proto/elitea/runtime/v1/errors.proto`, Go/Python regenerated). Main policy: `services/elitea-main/internal/transport/runtimegrpc/output/server.go`; `testdata/proto/runtime/v1/model_failure_policies.json`. Typed direct-HITL codes are kept as error codes instead of `invalid_resume`. |
| Settlement error code | `services/elitea-main/internal/infra/db/repos/settlements.go` `settlementErrorCode` | Main decodes the verified terminal `RuntimeErrorV1` that it already selected inside the settlement transaction and stores only the registered code name. |
| FU-236 | `src/agents/replay_history.rs:118,130` `is_confirmation_notice` | Only that exact text for a tool the history calls is removed from the provider request. Durable session events are unchanged. |

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
**TEST_COUNTS_PLACEHOLDER**. The ignored tests are the supervisor/preparation/hydration `#[ignore]` suites that CI runs
separately. `cargo fmt --check`, `cargo clippy --all-targets --all-features -D warnings` and
`RUSTDOCFLAGS=-D warnings cargo doc` are clean. Main: `go test` for `internal/infra/db/repos` and
`internal/transport/runtimegrpc/output` pass; `go vet` is clean.

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
- `replay_history.rs` `a_continuation_never_sends_the_confirmation_notice_to_the_provider`.
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

## Performance

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

BROWSER_EVIDENCE_PLACEHOLDER

## Follow-ups

- The toolkit direct-path read-only gates and nested-pipeline direct-tool nodes (inventory above).
- A nested call whose recorded result exceeds 256 KiB degrades to `RECOVERY_REFUSED` on a crash before the parent
  continues.
- The gateway does not log an upstream refusal's status/code. A fixed-reason log would have shortened FU-236.
