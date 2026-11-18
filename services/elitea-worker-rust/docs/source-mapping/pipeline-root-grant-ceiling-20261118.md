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
  - `root_delegation_and_credentials` (`W/agents/pipeline.rs:2881-2898`, called at `:674`, scope used at `:835`)
    builds the scope from the principal-bounded full admitted snapshot, then reduces that snapshot to the nodes'
    aliases for credentials. The extraction (commit `e975c0343`) is behaviour-preserving; the fix (`716251778`)
    changes only which snapshot the scope reads.
  - `bind_scoped_agent` takes the node's graph path (`W/agents/pipeline.rs:2908-2958`; root call site `:1099`,
    nested call site `:2779-2786` with `graph_path/node`) and ends a refused binding with #2235's
    `delegation_refused_error` (`:2953-2957`; `W/agents/application_tools.rs:4069-4091`): `AUTHORIZATION_FAILED`,
    cause `DELEGATION_GRANT_MISSING` / `DELEGATION_DEPTH_EXCEEDED` with the grant kind, and Main's `DelegationNotice`
    (edge kind `pipeline_node`, id `pipeline_node:<graph path>`, `callee_id` for Review delegation), which the
    lifecycle publishes before the terminal (`W/execution/native_agent_lifecycle.rs:264-276`, unchanged). The entry's
    enforced refusal is read by `MaterializedApplicationTool::enforced_refusal`
    (`W/agents/application_tools.rs:3084-3093`).
  - Inside a saved pipeline offered as a tool, the refusal stays that tool's result
    (`W/agents/pipeline.rs:2637-2648`): the error also carries the refusal (`L/assembly_error.rs:96,128-136`), so the
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

## Performance

- The scope is built once per assembly from the already-admitted snapshot (`GrantSet::from_snapshot`, O(attachments));
  the principal bound is applied once more in `DelegationScope::root` (`L/delegation.rs:1173-1179`), as on the
  ordinary path. No new I/O, query or allocation per node.
- Credentials are unchanged: still materialized only for toolkits a root node selects. Proven by
  `an_attached_toolkit_nothing_uses_is_never_dialled` (0 dials of the unused toolkit) and
  `a_pipeline_roots_own_credentials_stay_within_the_principal`.
- The refusal check is one field read per bound Agent node, before any model call or dial (every refusal test asserts
  0 model calls and 0 dials).

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
- **Data.** The notice is main's: names and ids only. The in-process refusal on the error is never serialized.
- `security-review` and `code-review`: see the PR-less report (no PR on this branch by instruction).

## Recovery guarantee rows

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission (assembly) of a pipeline root delegating to an Agent node, Map member or saved pipeline node | R (unchanged): assembly is pure and repeats identically on reclaim | `W/agents/pipeline.rs:2881-2898` | parity and ceiling tests |
| Worker × admission of a refused Agent node or Map member | F: typed `AUTHORIZATION_FAILED` with Main's notice published first; refused before any model call, dial or effect, so nothing repeats | `W/agents/pipeline.rs:2953-2957`, `W/agents/application_tools.rs:4069-4091`, `W/execution/native_agent_lifecycle.rs:264-276` | refusal tests (root, nested, Map on PostgreSQL 18) |
| Worker × tool call of a saved pipeline whose Agent node is refused | F as a tool result (the turn goes on) | `W/agents/pipeline.rs:2637-2648` | `a_called_pipeline_tool_with_a_refused_agent_node_answers_the_refusal` |
| Main, NATS, PostgreSQL, sandbox supervisor, LLM gateway, Web | not touched | — | — |

## Real-browser evidence

**Not yet run on this branch** (the orchestrator asked for the plan first). The gate is open until it is.

Plan, on a disposable real-model stack (runbook `handoffs/real-model-stack/README.md`, production profile,
Worker image built from this branch after #1160 with a branch-unique symbol checked in the extracted binary):

1. **Shape 899.** Root pipeline P0 attaching MCP toolkit T, one Agent node → saved agent S (T attached) → S's pipeline
   tool P1 (MCP node on T). Expect: the run completes, P1's MCP result reaches S; reload; History shows the same.
2. **Map.** Root pipeline with a Map node whose member Agent node runs S (T attached to P0 and S). Expect: completes;
   reload.
3. **Refusal with Main's notice.** Remove T from P0 (keep it on S). Expect: the run ends at start with
   "Execution authorization failed." and, above it, the `delegation_refused` notice for the node (edge
   `pipeline_node`, missing toolkit T) with **Review delegation** opening S's delegation dialog on P0; reload keeps
   both. The default-off switch is enough: with `ELITEA_DELEGATION_GRANTS_ENFORCED` off and the default `enforce`
   mode, an edge with no grant is refused when the caller lacks what the callee holds.
4. **Grants on (optional, for the approve path).** Set `ELITEA_DELEGATION_GRANTS_ENFORCED: "true"` on the
   `elitea-main` service in the stack's compose overlay (`override.yml`; Helm: `deploy/helm/elitea/values.yaml:3186`),
   restart Main, approve the P0→S grant in Review delegation, run again without T on P0: expect it to run (the grant
   covers it); revoke it: expect step 3 again.

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
- The pipeline-participant binder (`W/agents/pipeline.rs:1158`) passes no graph path, so a refused Agent node inside a
  saved pipeline bound as a participant ends the run with a node notice keyed by the inner node; not covered by a
  dedicated test.
