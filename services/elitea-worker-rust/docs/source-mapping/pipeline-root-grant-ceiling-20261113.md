# A pipeline root delegates its attached toolkits; a refused delegation is readable (FU-335)

- **Branch:** `fix/fu335-pipeline-root-grants`, off `main` at `fdab93771`.
- **Paths:** `W/` = `services/elitea-worker-rust/src`, `L/` = `libs/rust/agent-runtime/src`, `M/` =
  `services/elitea-main/internal`.
- **Rule:** ADR-0044 decision 3. A delegated saved agent or pipeline runs with the intersection of its own grants and
  its caller's; a root's ceiling is its own grants.

## Business behaviour

- **Current platform (reference only).** A pipeline's Agent node runs a saved agent with the tools that agent holds.
  There is no grant narrowing there, so nothing in it is ported except "a saved agent inside a pipeline uses its own
  attached toolkits".
- **Before (new platform, `fdab93771`).**
  - An ordinary agent root's ceiling was its full admitted tool snapshot (`W/agents/ordinary.rs:403-407`).
  - A pipeline root's ceiling was `toolsets.retain_toolkit_names(runtime_toolkit_aliases())`: only the toolkits its
    own LLM and toolkit nodes select. That filter exists to keep credentials of unused toolkits from being
    materialized (`L/toolkits/snapshot.rs:188-198`); reusing it as the grant ceiling dropped every toolkit attached
    for an Agent node's saved agent.
  - So root → Agent node → saved agent S (toolkit 100) → pipeline tool P1 (MCP node on 100) was refused
    `DELEGATION_GRANT_MISSING` although every level held 100 (FU-335, chats 896–899).
  - The refusal surfaced as generic text: at a root Agent node the refused agent was bound as a tool that emits no
    terminal event, so `W/agents/pipeline/scoped_runtime.rs:572` failed with `failure()` and adk-graph wrapped it as
    `agent.legacy` ("The runtime operation failed."); in a Map member the refused agent had no fingerprint, so the Map
    worker's identity was "unproved" (`W/agents/pipeline/parallel_application_resolver.rs:222-227`,
    `W/agents/graph/parallel_compiler.rs:550-557,1266-1272`) and the run ended "Configuration type is not supported."
- **Now.**
  - The pipeline root's ceiling is its own frozen, policy-applied attachments, taken before the alias reduction
    (`W/agents/pipeline.rs:673-683`, used at `:845`). The reduction still decides what is materialized.
  - A graph node (Agent node, Map item, Parallel branch) whose bound saved agent is refused ends the run at start,
    typed: `bind_scoped_agent` checks the bound entry's enforced refusal (`W/agents/pipeline.rs:2915-2921`,
    `W/agents/application_tools.rs:3084-3093`) and raises `delegation_refused_error` with the node kind
    (`W/agents/application_tools.rs:4037-4058`). A pipeline node refused for its saved pipeline does the same
    (`W/agents/pipeline.rs:2713`).
  - Inside a saved pipeline offered to an agent as a tool, that refusal stays scoped to the tool: the binder turns it
    into the tool's refusal (`W/agents/pipeline.rs:2637-2650`), so an uncalled tool fails nothing and a called one
    answers the refusal by name.
  - The lifecycle maps a missing grant to `RuntimeFailureKind::DelegationGrantMissing`
    (`W/execution/native_agent_lifecycle.rs:1971-1981`) and publishes a notice as a progress event before the
    terminal (`:297-316`, `:1913-1942`). The notice is a dedicated node event, `agent_delegation_refused`, carrying
    data only: the node kind and the toolkit's bounded display name (`W/agents/events.rs:4259-4295`,
    `L/delegation.rs:273-291`).
  - **Main shows that notice only beside a FAILED terminal (option a).** The replay query serves an
    `agent_delegation_refused` row only once the execution has a projected `RUNTIME_FAILURE` terminal at or after the
    notice's generation, and otherwise SKIPS it, never a barrier: a run that ends any other way (lost claim, deadline,
    stop) still delivers what follows (`M/infra/db/repos/replay_events.go:154-168`, review 1 item 2). A node event's
    JSON is decoded once per row (`:138-145`, review 1 item 5). Every other `full_message` on a failed run stays
    hidden as before, and a succeeded run is unchanged.
  - **Can a refused run be reclaimed under a new generation? No.** A reclaim takes a new claim attempt under the
    SAME generation (`M/infra/db/repos/claims.go:384-389,410-430`); it re-assembles, refuses again and ends with its
    own `RUNTIME_FAILURE` in that generation, which releases the earlier notice too. The gate is still written so it
    cannot stall: `terminal.generation >= replay.generation`, and a hidden notice is skipped. Its ingest wakes no stream (`M/runtimecomposition/execution_replay_wake.go:290-292`):
    a wake for a still-hidden row would make every waiter re-read the same barrier, so it is delivered with the
    failure, at the replay poll, like every failure today. For a reload, the failure projection copies the newest
    notice's node kind and toolkit name into the message group's `meta.delegation_refused`, only for
    `AUTHORIZATION_FAILED` and only when the name is within `MaxDelegationNoticeToolkitChars` (128)
    (`M/infra/db/repos/configuration_validation_results.go:515-588`).
  - **Web renders it above the failure text** from a registered template (four i18n keys
    `features.chatMessages.delegationRefusal.*`), with the toolkit name ONLY as data
    (`apps/elitea-web/src/features/chat-messages/ui/chat-box/DelegationRefusalNotice.tsx`,
    `ApplicationAnswer.tsx:330`). The name is bounded (`MAX_DELEGATION_TOOLKIT_NAME_CHARS` = 128) and rendered as a
    plain React text node, never parsed as HTML or markdown, so it is inert and `Jira <prod>` shows as written
    (review 1 item 4: sanitize-then-text dropped tag-like parts)
    (`apps/elitea-web/src/features/chat-messages/lib/delegationRefusal.ts`); the live frame
    (`chatStreamReducer.ts:130`) and the persisted meta (`convertMessagesToChatHistory.ts:338`) feed the same notice.
  - The terminal is `AUTHORIZATION_FAILED` with a data-free registered sentence (`W/protocol/output.rs:1088,1278-1282`,
    registry `runtime.delegation_grant_missing`); an older Main gets the generic authorization text once
    (`W/protocol/output.rs:974`). Main admits the exact sentence under that code only
    (`M/transport/runtimegrpc/output/server.go:1185-1190,1270-1272`).
- **Not ported / not changed.**
  - No transitive grants: a callee never defines its caller's ceiling (option B, rejected).
  - The depth cap and its refusal are unchanged; a depth refusal keeps the generic authorization text and no notice.
  - Built-in tools (Attachments, Agent memory) of a nested saved agent are a separate cause, out of scope: FU-353.

## Changed paths

- Worker: `W/agents/pipeline.rs`, `W/agents/application_tools.rs`, `W/agents/events.rs`,
  `W/execution/native_agent_lifecycle.rs`, `W/execution/agent_preparation.rs`,
  `W/execution/toolkit_delivery_processor.rs`, `W/protocol/output.rs`.
- Shared runtime: `L/delegation.rs` (`DelegatedCallee`, `toolkit_display_name`), `L/assembly_error.rs` (the refusal
  carried with the error).
- Registry: `libs/go/faults/registry/runtime.json`; generated `libs/go/faults/codes_gen.go`, `docs/errors/README.md`.
- Main: `M/transport/runtimegrpc/output/server.go` (the sentence), `M/infra/db/repos/replay_events.go` and
  `M/runtimecomposition/execution_replay_wake.go` (the notice beside a failed terminal),
  `M/infra/db/repos/configuration_validation_results.go` (the notice persisted for a reload).
- Web: `apps/elitea-web/src/features/chat-messages/lib/delegationRefusal.ts` (new),
  `ui/chat-box/DelegationRefusalNotice.tsx` (new), `ui/chat-box/ApplicationAnswer.tsx`, `lib/chatStreamReducer.ts`,
  `lib/convertMessagesToChatHistory.ts`, `lib/convertMessagesToChatHistory.types.ts`, `entities/message/lib/wire.ts`,
  `src/shared/i18n/en.json` (backfilled).
- Tests: `M/infra/db/repos/delegation_notice_postgres_integration_test.go` (new),
  `M/runtimecomposition/execution_replay_wake_test.go` (one test), `apps/elitea-web/src/features/chat-messages/lib/delegationRefusal.test.ts`
  and `ui/chat-box/DelegationRefusalNotice.test.tsx` (new), `W/agents/pipeline_root_grant_tests.rs` (new, wired in `W/agents/pipeline_tests.rs`),
  `W/agents/application_tools/materialization_tests.rs` (one test rewritten for C2),
  `W/protocol/output.rs` (fallback table test extended), `M/transport/runtimegrpc/output/delegation_grant_missing_test.go`
  (new).
- Docs: this mapping, the README entry, `services/elitea-worker-rust/docs/recovery-guarantees.md` (one row),
  `docs/upgrading/2026-11-13-0-pipeline-root-grant-ceiling.md`.

## Tests

STEP 0 evidence (failing on `fdab93771`) is kept outside the repository with the handoff.
`W/agents/pipeline_root_grant_tests.rs` runs through the real `PipelineNativeAgentAssembler::assemble` under an
ENFORCING delegation policy (the shared fixture authority only reports narrowing, `W/protocol/control.rs:1979-1991`,
which is why no earlier test met this); the Map cases run on the production PostgreSQL assembler.

| Test | Proves | First run |
|---|---|---|
| `a_pipeline_root_delegates_an_attached_but_unused_toolkit_to_its_agent_node` | (a) root attaches 92, no node uses it; S holds 92 and is offered its three tools | failed first (`agent.legacy`) |
| `the_reported_root_agent_pipeline_tool_mcp_shape_completes` | chat 899's shape completes; P1's MCP node really ran (2 dials, result reached S) | failed first (`agent.legacy`) |
| `postgres_a_map_member_holding_the_roots_attached_toolkit_runs` | the Map shape (chat 898) runs with the toolkit | failed first ("the stored pipeline could not be compiled") |
| `postgres_control_a_map_member_agent_without_toolkits_runs` | the Map fixture itself is valid | control, passed on base |
| `a_root_agent_node_refused_a_toolkit_ends_typed_naming_it` | root lacking 92 → typed refusal naming `batch`, `DelegationGrantMissing`, 0 model calls, 0 dials | failed first (`agent.legacy`) |
| `postgres_a_map_member_refused_a_toolkit_ends_typed_naming_it` | the same in a Map member, 0 model calls | failed first (`UnsupportedCapability`) |
| `the_delegation_notice_names_only_the_node_kind_and_the_toolkit` | every grant kind × node kind: a dedicated `agent_delegation_refused` event, data only, toolkit display name only; no tool, credential reference, data class, host, URL; the name bounded at `MAX_NOTICE_LABEL_BYTES` (limit kept, limit+1 cut) and control characters removed | new code |
| `TestPostgresDelegationNoticeIsShownBesideAFailedTerminal` (Main, real PostgreSQL) | (i) hidden before the terminal, shown beside a `RUNTIME_FAILURE` one, through the real replay query the live stream and a resume both read | failed on the previous head (shown before the terminal) |
| `TestPostgresAnOrdinaryFinalMessageStaysHiddenOnAFailedRun` | (ii) a partial `full_message` on a failed run stays hidden | guard (passes on both) |
| `TestPostgresASucceededRunIsUnchangedAndHidesTheNotice` | (iii) a succeeded run shows its final message, and no notice | failed on the previous head (notice shown) |
| `TestPostgresAFailedTurnPersistsItsDelegationNoticeForAReload` | reload: node and toolkit persisted at the bound; nothing past it (limit+1) or for another failure | failed on the previous head (nothing persisted) |
| `TestADelegationNoticeDoesNotWakeTheStreamItIsHiddenFrom` | the notice's ingest wakes no stream; progress still does | new |
| Web `delegationRefusal.test.ts` (6) and `DelegationRefusalNotice.test.tsx` (4) | parse; 128 kept / 129 cut; `<script>`, `<img onerror>`, `javascript:` link and a markdown link render inert; live frame then failure; reload from meta; notice above the failure text | the render tests failed with the notice line removed |
| `a_missing_grant_ends_with_its_registered_terminal_and_depth_stays_generic` | wire code, not retryable, data-free, older-Main fallback, depth stays generic | new code |
| `a_toolkit_held_only_by_a_descendant_is_refused_at_its_own_hop` | descendant-only toolkit refused at S → P1, S reads it by name, 0 dials | passed on base (guard) |
| `the_root_ceiling_never_passes_through_a_caller_that_lacks_the_toolkit` | never transitive: root holds 92, S not, P1 refused | passed on base (guard) |
| `a_toolkit_removed_by_policy_is_not_granted` | `apply_policy` removes it from the ceiling: S refused by name | guard |
| `a_foreign_project_agent_is_refused_although_the_root_holds_its_toolkit` | no foreign child under the wider ceiling; its version is never read | guard |
| `an_attached_toolkit_nothing_uses_is_never_dialled` | C1 (b): the unused toolkit gets 0 dials; the delegated one is dialled | guard |
| `the_delegation_depth_cap_is_unchanged` | max depth 1: S runs, P1 refused `DELEGATION_DEPTH_EXCEEDED` | guard |
| `ordinary_and_pipeline_roots_with_the_same_attachments_hold_the_same_ceiling` | C1 (a): equal ceiling digests; the alias-reduced one differs, so the test fails if it returns | new |
| `an_already_allowed_child_keeps_its_effective_grant_digest` | C3 golden: an already-allowed child's effective grant digest is equal under both ceilings and pinned | new |
| `an_uncalled_pipeline_tool_with_a_refused_agent_node_does_not_fail_its_agents_turn` | C2: uncalled P2 is offered as its refusal; S's turn completes | guard |
| `a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal` | C2: called, P2 answers the refusal by name; the inner agent never ran | guard |
| `a_pipeline_tool_whose_agent_node_is_refused_is_offered_as_the_refusal` (materialization) | replaces `a_pipeline_agent_node_offers_a_refused_saved_agent`: the C2 change at the unit level | the old test failed by design |
| `TestDelegationGrantMissingShowsItsReadableText`, `TestOutputServerAcceptsTheDelegationGrantMissingFrame` (Main) | exact text under its code only; registry byte-identical; frame settles typed | failed with the recognizer removed |

Counts on the branch head:

- Worker lib, `cargo test --offline --locked --all-features --lib` with `ELITEA_TEST_DATABASE_URL` (PostgreSQL 18,
  disposable), `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`:
  3357 passed, 0 failed, 147 ignored (unchanged), no SKIP line.
- `libs/rust`: `elitea-agent-runtime` lib 1864 passed, 1 ignored; `elitea-faults` 10 passed.
- `libs/go/faults`: every package `ok`.
- Main, `go vet` and `go test -race -count=1` with `ELITEA_TEST_DATABASE_URL` (PostgreSQL 18): `internal/infra/db/repos`
  ok (272 s), `internal/runtimecomposition` ok, `internal/transport/runtimegrpc/output` ok; `go build ./...` ok.
- Web: `tsc --noEmit` clean, `oxlint` clean, `i18n-backfill --check` OK (4 keys added), `check-dead-code` (knip) OK,
  vitest 7 affected files 182 passed (the 10 new among them).
- Production size of the notice change: Main +41 / -3 lines, Web +101 / -3 lines (5 of them `en.json`).
- C3: the FU-323a / FU-338 proofs pass unmodified, among them
  `root_saved_agent_frontier::postgres_a_root_saved_agent_node_lost_mid_model_resumes_from_its_model_checkpoint`,
  `postgres_a_root_agent_step_changed_after_its_grant_is_refused`,
  `postgres_a_receipt_without_its_definition_stamp_is_refused_unproven`,
  `postgres_a_saved_agent_edited_between_the_kill_and_the_reclaim_is_refused_typed`, and
  `materialization_tests::saved_agent_materialization_retains_sealed_identity_and_definition_digest`.

## Performance

- Mechanism: one `GrantSet::from_snapshot` over the root's admitted snapshot per assembly
  (`W/agents/pipeline.rs:678-682`), the same work an ordinary root does; no I/O, no extra round trip, no new
  allocation per node. The refusal check in `bind_scoped_agent` is one field read per bound Agent.
- Proof: the materialization path is unchanged (`an_attached_toolkit_nothing_uses_is_never_dialled`: 0 extra dials).
- Result: no measurable change; nothing on a hot path was added.

## Durability

- Mechanism: no new durable state. A refused delegation is decided at assembly, before the first effect or model
  call, so a retry or reclaim reaches the same decision from the same frozen versions.
- Proof: the refusal tests assert 0 model calls and 0 dials; C3 golden and the FU-323a / FU-338 proofs show that
  already-allowed children keep their delegation digests, so receipts and re-entry proofs of in-flight runs still
  match.
- Result: no drift.

## Resilience

- Mechanism: typed, readable failure (`DelegationGrantMissing`) with a bounded notice; the notice is best effort and
  never replaces the terminal (`W/execution/native_agent_lifecycle.rs:297-316`); an older Main gets the generic
  authorization text once (`W/protocol/output.rs:974`). A refusal inside a pipeline tool stays a tool result.
- Proof: `a_missing_grant_ends_with_its_registered_terminal_and_depth_stays_generic`,
  `a_refused_new_kind_is_replaced_by_the_kind_an_older_main_admits` (extended), the C2 pair.
- Result: no generic `Internal` or `UnsupportedCapability` ending for this refusal; the notice is shown above the
  failure text, live and after a reload. An older Main serves the unknown notice type as plain progress, which an
  older Web ignores, so a Worker ahead of Main never blocks the failure.

## Security

- Authorization source: the ceiling is the root's own frozen attachments after `apply_policy`, never raw tools and
  never transitive (`W/agents/pipeline.rs:673-683`). Proofs: descendant-only, never-transitive, policy-removed,
  foreign-project, ordinary/pipeline equivalence.
- Fail closed: a refused bound Agent ends assembly before any effect; Report mode is unchanged.
- Credentials (C1, "0 redemptions" by construction): every credential redemption and connection of a pipeline root
  runs over `selected_snapshot`, the alias-reduced snapshot: configured toolkits through
  `materialize_configured_toolsets_with_authorities_governed(&selected_snapshot, ..)` and MCP through
  `materialize_governed_mcp_toolsets(&selected_snapshot, ..)` or `materialize_mcp_toolsets_with_tokens_and_authorization(&selected_snapshot, ..)`
  (`W/agents/pipeline.rs:784-812`). The ceiling change does not reach that code: `delegation_root` is only handed
  to the delegation path (`:845`). An attached toolkit no node and no delegated descendant uses is therefore never
  redeemed; the dial count is tested (`an_attached_toolkit_nothing_uses_is_never_dialled`), the redemption is by
  construction (no governed seam in the test).
- Rendering: the toolkit name is author-written and shown in chat. Main persists it only within
  `MaxDelegationNoticeToolkitChars`; Web bounds it again and renders it as a React text node only, never HTML or
  markdown (Web tests: `<script>`, `<img onerror>`, a `javascript:` link and a markdown link render as literal,
  element-free text; `Jira <prod>` shows as written).
- The terminal sentence stands alone (review 1 item 3): it no longer points at the notice, which a failed publish,
  a client without the renderer (mobile) or a depth refusal does not show.
- Redaction: the notice shows only the node kind and a bounded, control-free toolkit display name
  (`L/delegation.rs:273-291`, `W/agents/events.rs:4259-4295`); never a credential reference, tool, data class, host
  or URL. The terminal is data-free; Main admits only the exact sentence under `AUTHORIZATION_FAILED`.
- Security findings: none opened; nothing here cites a local finding.

## Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission (P01) / delegation resolve (P12, P13): a graph node's saved agent or pipeline needs a grant its caller lacks | F typed (`AUTHORIZATION_FAILED`, `runtime.delegation_grant_missing`, notice naming the toolkit); refused before any effect, nothing repeated | `W/agents/pipeline.rs:2915-2921`, `W/execution/native_agent_lifecycle.rs:1971-1981,297-316` | `a_root_agent_node_refused_a_toolkit_ends_typed_naming_it`, `postgres_a_map_member_refused_a_toolkit_ends_typed_naming_it` |
| Worker × nested pipeline tool (P12): its Agent node's saved agent is refused | F as a tool result (the caller's turn goes on) | `W/agents/pipeline.rs:2637-2650` | the C2 pair |
| Worker × admission with the new ceiling (P01): already-allowed children | unchanged (R/I as before) | `W/agents/pipeline.rs:673-683` | C3 golden; FU-323a / FU-338 proofs unmodified |

The platform matrix row is in `services/elitea-worker-rust/docs/recovery-guarantees.md`.

## Real-browser evidence

Not run in this change; the browser pass on a disposable real-model stack is run by the reviewer (chat 899's shape,
the Map shape, one refusal showing the notice, each with a reload).

## Fixtures

All fixtures are built in test code: saved versions served by a scripted platform route (with Main's
`frozen_definition_sha256`), a scripted model gateway, a recording MCP connector, and for Map cases an isolated
PostgreSQL 18 database created per test. No database fixture was written by hand.

## Follow-ups

- FU-353: a nested saved agent's Attachments and Agent memory tools (separate cause, decision needed).
- FU-354: a depth refusal at a graph node keeps the generic authorization text and gets no notice.
- FU-355: the refused-pipeline-tool step records the inner Agent's delegation evidence, not P1's.
- Live latency: the notice and the failure reach an open chat at the replay poll, as every failure does today
  (the runtime-failure projection wakes no stream); waking on failure projection would serve both at once.
