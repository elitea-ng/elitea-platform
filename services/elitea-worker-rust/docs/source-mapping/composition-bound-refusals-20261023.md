# Composition bound violations are refused, never dropped (FU-244, FU-243)

Branch `fix/fu244-typed-bound-refusals`, from `main` `6b0cea32c` (#1407). Step 0 of the 2a-2 brief
(`handoffs/point5-wave1-20261008/T-2A2-parallel-brief.md`).

## Business behaviour

- **Current platform (reference only).** Attaching an agent as a tool when the child already reaches the parent is
  refused at the attach with a readable message (`pylon_main/plugins/elitea_core/utils/application_tools.py:1057-1107`,
  "Adding this agent would create a circular reference"); publish validation reports circular and too-deep sub-agent
  trees (`utils/publish_utils.py:422,668`). EliteaUI phrases the refusal (`mapAssociationError`); the Web port already
  carries that mapping (`apps/elitea-web/src/features/agents/lib/associationError.ts`), but Main had lost the backend
  half: the relation route attached anything.
- **Before this change (since #1407).** A cycle, graph-depth overflow, a third ordinary graph scope and a static-pausing
  saved pipeline beneath a pipeline root were silently dropped: the offending edge was not built and the run answered
  without it, with only a count in the Worker log (`pipeline_agent_attachment_skipped`). The user decided
  (2026-10-24) that this silent degrade is not acceptable: these are authoring errors.
- **Now.**
  - **Save time (Main).** Attaching an agent or pipeline (relation PATCH) and saving pipeline instructions are refused
    with `400 APPLICATION_COMPOSITION_INVALID` and a message naming the path (`"Root" → "Top" → "Middle" → "Leaf"`), the
    pipeline and the Agent node; nothing is written. Bounds: circular reference, 3 agent tiers, 3 levels of saved
    pipeline nesting, 2 levels of pipeline Agent nodes, 25 attached agents and pipelines, and a pipeline that pauses
    before/after a node used as a tool of an agent beneath a pipeline root.
  - **Run time (Worker).** The same violations, reached by a composition stored before this check or broken later by
    an edit of a descendant, are refused before any model call with the registered message
    "This agent or pipeline cannot run: its attached agents and pipelines form a circular reference, are nested deeper
    than allowed, or include a pipeline that pauses for review where the pause cannot be continued. Review its
    attachments, then remove or change the one that breaks the rule." (public code `APPLICATION_COMPOSITION_INVALID`).
  - **Genuinely unbuildable attachments still degrade** (a deleted frozen version, a pause kind the runtime cannot
    resume, an unavailable capability): the run answers without them, and now **every root names them**, pipeline roots
    included (FU-243): the `skipped_capabilities` notice of `agent_start` / `full_message` lists them as it does for an
    ordinary Agent.
- **Not ported.** The legacy depth-1 "container agent" rule (#5680) stays a Web-only guard as today; the server bounds
  are the Worker's real limits, so Main never refuses what the runtime runs.

## Design

- **One typed cause.** Every bound violation carries `application.composition_bound` with a data-free detail
  (`cycle`, `agent_tiers`, `pipeline_depth`, `ordinary_graph_scopes`, `application_hops`, `version_bytes`,
  `checkpoint_paths`, `static_pause_under_graph_root`): `agents/application_tools/materialization.rs:21-48`. Tagged at
  every bound: `materialization.rs:120-224` (scopes, tiers, depth, cycle, hops, bytes), `application_tools.rs:3225-3254`
  (nested build hops, tiers, cycle), `pipeline/composition.rs:89-123,294-297` (saved pipeline composition),
  `pipeline.rs:1830-1838` (static pause under a graph root).
- **Refused, not skipped.** `materialize_pipeline_children` returns a bound violation instead of skipping the child
  (`application_tools.rs:3065`). The lifecycle maps the cause to `RuntimeFailureKind::ApplicationCompositionInvalid`
  whatever coarse code the bound used (`execution/native_agent_lifecycle.rs:1558`; registered at
  `protocol/output.rs:863`, wire `UNSUPPORTED_CAPABILITY` + registered text, like the node-type refusal).
- **Named degrade on every root (FU-243).** `ApplicationMaterializationPath` carries one shared, bounded skipped set
  (`materialization.rs:187-205`, at most `MAX_SKIPPED_APPLICATION_CHILDREN` = 8), filled by `pipeline::bind_scoped_agent`
  (`pipeline.rs:2137`) and the legacy direct path (`pipeline.rs:815`); a pipeline root reads it into
  `PipelineRuntimeBindings` (`pipeline.rs:753`) and its projection (`agents/session.rs:1952`); an ordinary root merges
  it (`application_tools.rs:2962`).
- **Main save check.** `internal/domain/applicationcomposition/composition.go` walks the saved composition with the
  Worker's path rules (`Check` `:127`, `enter` `:182`, `enterAgent` `:199`, `enterPipeline` `:210`, `walkAgent` `:220`,
  `walkPipeline` `:267`, `ReadPipeline` `:393`). The version being saved is walked as a root of its own kind, so a
  violation found there is one in every root that reaches it (embedding only adds depth): the save never refuses what
  the runtime runs. Read through `repos.CompositionSource` (`internal/infra/db/repos/application_composition.go:130`),
  which reads the same attachment rows as the relation route (the reader moved there: `ListApplicationToolReferences`
  `:61`). Wired at the relation attach, before anything is written, under the parent row lock
  (`internal/api/v2/eliteacore/application_relation.go:326,373`), and at a pipeline instructions save
  (`internal/infra/db/repos/applications.go:831,996`). Main maps the Worker's run-time refusal to the same code
  (`internal/transport/runtimegrpc/output/server.go:1172`).
- **Shared contract.** `libs/proto/contracts/application-composition-limits-v1.json` holds the four bounds, the public
  code and the registered message; the Worker (`materialization_tests.rs`
  `composition_bounds_match_the_shared_contract`) and Main (`applicationcomposition` `TestBoundsMatchTheSharedContract`,
  `output` `TestApplicationCompositionFailureAdmitsOnlyTheRegisteredMessage`) assert it.
- **Web.** No change. The agent attach flow already shows Main's `{"error"}` (a circular refusal in EliteaUI's own
  phrasing, others verbatim) and the pipeline editor shows a save refusal inline (`EditPipelineAlerts.tsx:59-66`).

## Changed paths

Worker (`services/elitea-worker-rust/src`): `agents/application_tools.rs`, `agents/application_tools/materialization.rs`,
`agents/application_tools/materialization_tests.rs`, `agents/pipeline.rs`, `agents/pipeline/composition.rs`,
`agents/pipeline_scoped_tests.rs`, `agents/ordinary_tests.rs`, `agents/session.rs`, `execution/native_agent_lifecycle.rs`,
`execution/toolkit_delivery_processor.rs`, `protocol/output.rs`. No `libs/rust` change.
Main (`services/elitea-main/internal`): `domain/applicationcomposition/{composition.go,composition_test.go,contract_test.go}`
(new), `infra/db/repos/application_composition.go` (new), `infra/db/repos/applications.go`,
`api/v2/eliteacore/application_relation.go`, `api/v2/eliteacore/application_composition_postgres_integration_test.go`
(new), `transport/runtimegrpc/output/{server.go,composition_failure_test.go}`.
Contract: `libs/proto/contracts/application-composition-limits-v1.json` (new).

## Tests

TODO counts

## Performance

- **Budget (Main save check).** One walk reads each saved version at most once (per-walk cache) and enters at most
  `MaxApplicationHops` = 25 attachments, so a check costs at most 27 version reads (root, 25 entered, the refused one);
  each read is one version row plus the attachment rows (one query, plus the legacy-table probe and read on a
  legacy-migrated tenant): at most 27 × 3 = 81 statements, in the attach transaction or the save's pool. Proven by
  `TestEachVersionIsReadAtMostOnceWithinTheHopBudget` (diamond read once per version; a refused walk reads ≤ 27). Only
  an attach and a write that carries pipeline instructions pay it; an agent save never walks.
- Pipeline text is parsed only within the save-time bounds (`pipelinelimits.Check`: 512 KiB, alias expansion budget)
  and only top-level `nodes` / `interrupt_*` are read; an over-bound stored text is not read past its length
  (`CASE … octet_length … <= $3` in the version query).
- **Worker.** No new work on the run path: the cause is a field on the existing error; the skipped set is a bounded
  `BTreeSet` (≤ 8 entries) behind an uncontended mutex touched only when something was skipped.
- Measured: TODO

## Durability

- Nothing new is persisted by the Worker. A refusal happens at assembly, before any model call, tool call or
  receipt (proven by the tests asserting zero model calls and no read past the bound).
- Main: a refused attach writes no row (checked before the insert, under the parent row lock); a refused save stores
  nothing (`TestAPausingPipelineUnderAPipelineAgentNodeIsRefusedAtAttachAndAtSave` reads the row back).
- The check reads in the attach transaction, so the new edge and the parent lock are consistent; a concurrent edit of a
  *descendant* is not locked (the walk would have to lock the whole tree): such a race can store a violating
  composition, which the Worker then refuses typed at run time (row below). Documented, not hidden.

## Resilience

- Bounded: the walk's hop budget, per-walk cache, label length (64 runes, printable, quotes replaced), YAML size and
  alias budget; the skipped set (8).
- Typed and readable everywhere: a bound violation is `application.composition_bound` + detail in the Worker, the
  registered `APPLICATION_COMPOSITION_INVALID` terminal for the user, and a 400 with the same code at save. No silent
  coercion: the previous "drop the edge, log a count" is gone (`mixed_graph_agent_cycle_is_refused_before_provider_effects`
  and siblings were the dropped-edge tests and now assert the refusal).
- Unbuildable ≠ bound: a deleted version or unavailable capability still degrades, named
  (`an_unbuildable_attachment_of_a_pipeline_agent_node_is_skipped_and_the_node_answers` now also asserts the pipeline
  root's notice; `TestAnUnreadableAttachmentIsNotFollowed` on the Main side).

## Security

Checked against `rules/security.md`:
- **Identity / authorization.** No new route. The relation route's existing project scoping and parent row lock apply;
  the walk reads only the request's tenant schema (quoted via `tenantschema.Quote`), so a foreign project's version is
  simply not found and not followed. The Worker keeps its foreign-project refusal
  (`a_saved_participant_from_a_foreign_project_is_refused_before_any_effect`).
- **Input / amplification.** Stored YAML is parsed only after `pipelinelimits.Check` (size + alias-expansion budget);
  the walk is bounded by the hop budget and cache; cycles terminate (ancestor check).
- **Injection.** Parameterized SQL only; the schema identifier is the pre-quoted tenant schema.
- **Data in messages.** A save refusal echoes the author's own application names and node ids (bounded, printable)
  back to the author of the same project, as the existing node-type refusal does; the run-time message is data-free
  (registered text only); nothing is logged except the existing data-free error lines.
- **Secrets / egress / supply chain.** Not applicable: no secret handled, no outbound call, no new dependency.
- Not applicable to this change: egress, delegated credentials, sandbox, CSP.

## Recovery rows (replatform-delivery-gate §2)

TODO

## Browser evidence

TODO

## Follow-ups

TODO
