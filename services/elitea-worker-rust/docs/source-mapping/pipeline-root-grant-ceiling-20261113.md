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
    (`W/execution/native_agent_lifecycle.rs:1971-1981`) and publishes a notice before the terminal (`:297-316`,
    `:1913-1940`). The notice names the node kind and the toolkit's display name only
    (`W/agents/events.rs:4259-4295`, `L/delegation.rs:273-291`).
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
- Main: `M/transport/runtimegrpc/output/server.go`.
- Tests: `W/agents/pipeline_root_grant_tests.rs` (new, wired in `W/agents/pipeline_tests.rs`),
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
| `the_delegation_notice_names_only_the_node_kind_and_the_toolkit` | every grant kind × node kind: toolkit display name only; no tool, credential reference, data class, host, URL; control characters and size bounded | new code |
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
- `libs/go/faults`: every package `ok`. Main `internal/transport/runtimegrpc/output`: `ok`.
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
- Result: no generic `Internal` or `UnsupportedCapability` ending for this refusal. The notice is published but is not yet shown by Main (see Follow-ups).

## Security

- Authorization source: the ceiling is the root's own frozen attachments after `apply_policy`, never raw tools and
  never transitive (`W/agents/pipeline.rs:673-683`). Proofs: descendant-only, never-transitive, policy-removed,
  foreign-project, ordinary/pipeline equivalence.
- Fail closed: a refused bound Agent ends assembly before any effect; Report mode is unchanged.
- Credentials: materialized only for the alias-reduced snapshot, as before (`W/agents/pipeline.rs:784-812`); proof
  `an_attached_toolkit_nothing_uses_is_never_dialled`.
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

- **Open, needs a decision before the browser pass: the notice is not shown by today's Main.** Main treats a
  `full_message` node event as a turn's terminal: it is not woken live
  (`services/elitea-main/internal/runtimecomposition/execution_replay_wake.go:282-296`) and replay shows it only when
  the execution has an `AGENT_EXECUTION_RESULT` terminal (`services/elitea-main/internal/infra/db/repos/replay_events.go:118-133`).
  A run refused at start ends with a runtime-error terminal, so the notice is stored but hidden, live and after a
  reload; the user sees only the data-free sentence, whose "as the notice above says" then points at nothing. Options:
  (a) Main shows a `full_message` with a `delegation_refused` block beside a failed terminal (a Main change, plus a
  Web check); (b) end such a run COMPLETED with the notice as its result, as the #982 authorization notice does
  (an automation would read a refused run as success); (c) drop "as the notice above says" from the sentence and leave
  the name to the run's details.
- FU-353: a nested saved agent's Attachments and Agent memory tools (separate cause, decision needed).
- A depth refusal at a graph node keeps the generic authorization text; a registered depth text and notice would make
  it as readable as the grant refusal.
- The refusal evidence of a pipeline tool refused for an inner Agent node is that inner delegation's (depth and
  digest of the inner step), recorded on the tool's step.
