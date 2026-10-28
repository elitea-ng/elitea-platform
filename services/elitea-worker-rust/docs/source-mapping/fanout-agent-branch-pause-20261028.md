# A saved Agent Parallel branch pauses with its own card (Point 5, FU-246)

Branch `feat/graph-fanout-agent-branch-pause`, stacked on `feat/graph-fanout-inline-llm-branches` (FU-247) and
`feat/graph-fanout-parallel-admitted` (Wave 2a-2, [mapping](fanout-parallel-admitted-20261027.md)). Contract:
`libs/proto/contracts/fanout-interrupt-decisions-v1.md` v1.1, unchanged — a saved Agent member's card is an ordinary
`tool_guard` / `ask_user` card on the 2a-2 ledger.

## Business behaviour

Taken from the current platform (EliteaUI + elitea-sdk sub-agent HITL inside parallel tool calls):

- A `parallel` branch that runs a saved Agent whose model asks the user (`ask_user`) or calls a sensitive tool raises
  that question or approval as its own card next to the other branches' cards. The card is answered on its own, in any
  order and tab; only that member's Agent resumes, with exactly that decision (the answer, an approval that runs the
  tool once, or a rejection the Agent is told about).
- One pause authority: the member's pause is never also shown as a legacy pause.

Deliberately not ported / still refused typed (`graph.fanout.agent_pause_unsupported`, readable, naming the node):

- a delegated sign-in raised by the member's Agent (FU-250 carries the credential reference end to end first);
- a pause raised deeper inside the member's Agent (an Agent the member's Agent calls).

## Design

1. Where the confirmation is born (the nested Agent's tool loop), a member builds its ledger card from the child's own
   sensitive catalog (`events::fanout_member_card`, `src/agents/events.rs:7054`; `application_tools.rs:5640
   fanout_member_pause`), returns it beside the nested interrupt ids (`FANOUT_MEMBER_CARDS_KEY`,
   `application_tools.rs:111`), and marks the forwarded confirmation (`FANOUT_MEMBER_PAUSE_KEY`, `events.rs:134`).
2. The scoped runtime persists the member's `Paused` receipt when the result carries cards
   (`src/agents/pipeline/scoped_runtime.rs:334`); a pause without cards keeps the typed refusal.
3. The Agent node carries the card in its interrupt data (`src/agents/graph/application.rs:1084`), so a re-raise after
   a restart is byte-equal; `child_card` / `card_parts` bind it (`src/agents/graph/parallel_cards.rs:97,143`, read by
   `events::fanout_member_card_parts`, `events.rs:7160`).
4. Projector, one pause authority: the marked confirmation records the call as a paused member call without a frame
   (`events.rs:1354`); when the member resumes in the same execution, its own re-announced call (`events.rs:3027`) and
   its Agent's re-emitted paused call (`events.rs:1441`) are consumed silently.
5. Decision: `CompilerParallelBranches::resume_agent_member` (`src/agents/graph/parallel_compiler.rs:500`) proves the
   decision against the member's card, reads the run's root session (`RootSessionSource`, `parallel_compiler.rs:43`;
   `PipelineRootSession`, `src/agents/session.rs:2118`; carried through the scope rebind `parallel_compiler.rs:224`),
   binds it to the forwarded confirmation through the same strict path a browser continuation of a nested Agent uses
   (`DirectHitlDecisionSet::fanout_member`, `src/agents/direct_hitl.rs:454` → `resolve` → `Nested`), installs it in the
   member scope's nested decision registry (`install_scope_resume`), and re-enters the Agent node with an empty resume;
   the scoped runtime finds `Paused` + the pending resume and resumes the Agent. The resume proof accepts the member's
   identical call re-announcement as the same call (`application_tools.rs:1191`).

## Changed paths

Worker only: `src/agents/{events.rs, application_tools.rs, direct_hitl.rs, session.rs, runtime.rs (test accessor)}`,
`src/agents/graph/{application.rs, parallel_cards.rs, parallel_compiler.rs, compiler.rs, parallel.rs}`,
`src/agents/pipeline/scoped_runtime.rs`; tests `pipeline_fanout_effect_pg_tests.rs`, `pause_frames_tests.rs`,
`parallel_cards_tests.rs`, `parallel_compiler_tests.rs`. No Main, Web, contract or migration change.

## Tests

Full suites on the branch head (`handoffs/point5-wave1-20261008/2a2/logs/*-fu246a.log`, production test profile, real
PostgreSQL):

- Worker: 2795 passed, 6 failed, 103 ignored (pre-existing ignores). The 6 failures are load flakes in unrelated
  PostgreSQL timing and budget tests (`postgres_graph_receipts_append…`, `postgres_application_subgraph_threads…`,
  `postgres_checkpointer_round_trips…`, `postgres_session_full_context…`, `superseded_recovery_markers…`,
  `fanout_load_meets_the_postgres_budgets`); each passes rerun in isolation.
- New in this branch: 4 real-PG tests (`pipeline_fanout_effect_pg_tests.rs:569,642,1177,1467`) and the
  `a_member_pause_card_*` unit tests (`pause_frames_tests.rs`), all passing.
- Main (`go test -race ./...`, unchanged by this branch): all ok except `TestImportKeepsBothSkillsWithIdenticalNames`
  under load; it passes in isolation.
- Web (unchanged by this branch): 19403 passed, 7 failed. Six are the pre-existing failures (Node ≥ 26 theme, 4
  steering, 1 workspacePages), which are identical on a clean `origin/main`. The seventh,
  `CreateApplication.test.tsx`, passes 14/14 in isolation.
- agent-runtime: not changed by this branch (1380/0 on the base, #1470).

## Performance

| Mechanism | Path | Proving test | Measured |
|---|---|---|---|
| The card is built once, where the confirmation is born; re-raises read it from the persisted interrupt (no rebuild, no session read) | `application_tools.rs:5640`, `parallel_cards.rs:97` | `a_member_pause_card_round_trips_for_a_sensitive_call_and_an_ask` (byte-stable) | 1 build per pause |
| The root session is read only when a member's decision is applied (one read per decision), never on a tick | `parallel_compiler.rs:500` | `postgres_fanout_agent_branches_approve_and_reject_their_own_calls` | 1 read per decision |
| Card counts and decision bounds are the 2a-2 limits (16 open cards, 16 per fetch) | runner | 2a-2 limit tests | unchanged |

## Durability

| Mechanism | Path | Proving test |
|---|---|---|
| The member's pause is a persisted `Paused` receipt plus a persisted Agent-node interrupt carrying the card: a restart re-raises the same card | `scoped_runtime.rs:334`, `application.rs:1084` | `postgres_a_wake_resumes_the_answered_agent_branch_and_parks_the_rest` (the unanswered members re-raise their cards in the wake) |
| The decision is bound to the forwarded confirmation persisted in the root session (not to anything in the decision) | `parallel_compiler.rs:500`, `direct_hitl.rs:454` | `postgres_fanout_agent_branch_asks_and_resumes_with_the_answer` (the resumed request carries the answer) |
| A wake lost while the resumed Agent is in its model call resumes that step with the answer (R), never asks again | 2a-2 recovery frontier + the member's model checkpoint | `postgres_a_wake_lost_mid_model_resumes_the_answered_agent_branch` (production recovery path) |

## Resilience

| Mechanism | Path | Proving test |
|---|---|---|
| Only the approved call runs, once; a rejection reaches the Agent as a decline | nested registry | `postgres_fanout_agent_branches_approve_and_reject_their_own_calls` (tool calls = 1) |
| A pause no card can express (delegated sign-in, a pause deeper inside) ends the member typed and readable | `application_tools.rs:5640`, `scoped_runtime.rs:334` | `a_member_pause_card_refuses_what_it_cannot_express` |
| A decision that does not answer the member's current card, or an unavailable root session, is refused typed (`graph.parallel.decision_refused`) | `parallel_compiler.rs:500` | strict checks before the resolve |
| One pause authority: no legacy pause frame in any of the runs | projector | every FU-246 PG test asserts no legacy frame (`run_keyed_deciding`) |

## Security

| Category (`rules/security.md`) | Applies | How it was checked |
|---|---|---|
| Identity / authorization | yes | No new route. A decision still arrives only through the 2a-2 claim-fenced ledger; it is then bound to the member's own forwarded confirmation by the same strict proof a browser continuation of a nested Agent uses (interrupt id, call id, call digest, guardrail kind), so it cannot target another member or another call. |
| Input bounds and strict schemas | yes | The card descriptor is built server-side from the child's own catalog and read back strictly (exact keys, known kinds and actions, hex digest, bounded text via the existing validators); tampered descriptors are refused (unit test). Tool arguments are masked as in the root projector. |
| Tool scope | yes | The resumed Agent uses its own frozen toolsets and sensitive catalog; only the decided call is affected. |
| Injection, egress, secrets | no new SQL, outbound call or logged value | — |

`/security-review`: no findings (delta `c0858d37e..be5a8ed4c`, reviewed in this session without subagents). Checked:
- A ledger decision reaches a member only after it matches the member's own proven card (interrupt id, call id and
  an action that card kind accepts; a member card is only `tool_guard` or `ask_user`, so `authorize` cannot pass, and
  Main refuses it at Decide).
- It is then bound by the strict nested-continuation `resolve` against the confirmation persisted in the root session.
- The root session is read with the claim plan's user and session ids, never with data from the decision.
- Refusal texts are fixed codes.
- No new SQL, outbound call, log field or dependency.

## Recovery rows (replatform-delivery-gate §2)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × saved Agent member pause and decision (live) | R | `parallel_compiler.rs:500`, nested registry | `postgres_fanout_agent_branch_asks_and_resumes_with_the_answer`, `..._approve_and_reject_...` |
| Worker × saved Agent member decided while parked (wake, new execution) | R | 2a-2 park/wake + `resume_agent_member` | `postgres_a_wake_resumes_the_answered_agent_branch_and_parks_the_rest` |
| Worker × crash mid-model in the resumed member Agent | R | member model checkpoint, 2a-2 recovery frontier | `postgres_a_wake_lost_mid_model_resumes_the_answered_agent_branch` |
| Worker × crash with an approved member call in flight | F typed (`graph.fanout.may_have_started`) | 2a-2 approved-call stop (`scoped_runtime.rs` `ChildFailure::ApprovedCallStop`) | 2a-2 tests; not re-proven for the member path here (follow-up) |

## Browser evidence

BLOCKED by the environment, stated honestly: on the `fanpar` stack (images `2a2-be5a8ed4c`, binary-checked for
`__elitea_fanout_member_cards_v1` and `elitea.fanout.member_pause.v1`, verify-profile 50/0) every saved Agent's first
model call is refused by the upstream provider (`llm_provider_refusal` 403), while pipeline LLM nodes on the same model
succeed — the same refusal recorded on 2026-10-27. The saved-Agent member therefore never reached its question. The
other card kinds in the same run were raised as before. Evidence: `handoffs/point5-wave1-20261008/2a2/evidence/
browser-cases.md` ("FU-246 browser attempt").

## Follow-ups

- Browser proof of a saved Agent member on a stack whose provider accepts saved-Agent calls.
- FU-250: a delegated sign-in raised by a member's Agent.
- A pause raised deeper inside a member's Agent (an Agent the member's Agent calls).
- A member Agent with an approved call in flight at a crash: re-prove `may_have_started` on the member path.
