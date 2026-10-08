# Effectful tools in pipeline direct tool nodes

Status: implemented for direct `toolkit` and `mcp` nodes behind the node-recovery journal.
Operator retry/stop for an uncertain direct-tool effect and an LLM-node receipt are open (see "Gaps").

## Decision summary

A direct tool node may now run a tool that is not annotated read-only. This covers configured
toolkit actions and MCP tools without `readOnlyHint`. The business behaviour follows the current
platform, with one rule the current platform does not enforce: **an external effect is never
dispatched twice**.

| Situation | Behaviour |
|---|---|
| Non-sensitive effectful tool | Runs once, and its output is projected like a read-only tool's. |
| Sensitive effectful tool | Pauses with approve / reject / block_with_comment. Approve runs the tool once. |
| Reject or block | No call. The whole pipeline stops with the "blocked by user" message and `_pipeline_blocked`. Declared outputs get no tool data, and no downstream node runs. |
| Auth-guarded MCP | Shows the authorize / skip card. Skip stops the whole pipeline with its message. Authorize runs the call once. |
| Tool returns an error | The pipeline stops with the typed `pipeline.tool_failed` error, as for read-only tools. The attempt is recorded as an uncertain effect and is never re-dispatched. |
| Tool ran but its result could not be journaled (lease lost, result over the 1 MiB journal cap) | Typed `pipeline.tool_failed`, saying "The tool completed, but its result could not be recorded. It will not run again." No re-dispatch. |
| Worker dies during the call | The node does not call the tool again. Run recovery refuses a direct-tool frontier, and a re-entered attempt returns the recovery card. |
| No fenced node writer (`agent_node_recovery` off) | Effectful tools are refused before any pause or call. Read-only tools run as before. |

## Current platform behaviour (reference only)

The behaviour we keep from elitea-sdk:

- The sensitive pause (`elitea_sdk/runtime/middleware/sensitive_tool_guard.py:257-341`).
- The block termination: an assistant message, `_pipeline_blocked`, declared outputs nulled, and
  the pipeline routed to END (`tools/function.py:282-335`).
- The MCP authorize/skip card (`tools/function.py:377-426`) and its skip and refresh-failed
  terminations (`:337-376`).

What we deliberately do not port:

- **Fail-open decisions.** A non-dict resume, or an unknown or missing action, counts as approve
  (`sensitive_tool_guard.py:314-333`). Rust rejects malformed decisions
  (`src/agents/graph/direct_tool.rs` `sensitive_decision`).
- **Repeated effects.** In elitea-sdk an approved tool runs again if the process dies after the call
  but before the step checkpoint.
- **MCP auth identity without the graph step.** Rust uses `pipeline:{node}:{step}`.

## Mechanism

1. **Admission** (`src/agents/pipeline.rs` `build_direct_tool_resolver`). The read-only refusal is
   removed. Policy blocklists still apply first, through `unsupported_direct_tool_scope`.
2. **Writer requirement** (`direct_tool.rs:426-431`). Without node-recovery authority, an effectful
   tool fails with a static policy error before any pause or call. The compiler attaches the
   authority to every direct node (`compiler.rs:1317-1319`).
3. **Decisions before the journal.** The sensitive pause, approve/block, and MCP authorize/skip are
   decided in `execute_mapped` and `execute_sensitive` before `dispatch`. A pause or block therefore
   writes no journal record.
4. **Journaled dispatch** (`direct_tool.rs:536-592`). For an effectful tool, `dispatch` runs one
   `DirectToolAttempt` (`:842`) under `RecoverableNode`, which appends a fenced `Started` before the
   body runs.
   - **Body** (`:867-937`). A re-entered `Started` returns `UnknownExternalEffect` without calling the
     tool (`:886`).
   - **Successful call.** The result is committed to the journal and replayed from it on re-entry.
   - **Tool error.** It is recorded as `UnknownExternalEffect`. A projection failure is recorded as
     `CompletedExternalEffect`.
   - **Handed-over outcomes** (`:574-592`). An auth challenge raised inside the call, and this visit's
     own typed tool failure, are passed back so the person sees the normal card or error. The
     journal still keeps the uncertain record.
5. **Identity.** The journal activation binds the execution, node, definition digest, graph step
   and input state digest (`node_recovery_runtime.rs` `NodeAttemptActivation`). The decision state
   differs between pause and resume, so the pause and the approved dispatch are different
   activations.
6. **Configuration.**
   - `agent_node_recovery: true` is set in `deploy/runtime/worker-runtime.json` and
     `worker-runtime.nats-secure.json` (the local/rehearsal stack).
   - Helm has a `worker.runtime.agentNodeRecovery` toggle for Rust installs. It defaults to false
     because the chart's default worker is Python and the runtime file is shared between them.

## Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission, no fenced writer | F | `direct_tool.rs:426-431` | `effectful_tool_requires_the_fenced_writer_before_any_call` |
| Worker × sensitive pause / decision | R | `execute_sensitive`, digest-bound decision | `sensitive_effectful_pause_leaves_no_started_attempt_and_approval_runs_once` |
| Worker × effectful call, result committed | R | `RecoverableNode` result replay | `effectful_direct_tool_runs_once_and_its_committed_result_is_replayed` |
| Worker × crash after Started, before result | C | `direct_tool.rs:886`. Run recovery refuses a direct-tool frontier (`compiler.rs:766`). | `started_attempt_without_result_never_repeats_an_effectful_tool` |
| Worker × tool error | F (typed) + C record | `direct_tool.rs:574-592` | `effectful_tool_failure_stops_with_its_error_and_is_not_called_again` |
| Worker × result not journaled after the call | F (typed) + C record | `direct_tool.rs` `dispatch` (`completed` flag) | `effect_whose_result_cannot_be_recorded_is_reported_and_never_repeated` |
| Worker × block / skip | F (stop with message) | `direct_tool.rs:514`, `:719` | `blocked_effectful_sensitive_tool_stops_the_whole_pipeline_under_node_recovery`, `skipped_effectful_authorization_stops_the_whole_pipeline_under_node_recovery` |
| PostgreSQL × journal append | Fenced | `StateWriterLease` in `postgres_checkpointer/node_attempts.rs` | Existing node-recovery Postgres tests (DB-gated) |

## Gaps

1. **No operator actions for direct-tool recovery cards.** Operator retry/stop and committed-result
   projection for direct nodes are not wired: `node_recovery_spec` and `node_result_projector` are
   Code-only (`compiler.rs:786-830`). A direct-tool recovery card therefore fails closed on operator
   actions. It is reached only by re-entering the same uncertain attempt. It never causes a second
   call.
2. **Auth challenge crash.** A crash while an effectful MCP call is answering an auth challenge
   leaves a `Started` record, so re-entry shows the conservative card.
3. **Repeated auth challenge with identical state.** An auth challenge raised inside an effectful
   call is recorded as a terminal no-effect failure for its activation. If a later visit reaches
   byte-identical state (for example, a second Authorize that the server challenges again, or a crash
   before the challenge's interrupt checkpoint), the node stops with a typed failure instead of a
   fresh card. This never causes a second call. The root fix is a first-class no-effect pass-through
   outcome in `RecoverableNode`, which would also remove the per-call hand-over slot. It touches
   Code-node recovery, so it is a follow-up.
4. **Limits inherited from the journal.** The activation input is the full graph state, capped at
   8 MiB (`NodeAttemptActivation::from_context`), and committed updates are capped at 1 MiB. Above
   these limits an effectful direct call is refused or reported as unrecorded.
5. **Missing writer is detected at run time.** The authority is attached after assembly
   (`src/agents/session.rs:1606`). With the flag off, upstream nodes run before the effectful node
   refuses. The committed runtime configs enable the flag.
6. **No real-PostgreSQL journal tests.** The direct-tool journal tests use the in-memory fixture
   journal. No DB-gated test covers `postgres_checkpointer/node_attempts.rs` yet, for Code nodes
   either. Crash-window and second-claim tests against PostgreSQL are a follow-up.
7. **LLM-node receipts.** Pipeline LLM nodes admit effectful sensitive tools without an effect
   receipt (`src/agents/graph/llm.rs:1326-1470`). They should move to the same journal so the two
   node kinds share one guarantee (Gate 6, `docs/remaining-gates.md:60`).
