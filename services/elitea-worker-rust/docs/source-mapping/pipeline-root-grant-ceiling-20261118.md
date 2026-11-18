# A pipeline root delegates as an ordinary root; a refused graph node ends with Main's notice (FU-335)

- **Branch:** `fix/fu335-pipeline-root-ceiling`, off `main` at `25829e2b4`. It replaces the first FU-335 branch
  (`fix/fu335-pipeline-root-grants`, kept for reference): main has since shipped the delegation grants model and
  notice (#2211, #2213, #2216, #2222, #2235), so this branch carries only the ceiling and the refused graph node,
  on main's model. No new failure kind, frame, client contract, replay rule or Web change.
- **Paths:** `W/` = `services/elitea-worker-rust/src`, `L/` = `libs/rust/agent-runtime/src`, `M/` =
  `services/elitea-main/internal`.
- **Rule (ADR-0044 decision 3 and Amendment 1, as written):** `E(X) = own(X) ∩ (E(Y) ∪ G(Y→X)) ∩ P`. The root is
  `own(root) ∩ P`. An edge grant adds what the caller lacks and never takes away what the caller holds.

## ADR-0044 note

A pipeline root is now an ordinary root for delegation: its `E(root)` is its own admitted, policy-applied attachments
bounded by the run principal, exactly as an ordinary agent root's (`W/agents/ordinary.rs:356-357,409-413`) and as
Main's desktop evaluator reads a caller (`M/application/delegationedge/edge.go:126,155`). The condition the
orchestrator set holds and is tested: the fix never gives a pipeline root more than an ordinary root with the same
attachments, grants and policy (parity test below). A partial approval (`omitted` items) narrows only what the
caller itself does not hold; this is the formula as written, not a change to it.

## Business behaviour

- **Current platform (reference only).** A pipeline's Agent node runs a saved agent with the tools that agent holds.
  There is no grant narrowing there; nothing else is ported.
- **Before (main `25829e2b4`).**
  - The pipeline root's ceiling was `principal_bounded_snapshot(toolsets.retain_toolkit_names(aliases))`
    (`W/agents/pipeline.rs:673-677`, used at `:838-842`): only the toolkits its own LLM and toolkit nodes select. That
    reduction exists so unused toolkits' credentials are not materialized; as the ceiling it dropped every toolkit
    attached for an Agent node's saved agent. Root → Agent node → saved agent S (toolkit 100) → pipeline tool P1 (MCP
    node on 100) was refused although every level held 100 (chats 896–899). With grants on, an approved edge grant
    covers this (`L/delegation/grants.rs:236-254`), but the switch is off by default
    (`M/runtimecomposition/config.go:158-163,351-357`), and an edge without an approved grant (missing, pending,
    revoked, every S6 legacy grant until approved) still met the reduced ceiling.
  - A refused saved agent bound to an Agent node or Map member was offered as a refusal tool no node can answer
    (`bind_scoped_agent`, `W/agents/pipeline.rs:2864-2923` on main): the run ended `agent.legacy` ("The runtime
    operation failed.") at the root and "the stored pipeline could not be compiled" for a Map member, with no notice.
    #2235 had fixed only the saved-pipeline node (`pipeline.rs:2706`).
- **Now.**
  - `root_delegation_and_credentials` (`W/agents/pipeline.rs:2880-2893`, called at `:674`, scope used at `:835`)
    builds the scope from the principal-bounded full admitted snapshot, then reduces that snapshot to the nodes'
    aliases for credentials. The extraction (commit `e975c0343`) is behaviour-preserving; the fix (`716251778`)
    changes only which snapshot the scope reads.
  - `bind_scoped_agent` takes the node's graph path (`W/agents/pipeline.rs:2902-2972`; root call site `:1099`,
    nested call site `:2779-2786`, with the path `graph_path/node` computed once per node at `:2742-2746`) and ends a refused binding with #2235's
    `delegation_refused_error` (`:2953-2957`; `W/agents/application_tools.rs:4069-4091`): `AUTHORIZATION_FAILED`,
    cause `DELEGATION_GRANT_MISSING` / `DELEGATION_DEPTH_EXCEEDED` with the grant kind, and Main's `DelegationNotice`
    (edge kind `pipeline_node`, id `pipeline_node:<graph path>`, `callee_id` for Review delegation), which the
    lifecycle publishes before the terminal (`W/execution/native_agent_lifecycle.rs:264-276`, unchanged). The entry's
    enforced refusal is read by `MaterializedApplicationTool::enforced_refusal`
    (`W/agents/application_tools.rs:3084-3093`).
  - Inside a saved pipeline offered as a tool, the refusal stays that tool's result
    (`W/agents/pipeline.rs:2636-2648`): the error also carries the refusal (`L/assembly_error.rs:96,128-136`), so the
    binder answers it as the tool's refusal; an uncalled tool fails nothing, a called one answers by name. Main's
    test `a_pipeline_agent_node_offers_a_refused_saved_agent` becomes
    `a_pipeline_tool_whose_agent_node_is_refused_is_offered_as_the_refusal`
    (`W/agents/application_tools/materialization_tests.rs`).
- **Deliberately not ported / not changed.** The first branch's `DelegationGrantMissing` failure kind and
  `runtime.delegation_grant_missing` sentence, its `agent_delegation_refused` frame, client contract 1.54, replay
  gate and Web `DelegationRefusalNotice` are dropped: main's `DelegationNotice` and `runtime.authorization_failed`
  cover them. Main and Web are untouched.

## Changed paths

- `W/agents/pipeline.rs`: the scope helper, its call site, `bind_scoped_agent`'s refusal check and graph path, the
  pipeline-as-tool binder.
- `W/agents/application_tools.rs`: `enforced_refusal`; `delegation_refused_error` names graph nodes generally and
  carries the refusal.
- `L/assembly_error.rs`: `with_delegation_refusal` / `delegation_refusal` (in-process only, never serialized).
- Tests: `W/agents/pipeline_root_grant_tests.rs` (new, 27 tests), `W/agents/pipeline_tests.rs` (module),
  `W/agents/application_tools/materialization_tests.rs` (one test renamed and re-asserted).

## Tests

`ELITEA_TEST_DATABASE_URL=<disposable postgres:18> cargo test --offline --locked --all-features --lib root_grant_tests`:
27 passed, 0 skipped (the four Map cases run on PostgreSQL 18). Failing-first output:
`handoffs/point5-wave1-20261008/fu335/evidence/step0-fails-on-main-25829e2b4.txt` (local evidence).

Every protection test failed first or was mutation-checked. FF = failed on unmodified main `25829e2b4` (or, for the
parity test, after the behaviour-preserving extraction). Each mutation was applied alone to the committed fix, the
suite run, and the file restored (`git diff --quiet HEAD` verified):
M1 unconstrained ceiling; M2 the pipeline skips its principal bound; M2c no principal bound anywhere in the ceiling
(pipeline and `DelegationScope::root`); M5 a pipeline tool's refused Agent node not kept as the tool's result;
M6 edge grants ignored; M7 report mode enforces; M8 a grant widens past its recorded set; M10 delegation made
transitive; M11 the notice dropped; M12 the nested graph path lost; M14 credentials not reduced to the nodes'
toolkits; M15 a missing grant ignored. M2 alone and `DelegationScope::root`'s bound alone each leave the ceiling
unchanged (P is applied twice); M2 is caught by the credential test, M2c by the parity test.

| Test | Failed first | Mutations that fail it |
|---|---|---|
| `a_pipeline_root_delegates_an_attached_but_unused_toolkit_to_its_agent_node` | FF (`agent.legacy`) | — |
| `the_reported_root_agent_pipeline_tool_mcp_shape_completes` (shape 899) | FF (`agent.legacy`) | M14 |
| `postgres_a_map_member_holding_the_roots_attached_toolkit_runs` | FF (could not be compiled) | — |
| `an_attached_toolkit_nothing_uses_is_never_dialled` | FF | M14 |
| `the_delegation_depth_cap_is_unchanged` | FF | M14 |
| `a_root_agent_node_refused_a_toolkit_ends_typed_naming_it` | FF (not refused at start) | M1, M11, M15 |
| `postgres_a_map_member_refused_a_toolkit_ends_typed_naming_it` | FF (could not be compiled) | M1, M11, M15 |
| `an_agent_node_inside_a_saved_pipeline_node_is_refused_by_its_graph_path` | FF | M11, M12, M15 |
| `a_depth_refusal_at_an_agent_node_carries_the_notice` | FF | M11, M12 |
| `a_toolkit_removed_by_policy_is_not_granted` | FF | M1, M11, M15 |
| `a_toolkit_outside_the_principal_is_refused_in_both_modes` | FF | M11 |
| `with_grants_on_an_edge_without_a_grant_is_refused_with_the_notice` | FF | M1, M11, M15 |
| `postgres_with_grants_on_a_map_member_without_a_grant_is_refused_with_the_notice` | FF | M1, M11, M15 |
| `with_grants_on_a_partial_grant_never_cuts_what_the_root_holds` (the ruling) | FF | — |
| `a_pipeline_root_delegates_exactly_as_an_ordinary_root_with_the_same_inputs` (parity) | FF | M1, M2c, M6, M8, M14 |
| `a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal` | FF | M5, M10, M14, M15 |
| `with_grants_on_an_approved_edge_grant_covers_what_the_root_lacks` | passes on main | M6 |
| `postgres_with_grants_on_a_map_member_with_an_approved_grant_runs` | passes on main | M6 |
| `with_grants_on_a_partial_grant_covers_only_its_recorded_set` (stale) | passes on main | M1, M6, M8 |
| `report_mode_records_a_missing_grant_and_runs` | passes on main | M7 |
| `an_uncalled_pipeline_tool_with_a_refused_agent_node_does_not_fail_its_agents_turn` | passes on main | M5 |
| `a_toolkit_held_only_by_a_descendant_is_refused_at_its_own_hop` | passes on main | M15 |
| `the_root_ceiling_never_passes_through_a_caller_that_lacks_the_toolkit` | passes on main | M10, M14, M15 |
| `a_foreign_project_agent_is_refused_although_the_root_holds_its_toolkit` | passes on main | M14 |
| `a_pipeline_roots_own_credentials_stay_within_the_principal` | added with the fix (main also bounds) | M2, M2c |
| `an_already_allowed_child_keeps_its_effective_grant_digest` | passes on main | golden digest pin, not mutated |
| `postgres_control_a_map_member_agent_without_toolkits_runs` | control | fixture control, not a protection |
| `materialization_tests::a_pipeline_tool_whose_agent_node_is_refused_is_offered_as_the_refusal` (main's test, re-asserted) | changed behaviour | M5, M15 |

### Gates (on `8c150ad1a` plus these docs)

| Gate | Result |
|---|---|
| Worker `cargo fmt --all -- --check` | ok |
| Worker `cargo clippy --all-targets --all-features -- -D warnings` | 0 warnings |
| Worker rustdoc (`RUSTDOCFLAGS=-D warnings cargo doc --no-deps`) | ok |
| Worker lib on PostgreSQL 18 (receipts required, disposable guard) | 3470 passed, 0 failed, 149 ignored |
| Ignored-test guard (`scripts/ci/check-worker-ignored-tests.mjs`) | 149 ignored: 135 run by CI, 14 allowlisted, 0 run by nothing, 0 errors; its own tests 49/49 |
| agent-runtime fmt, clippy `-D warnings`, rustdoc | ok |
| agent-runtime lib | 1960 passed, 0 failed, 1 ignored |
| Faults (`go test ./...` in `libs/go/faults`, every package; `go generate` clean) | ok |
| Main `go build ./...`; client-contract lock test | ok; ok (untouched) |
| `check-upgrading-notes.mjs` | ok |

`code-review` and `security-review` skill passes were not run in this session; the Security section states each
category by hand. Dependency scanners are not affected (no dependency change).

### Main moved under the branch (#2239)

The branch was cut from main `25829e2b4`; the failing-first evidence stays recorded against it. Main then landed
#2239, which changes no Worker file and flips two Main defaults: `ELITEA_DELEGATION_GRANTS_ENFORCED` is `"true"` and
the principal bound is on, and Main's composition walk writes a **pending** grant for an edge whose callee holds more
than its caller. The branch merges it (`882b518f8`), and the gates above were re-run on the merged head. No Worker
test depends on Main's default: each builds its `DelegationPolicy`, grants and principal bound explicitly. The grant
cases (approved, missing, pending, revoked, stale, partial) and the parity test carry their grants and principal
bound in the command, and the ceiling cases need no grant because the root holds what the callee holds, so they hold
with the switch on or off.

## Performance

- The scope is built once per assembly from the already-admitted snapshot (`GrantSet::from_snapshot`, O(attachments));
  the principal bound is applied once more in `DelegationScope::root` (`L/delegation.rs:1173-1179`), as on the
  ordinary path. No new I/O, query or allocation per node.
- Credentials are unchanged: still materialized only for toolkits a root node selects. Proven by
  `an_attached_toolkit_nothing_uses_is_never_dialled` (0 dials of the unused toolkit) and
  `a_pipeline_roots_own_credentials_stay_within_the_principal`.
- The refusal check is one field read per bound Agent node, before any model call, and before the refused node or
  any later node is bound (every refusal test asserts 0 model calls and 0 dials). Nodes are bound in order, as
  #2235's refused pipeline node is: an allowed Agent node bound *before* the refused one has already materialized
  its toolkits (an MCP toolkit lists its tools), with no model call and no effect. A pre-pass that refuses every
  graph node before binding any is FU-357.

## Durability

- No new state. Assembly is a pure function of the frozen command (versions, policy, grants, principal); a reclaim
  re-assembles and reaches the same scope and the same refusal. The notice is published through the existing
  assembly-notice path before the terminal, as #2235's (`W/execution/native_agent_lifecycle.rs:264-276`).

## Resilience

- A refused Agent node or Map member is a typed `AUTHORIZATION_FAILED` at start, with the cause code and grant kind,
  never an untyped `agent.legacy` or "could not be compiled". A depth refusal carries the notice too (`reason`
  `depth`). Bounded: the notice is main's (at most 16 items; ids bounded at 128 bytes).

## Security

- **Authorization fails closed.** The ceiling is `own(root) ∩ P` by construction; a toolkit outside P is refused in
  both modes although the root attaches it (`a_toolkit_outside_the_principal_is_refused_in_both_modes`); a
  policy-removed toolkit is not delegated (`a_toolkit_removed_by_policy_is_not_granted`); a foreign-project agent is
  never read (`a_foreign_project_agent_is_refused_although_the_root_holds_its_toolkit`); never transitive
  (`the_root_ceiling_never_passes_through_a_caller_that_lacks_the_toolkit`).
- **Edge grants are never bypassed or widened.** The edge path (`L/delegation.rs:1315-1326,1358-1413`,
  `L/delegation/grants.rs:165-254`) is unchanged; a partial or stale grant covers only its recorded set where the
  root lacks the item (`with_grants_on_a_partial_grant_covers_only_its_recorded_set`).
- **Parity.** `a_pipeline_root_delegates_exactly_as_an_ordinary_root_with_the_same_inputs` composes both roots with
  their production code under one claim (partial grant, principal bound) and compares the ceiling, the effective set,
  the narrowing records and the tools S keeps, for a root that holds the items and one that does not.
- **Data.** The notice is main's: names and ids only. The in-process refusal on the error
  (`L/assembly_error.rs`, `delegation_refusal`) has one writer (`delegation_refused_error`,
  `W/agents/application_tools.rs`) and one reader (`admit_saved_pipeline_child`, `W/agents/pipeline.rs`), which turns
  it into main's existing refusal tool. The error's `Debug` prints only its code and `Display` its static message, so
  no log line, event or record carries it. An uncalled pipeline tool never fails its agent's turn
  (`an_uncalled_pipeline_tool_with_a_refused_agent_node_does_not_fail_its_agents_turn`); a called one answers the
  refusal as its tool result (`a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal`).
- **The refactor of authorization code is behaviour-neutral.** `e975c0343` moves main's two calls
  (`principal_bounded_snapshot` of the alias-reduced snapshot, then `root_delegation_scope` of it) into
  `root_delegation_and_credentials` with the same arguments and order; `root_delegation_scope` is pure (a
  `GrantSet` from the snapshot and the policy `Arc`), so calling it before the later assembly steps changes nothing.
  The commit touches no existing test; across the branch the only changed existing test is the one whose meaning the
  fix changes (`application_tools/materialization_tests.rs`, commit `716251778`). The delegation goldens and the
  FU-323a/FU-338 digest goldens are unmodified and pass (Worker lib run below).
- `security-review` and `code-review`: see the PR description.

## Recovery guarantee rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission (assembly) of a pipeline root delegating to an Agent node, Map member or saved pipeline node | R (unchanged): assembly is pure and repeats identically on reclaim | `W/agents/pipeline.rs:2880-2893` | parity and ceiling tests |
| Worker × admission of a refused Agent node or Map member | F: typed `AUTHORIZATION_FAILED` with Main's notice published first; refused before any model call or effect and before the refused node is bound (an earlier allowed node may have listed its MCP tools, FU-357), so nothing repeats | `W/agents/pipeline.rs:2953-2957`, `W/agents/application_tools.rs:4069-4091`, `W/execution/native_agent_lifecycle.rs:264-276` | refusal tests (root, nested, Map on PostgreSQL 18) |
| Worker × tool call of a saved pipeline whose Agent node is refused | F as a tool result (the turn goes on) | `W/agents/pipeline.rs:2636-2648` | `a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal` |
| Main, NATS, PostgreSQL, sandbox supervisor, LLM gateway, Web | not touched | — | — |

## Real-browser evidence

Run 2026-11-18 on the disposable stack `fu335s1` (`http://fu335s1.localhost:49900/app/`, production profile,
`verify-profile: 50 passed, 0 failed`), Claude in Chrome, main chat, each case reloaded. Images:

| Service | Image | Id | Branch check |
|---|---|---|---|
| Worker | `elitea-worker-rust:fu335b-be773eb8c` (this branch's head) | `785624f24e09` | `binary-check … "a graph node delegates to a saved agent or pipeline its caller may not run"`: present (1); absent in `main-ce2756fad-verify` |
| Main | `elitea-main:fu335b-882b518f8` (merged head; no Main change on the branch) | `3fd848d7e171` | `/elitea-migrate` carries `0254_delegation_migration`, `/elitea-main` carries `delegation_legacy_pending` (#2239) |
| Web | `elitea-web:fu335b-882b518f8` | `cd999519bb35` | — |

Supervisor `pm-778e5beb8`, runners `pm-2d7f7d3e8-deno|-rust`. Main runs #2239's defaults: `ELITEA_DELEGATION_GRANTS_ENFORCED`
unset, which is `true`, and the principal bound on. On first spawn every preconfigured chat model answered one short
request (`eu.anthropic.claude-haiku`, `eu.anthropic.claude-sonnet-4-6`, `global.openai.gpt-5.6-luna`, `gpt-5.4`,
`gpt-5.4-mini`: each "OK", chat 887).

The stack is torn down; its database is kept locally as a `pg_dump -Fc` (mode 600, never committed).

Fixtures (project 2, created through Main's API, the same routes the UI calls: `POST applications`, the toolkit
relation and the application relation; no database write): P1 `fu_echo` (170, MCP node `echo` on toolkit 100 `c1 mock
mcp 20261008`), S `fu_echoer` (171, toolkit 100 + `fu_echo`), P0 `fu335 P0 shape899` (172, toolkit 100 + S), P0m
`fu335 P0m map` (173, toolkit 100 + S), P0r `fu335 P0r refusal` (174, S only), and for parity the ordinary agent R0
`fu335 R0 ordinary` (175, S only). Main's composition walk wrote a **pending** grant for every edge on attach
(grants 10–14), including P0→S and P0m→S where the root already holds the toolkit.

1. **Shape 899 (chat 888).** The run is admitted and S runs: no delegation refusal, although the P0→S grant is only
   pending (the root holds the toolkit, so the pending grant cuts nothing, as ruled). On main this run ended at start
   (`agent.legacy`). S's call of its pipeline tool P1 then stops at P1's MCP node with `PIPELINE_TOOL_UNAVAILABLE`,
   Worker `failure_reason` "A tool with external effects requires its current fenced node writer." The same stop
   happens when S is the chat's own agent (chat 889, an ordinary root), so it is not this change: an effectful direct
   node inside a saved pipeline offered as a tool of a non-member agent has no node writer (FU-358). Reload: same.
2. **Map (chat 890).** Completes: both items return the echo of "Echo the item via fu_echo"; the mock MCP server
   logged 2 `tools/call`. Members run the pipeline tool durably (FU-291), so the effectful node runs there. On main
   this run ended "Configuration type is not supported.". Reload: same.
3. **Refusal (chat 891).** P0r lacks the toolkit. Cause: **pending grant, root lacks T** (with grants on and only a
   pending P0r→S grant, the edge has no approved grant; T missing at the root is why it needs one). The run ends at
   start: Worker `delegation_refused` `{"code":"DELEGATION_GRANT_MISSING","kind":"toolkit","name":"c1mockmcp20261008"}`,
   `agent_assembly_delegation_notice_published`, then `native_agent.authorization_failed`; 0 model calls. The chat
   shows "Execution authorization failed." Live, main's notice did not appear; after the reload it sits above the
   terminal: **Delegation refused** — "delegate could not use c1mockmcp20261008 from its caller. This delegation is not
   approved." (persisted as `meta.delegation_notices_v1`, `code` `DELEGATION_GRANT_MISSING`, `reason` `no_grant`,
   edge `pipeline_node`, `callee_id` 171). **Review delegation is not offered**: the notice carries no `caller_id`.
   The ordinary root R0 (chat 892) shows the same: "fu_echoer could not use c1mockmcp20261008 from elitea-agent. This
   delegation is not approved.", also without `caller_id` and without Review delegation. The root's id reaches the
   policy only through `DelegationPolicy::with_grants` (`L/delegation.rs:235-247`), which Main calls only when the run
   carries an approved grant, so with every grant pending neither root names its caller (FU-359; the live gap is
   FU-360).
4. **Approve and rerun (chat 891).** Approved from P0r's editor, Tools row **Review delegation** ("What fu_echoer will
   be able to use": c1 mock mcp 20261008, tools echo and reverse, host mcp-mock), as `admin@centry.user`, the author
   (user 3; this profile is not governed, so no second person is needed). Grant 13 is `approved`. Rerun in the same
   chat: no delegation refusal, S runs (G(P0r→S) honoured as on an ordinary root) and stops at the same FU-358 node.
   Reload: the refusal turn and the rerun turns are both kept.

## Fixtures

All unit fixtures are built in code (saved versions served by a scripted Main RPC, scripted model and MCP server);
the Map cases use a disposable PostgreSQL 18 container. No database fixture.

## Follow-ups (re-checked against main 25829e2b4)

- **FU-353** (a nested saved agent's Attachments and Agent memory tools): still valid, separate cause, unchanged here.
- **FU-354** (a depth refusal at a graph node had no notice): closed by main's notice; proven by
  `a_depth_refusal_at_an_agent_node_carries_the_notice` (reason `depth`). The terminal keeps the generic
  `Execution authorization failed.`, as #2235's node does.
- **FU-355** (the refused-pipeline-tool step records the inner agent's delegation evidence, not P1's): still valid;
  the tool's refusal is the inner edge's (caller P2, depth 2), as `a_pipeline_tool_whose_agent_node_is_refused_is_offered_as_the_refusal`
  pins.
- **FU-357 (new): refuse every graph node before binding any.** Nodes are bound in order (as for #2235's refused
  pipeline node), so an allowed Agent node before a refused one has already materialized its toolkits, and inside a
  saved pipeline offered as a tool its unbuildable attachments are already recorded on the run's skipped-attachments
  notice although that tool is now offered as the refusal. No model call, effect or authority follows from either. A
  pre-pass needs each saved agent's narrowing without materializing it (`composition_refusal` walks only saved
  pipelines today).
- **FU-358 (new, pre-existing on main): an effectful direct node in a saved pipeline offered as a tool of a non-member
  agent has no node writer.** `PIPELINE_TOOL_UNAVAILABLE`, "A tool with external effects requires its current fenced
  node writer." Seen at a root Agent node (chat 888) and with the agent as the chat's own root (chat 889); a Map
  member runs the same pipeline tool durably (FU-291) and completes (chat 890).
- **FU-359 (new, pre-existing on main): no Review delegation on a root's refusal while every grant is pending.** The
  root's id reaches the policy only through `DelegationPolicy::with_grants`, which Main calls only when the run carries
  an approved grant, so the notice has no `caller_id` (pipeline root, chat 891; ordinary root, chat 892). Fix: send the
  root application id with the policy whenever the switch is on, with or without approved grants.
- **FU-360 (new): a refused graph node's notice shows only after a reload.** Live, chat 891 showed only "Execution
  authorization failed."; after the reload main's notice sits above it. The Worker publishes the notice before the
  terminal (`agent_assembly_delegation_notice_published`), so the live gap is on the Main/Web side. Not compared live
  with #2235's saved-pipeline node, which uses the same path.
- **Legacy unscoped continuation** (a conversation an older worker left paused inside an application call,
  `ApplicationContinuation::LegacyUnscoped`, `W/agents/pipeline.rs:926`): unchanged. Its direct Agent participants
  are bound by alias without the node check, so a refused saved agent there still answers its refusal text as the
  node's output, as on main; it grants nothing. Its saved-pipeline participant binder (`W/agents/pipeline.rs:1158`)
  passes no graph path, so a refused Agent node inside it is keyed by the inner node id. Neither path is reachable for
  a conversation this worker starts.

## Code review (round 1, `/code-review high`)

Nine candidates. Fixed: the stale comment on `bind_admitted_saved_pipeline`'s refusal (it now names
`admit_saved_pipeline_child` answering it as the tool result); the graph-path join computed once per node; the
`(alias, graph_path)` tuple split into two named parameters. Documented, not changed: binding order (FU-357 above,
same as #2235); the legacy unscoped path (above); the inner edge's evidence on a refused pipeline tool (FU-355); the
principal-bound log now counting a root toolkit no node uses, which is the ordinary root's behaviour
(`W/agents/ordinary.rs:356-357` bounds the full snapshot) and the parity this change asserts.
