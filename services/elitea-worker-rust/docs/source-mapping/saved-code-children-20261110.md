# Code inside a saved pipeline, at the root and as a fan-out member (W3-2b), 2026-11-10

Plan and reviews: `handoffs/point5-wave1-20261008/w3-2/PLAN-2b-DELTA.md` (D-b1..D-b4) and `REVIEW-PLAN-2b.md` (B1–B7,
which win where they differ), on top of `PLAN.md` / `REVIEW-PLAN.md`. Stacked on W3-2a (#1896, base `36788446f`).

Decisions applied:

- **Q-b1**: the root case was proven to fail first (below), then fixed here.
- **Q-b2**: a capture-based resolver. `RegisterSavedChild` stays unwired (FU-316).
- **Q-b3**: `graph.fanout.code_below_root` is kept (FU-317). `ContainsCode` is deleted.
- **Q2**: a saved child's Code time is the sum of its Code nodes' timeouts, each counted once.
- **Q3**: the slots are the smallest among the targets used.
- **Q7**: a direct Router child stays refused. A Router inside a saved child is admitted.

## Business behaviour

Taken from the current platform: a saved pipeline (an "application" of type pipeline) runs as a step of another
pipeline, through an Agent node, and its Code nodes run as they would at its own root.

New on the replatform: the same saved pipeline as a Map worker or a Parallel branch of the started pipeline. This is
the ETL `silver_partition` shape: a Map over partitions whose worker is `validate (Code) → route_quality (Router) →
normalize (Code)`.

Not ported, on purpose: the current platform's "lost, run again from scratch" failure behaviour. A Code attempt of a
saved child that may have started and has no owner proof ends typed and is never repeated.

## Pre-existing root gap closed here (Q-b1)

Before this change, Code inside a saved pipeline failed closed even outside any fan-out, for example when an Agent
node of the root runs a saved pipeline that contains a Code node. It failed at two layers:

- **Worker.** A saved child's runtimes carried no node journals for Code (`bind_saved_pipeline_runtimes` lends them to
  direct tool nodes only, FU-248), so its Code node ran on the raw path, which refuses an original saved declaration:
  "Code original-visit authority is unavailable on the raw invocation path." `with_saved_child_scope_provider` had no
  production caller.
- **Main.** The visit would have carried `saved_child_scope: null` on the saved pipeline's subgraph thread
  (`<root>/<agent node>`). The null-scope branch accepts only the root thread.

Proof, written first on the real Main repository path and run on `36788446f`:
`TestSavedChildCodeAtTheRootRegistersOnTheProductionPathPostgres` sent that shape and failed with
`source: original Code authority denied` (0 visits; the capture by `CaptureFrozenSavedChildVersion` succeeded).

The same test now asserts that this shape stays refused, and that the scoped visit and intent register. The Worker
half (journal, scope and recovery walk) is proven by the tests below.

Shapes refused before and admitted now:

- an Agent node of the root running a saved pipeline with Code;
- the same saved pipeline as a Map worker or Parallel branch, which was refused before it started with
  `graph.fanout.child_contains_code`.

## Changed paths

- Main:
  - `internal/infra/db/repos/code_source_saved_child.go` (new): the scope and its resolver.
  - `code_source_definition.go`: dispatch to the resolver.
  - `code_sandbox_intent.go`: a refusal keeps its stage.
  - `internal/infra/storage/runtime_saved_code_child.go` (new): `SavedCodeChildEdge`, `SavedCodeChildThread`.
  - `runtime_fanout_code_child.go`: `fanoutChildOwner` by owned type.
  - Tests: `saved_code_child_real_path_postgres_integration_test.go` (new), `runtime_saved_code_child_test.go` (new),
    and a helper in `code_registration_real_path_postgres_integration_test.go` (`newRealPathWorldWithVersion`, root
    tools; no assertion changed).
- Worker `src/agents/`:
  - `pipeline/saved_code_child.rs` (new): owner, member, scope, provider.
  - `pipeline/saved_child_http.rs`: `CodeVisitScope::SavedChild`.
  - `pipeline.rs`: the owner on root-level participants, the per-use provider, the B4 call, a test resolver.
  - `pipeline/composition.rs`: Main's frozen digest kept; `refuse_nested_saved_code`.
  - `pipeline/parallel_application_resolver.rs`: member rebinding.
  - `graph/application.rs`: the resolver hook.
  - `graph/compiler.rs`: runtime fields, journals for saved Code, Code leaves, `NestedSavedCode`, the `ContainsCode`
    removal.
  - `graph/parallel_compiler.rs`: `ContainsCode` check removed, Q2 for saved children, member binding.
  - `graph/parallel.rs`: a bare inner code names the member failure.
  - `session.rs`: Code leaves in the recovery walk.
  - Tests listed below.
- `libs/go/faults/registry/worker.json`, plus generated `codes_gen.go` and `docs/errors/README.md`: add
  `graph.fanout.nested_saved_code`. `libs/go/faults/legacy-codes.txt` drops `graph.fanout.child_contains_code`, so the
  list only shrinks.
- `docs/upgrading/2026-11-10-0-saved-code-children.md` (number assigned at PR time).
- Web: unchanged. No copy says that saved pipelines with Code are refused (`git grep` over `apps/elitea-web/src`). The
  Map worker hint already says "a saved agent or pipeline".

## Design as built

### D-b1, Main: the owner scope, resolved against the freeze capture

- **Decode and dispatch.** `readOriginalCodeSource` decodes `elitea.runtime.saved-code-child-scope.v1`
  (`code_source_definition.go:53`, `decodeSavedCodeChildScope`, `code_source_saved_child.go:42`: strict, at most 4 KiB,
  unknown fields refused). It does so in the transaction of every caller: the visit, the intent, the Supervisor read
  and the recovery read.
- **`readSavedCodeChildSource`** (`code_source_saved_child.go:73`) fails closed. Each refusal is `staged`, so Main's
  `code.registration_*` line names the step and carries no content (B7):

  | Stage | Line | Check |
  |---|---|---|
  | `saved_child_scope` | `:75` | Application and version are 1..2^31−1, the digest is valid, and the node ids are valid. The scope's child thread is the visit's thread, and its Code node is the visit's node. |
  | `saved_child_parent` | `:79-90` | The parent is the run's root thread (from Main's own claim access and input) when `fanout` is null. Otherwise it is a W3-2a fan-out child thread of the scope's family (`m1:`/`p1:`, not the root). |
  | `saved_child_thread` | `:94` | The child thread is exactly `parent + "/" + agent_node_id`: the subgraph thread ADK derives for an Agent node (`SavedCodeChildThread`, `runtime_saved_code_child.go:67`). At most 512 bytes. |
  | `saved_child_edge` | `:102` | The root's frozen source (`readOriginalRootSourceUnderOriginalAccess`) declares that Agent node selecting exactly the scope's application and version (`SavedCodeChildEdge`, `runtime_saved_code_child.go:28`). |
  | `saved_child_capture` | `:106-111` | A capture row exists for this execution, generation, application, version, digest, root-input digest, project and actor. The captured definition bytes are read and their digest recomputed (`FrozenDefinitionDigest`). The pre-redemption source is then read through the same capture (`readCapturedSavedChildSource`). |
  | `saved_child_code` | `:116` | The node is declared `type: code` in the captured saved pipeline, outside any Map or Parallel of it (`RootSavedCodePolicy` on the child's document). |

  For the edge check:
  - At the root, no Map or Parallel may own the Agent node (`:46`).
  - Under a fan-out, the W3-2a family checks apply: worker or branch at the ordinal, `1 ≤ item_count ≤ max_items`,
    `≤ 64` (`fanoutChildOwner` with owned type `agent`, `runtime_fanout_code_child.go:41`).
  - A tool with a foreign `application_project_id` is refused (`savedToolSelection`).

  The owning source returned is the saved pipeline's. `originalSource` then checks its YAML digest against the visit
  and pins the reference.
- **Stage kept** (`code_sandbox_intent.go:197`): a labelled refusal from the source read keeps its stage. It is still
  `code.ErrRejected`, so the old collapse of any source error into a refusal is unchanged.

### D-b1, Worker: one provider per use

- **Main's digest.** Composition admission keeps Main's `frozen_definition_sha256` for each saved version
  (`composition.rs:49`, `:124`).
- **The owner.** The scoped root resolver gives each root-level saved pipeline participant its owner
  (`pipeline.rs:1063`, `SavedCodeOwner`, `saved_code_child.rs:28`): application, version, Main's digest, and its Code
  node ids. Without Main's digest it has no owner, and its Code fails closed.
- **The provider.** `resolved_with` binds a `SavedCodeChildScopeProvider` per use, by the Agent node that runs it
  (`pipeline.rs:1670` → `for_saved_code_use`, `compiler.rs:381`), for both `resolve_node` and `resolve_node_in_fanout`.
  - A fan-out member rebinds its resolver with its member (`branch_journal_resolver`, `parallel_compiler.rs:1568` →
    `for_saved_code_member` → `rebind_saved_code_member`, `parallel_application_resolver.rs:93`;
    `SavedCodeFanoutMember::of`, `saved_code_child.rs:66`).
  - The provider serves only:
    - this saved pipeline's Code nodes;
    - on `<parent>/<agent node>`;
    - with the member's own thread as parent, under a fan-out;
    - for purpose `code_recovery` (`saved_code_child.rs:140`).

    Anything else is `PolicyDenied`.
- **Journals.** A scoped saved-child Code node journals through the run's family, or through the member's family when
  it runs in a fan-out member (`direct_tool_recovery`, FU-248), scoped to the child's visit (`compiler.rs:1902`,
  `:1932`). The visit-scoped rules then apply as for a W3-2a Code child: N3, R1, owner proof, and
  `child_effect_unknown` when there is no operator route.

### D-b2, Code time (Q2, Q3)

- Each Agent child records its saved pipeline's Code nodes at bind time: language and platform client, each node
  once (`saved_code_languages`, `parallel_compiler.rs:609`; `code_languages`, `compiler.rs:1465`).
- `admit_activation` counts them as that child's Code (`parallel_compiler.rs:1224`). The W3-2a `child_code_window`
  sums them and takes the smallest slots.
- **The underestimate** (B6): a Router loop can re-enter a Code node more times than the sum counted. Each attempt is
  still admitted against the run's deadline (`timeout + 90 s`), so a re-entry that no longer fits is refused typed
  (`pipeline.code.deadline_insufficient`) before any effect, and the run never overruns.
  - The per-attempt margin also means that once one Code node has run its full timeout, the second counted node
    needs its own margin.
  - A tight activation therefore refuses late attempts typed; it never lets them run past the deadline.

### D-b3, recovery

- **Root case (Worker kill).** The PM-2 recovery walk now stops at a saved child that the root's Agent node runs when
  that child is parked on an unowned Code node, or has no row yet and starts at one (`session.rs:1969`, `:2028`;
  `is_unowned_code` / `starts_at_code`, `compiler.rs:1157`, `:1168`).
  - This holds at the first level only, because deeper Code is refused (B4).
  - That attempt's visit-scoped journal decides the outcome: re-attach (R), re-run a lost runtime once (I), reconcile
    from owner proof (C), or end typed. Before this change the walk refused the frontier (`child_not_parked` /
    `child_missing`).
- **Depth 2.** A fan-out member resumes from its frozen occurrence (W3-2a). Its Agent node re-enters the saved child
  at its frontier, and the Code journal decides there.
- **Member failure (accepted behaviour change).** An Agent member whose saved child's Code ended
  `child_effect_unknown` (or the Code deadline) now names that code as the member failure (`graph_error_code`,
  `parallel.rs:2686`: a bare inner code now matches, as well as `code: sentence`). Before, it was
  `graph.node_execution_failed`. This was not requested; the orchestrator accepted it on 2026-11-10 as clearer, and it
  is in scope for /code-review. No existing assertion changed. Test: the C/F case of
  `real_pg_a_saved_child_platform_client_code_in_a_map_is_reconciled_never_rerun` asserts the named code.

### D-b4, refusals

- `ContainsCode` (`graph.fanout.child_contains_code`) is deleted (`parallel_compiler.rs:696`).
- `CodeBelowRoot` is kept.
- A direct Router child stays refused. A Router inside the saved child is admitted (unit test below).
- B4: `NestedSavedCode` (`graph.fanout.nested_saved_code`, registered) refuses a Code node of a saved pipeline that
  another saved pipeline runs.
  - It refuses before the run starts, by its path (`refuse_nested_saved_code`, `composition.rs:392`, called by the
    scoped root resolver at `pipeline.rs:1046`).
  - The terminal is readable: `PipelineNodeTypeNotAvailable`, through `FanoutChildRefusal::is_code`.
  - Main refuses the same shape independently at `saved_child_edge`: the scope's Agent node must be declared by the
    root.
- Typed reducers: the delta's fact 6 is not accurate. `compile_subgraph_with_runtime` refuses typed reducers in a
  nested pipeline in code (`compiler.rs:1669`, "typed state reducers are not supported in a nested pipeline"). The
  comment at `application.rs:795` is therefore correct, not stale, and is left unchanged.

## B1–B7

- **B1, who writes a capture.** `CaptureFrozenSavedChildVersion` (`execution_child_captures.go:16`) has exactly one
  production caller. That caller is the Worker's nested version read, `RuntimeApplicationVersionService.Resolve`
  (`runtime_application_version.go:247-249`), reached from the runtime-context route (`content_server.go:634`).
  - **That route freezes whatever application and version the Worker names** in the claim's project. It is not
    limited to applications reachable from the root's frozen definition.
  - `RegisterSavedChild` (unwired) writes `execution_saved_child_definitions`, not captures. Project deletion
    (`projectprovisioning/steps.go:444`) only deletes captures.
  - **So a capture alone proves nothing about reachability.** The single writer records whatever application and
    version the Worker names. What makes the resolver sound is the root-edge check, `saved_child_edge`: the run
    root's frozen source must declare the Agent node that selects exactly that application and version. The capture
    then only pins the version and digest the Worker ran. That check is one level deep; deeper levels are refused (B4).
    Accepted by the orchestrator on 2026-11-10.
  - Test: "an application the root selects but Main never captured" is refused at `saved_child_capture`. "An
    application the Agent node does not select" is refused at `saved_child_edge`, although it is captured.
- **B2, every field bound in the effect transaction.** See the table above. The tests:
  - `TestSavedChildCodeAtTheRootRegistersOnTheProductionPathPostgres` (visit and intent; replay idempotent);
  - `TestSavedChildCodeInAFanoutMemberRegistersOnTheProductionPathPostgres` (Map items 0–3; Parallel branches 0 and
    1, each with its own captured saved pipeline);
  - `TestSavedChildCodeScopeFailsClosedOnTheProductionPathPostgres` (24 negatives, each asserting its stage, and 0
    visit rows).
- **B3, negatives** (stage in brackets). Each refused case writes 0 visit rows.

  | Case | Stage |
  |---|---|
  | A version other than the captured one | `saved_child_edge` |
  | A digest other than the captured one | `saved_child_capture` |
  | An application not captured for this execution | `saved_child_capture` |
  | A capture from another generation (moved to generation 2) | `saved_child_capture` |
  | A capture from another execution in the same project (a sibling run with the same project, actor and input captured it) | `saved_child_capture` |
  | Captured bytes that no longer hash to the digest | `saved_child_capture` |
  | A Code node declared in a different captured application | `saved_child_code` |
  | A Router id on a Code visit | `saved_child_code` |
  | A visit node other than the scope's | `saved_child_scope` |
  | The same saved pipeline used twice (`run_saved`, `run_again`): both register, each with its own visit, and a visit naming the other use's thread is refused | `saved_child_scope` |

  W3-2a's set, adapted:

  | Case | Stage |
  |---|---|
  | Scope from another execution | `saved_child_parent` |
  | Map index outside the frozen count | `saved_child_edge` |
  | Count above `max_items` | `saved_child_edge` |
  | Forged family for a Map node | `saved_child_edge` |
  | Forged family kind | `saved_child_parent` |
  | Member on the root thread | `saved_child_parent` |
  | Member of the other family | `saved_child_parent` |
  | Parallel branch at the wrong ordinal | `saved_child_edge` |
  | Parallel branch count | `saved_child_edge` |
  | Foreign project tool (unit) | refused |
  | Visit thread other than the scope's | `saved_child_scope` |
  | Root-thread child | `saved_child_thread` |
  | Deeper thread | `saved_child_thread` |
  | Map-owned Agent node named at the root | `saved_child_edge` |
  | Root Agent node named as a member | `saved_child_edge` |
  | Unknown scope field | `source` |
- **B4.** Refused readably and registered: `graph.fanout.nested_saved_code`.
  - `code_below_a_second_saved_pipeline_is_refused_by_its_path` asserts the code and the detail `nested_saved_code`,
    and that one level down is admitted.
  - Main: "a nested saved child" is refused at `saved_child_edge`.
  - Reported for an FU: proving the scope through the chain (FU-318, registered).
- **B5, accepted on the same basis as W3-2a (orchestrator, 2026-11-10).** The basis is:
  - Main's real repository path on real PostgreSQL, including the takeover re-registration
    (`TestATakeoverReRegistersSavedChildCodeOnTheProductionPathPostgres`);
  - Worker real-PG kills at the root and at depth 2, both for a Map and for a Parallel branch
    (`real_pg_a_parallel_of_saved_children_with_code_survives_worker_replacement`);
  - the browser kill passes (a Worker kill and a Supervisor kill, on `silver_partition` and on the root case),
    pending in the orchestrating session.

  A real Worker↔Main harness does not exist; it is shared test-infrastructure work with W3-2a and is registered as
  exit work FU-321, under PLAN-REMAINING 0.6 (the #1261 crash suite, Docker lane).
- **B6.** `a_router_loop_beyond_the_saved_child_code_time_is_refused_per_attempt` (`code_slots_tests.rs`, paused
  clock).
  - Two Code nodes counted; the activation is admitted at exactly `60 s + 90 s`.
  - Each attempt takes 10 s. Three attempts run, the fourth (a Router re-entry) is refused `DeadlineInsufficient`
    before the deadline, and it holds no slot.
- **B7.**
  - `graph.fanout.nested_saved_code` is registered in the same commit as its use, and the legacy list only shrinks.
  - Main adds no code. Its stages are labels on the existing `code.registration_*` lines.
  - No definition content, item values or Code source reaches a refusal: the refusals are `ErrRejected` plus a stage,
    and on the Worker the node path.

## Tests

| Where | Tests | Result |
|---|---|---|
| Main `repos`, real PostgreSQL 18 (production `CodeIntentRepository` path, real `CaptureFrozenSavedChildVersion`) | `TestSavedChildCodeAtTheRootRegistersOnTheProductionPathPostgres` (Q-b1, then fix; uses twice; replay), `TestSavedChildCodeInAFanoutMemberRegistersOnTheProductionPathPostgres`, `TestSavedChildCodeScopeFailsClosedOnTheProductionPathPostgres` (24 subtests), `TestSavedChildCodeNeedsThisClaimsOwnCapturePostgres` (3 subtests), `TestATakeoverReRegistersSavedChildCodeOnTheProductionPathPostgres` (4 of 8 Map items, claim released, a new claim re-registers 8 at once: same 4 visits, 8 visits / 8 intents, no lock left) | 5 tests, 27 subtests, pass |
| Main `storage` (unit) | `TestSavedCodeChildEdgeSelectsOnlyTheDeclaredApplication` (3 admitted, 9 refused, foreign project, mismatched source), `TestSavedCodeChildThreadIsTheAgentNodesSubgraphThread` (limit 512 and 513) | pass |
| Main packages | `go vet ./...`; `go test -p 1 ./internal/infra/storage/ ./internal/infra/db/repos/` on PostgreSQL 18 | clean; pass |
| Faults gate | `go generate` + `go test ./...` in `libs/go/faults` | pass |
| Worker unit | `saved_code_child_tests.rs` (3: root scope JSON exact, member family, 6 + 1 + 4 refusals); `a_saved_pipeline_with_code_and_a_router_runs_as_a_map_worker_admitted_per_code_node` (the `silver_partition` Map compiles; admission `[2, 2, 2]`; no item starts when refused); `code_below_a_second_saved_pipeline_is_refused_by_its_path` (B4); `a_router_loop_beyond_the_saved_child_code_time_is_refused_per_attempt` (B6) | pass |
| Worker real PostgreSQL, CI prefix `agents::graph::node_recovery_runtime::tests::code_owner_tests::` (`node_recovery_saved_code_pg_tests.rs`; production compiler, production `NativePipelineApplicationResolver`, production checkpoint family, production `RemoteCodeRuntime`; fake Main and Supervisor) | root: `real_pg_a_saved_child_code_at_the_root_survives_worker_replacement` (R), `real_pg_a_supervisor_losing_a_saved_child_code_attempt_at_the_root_reruns_it_once` (I), `real_pg_a_saved_child_platform_client_code_at_the_root_is_reconciled_never_rerun` (C / F); depth 2: `real_pg_a_map_of_saved_children_with_code_survives_worker_replacement` (R), `real_pg_a_supervisor_losing_saved_child_code_in_a_map_reruns_each_once` (I), `real_pg_a_saved_child_platform_client_code_in_a_map_is_reconciled_never_rerun` (C / F); depth 2, Parallel: `real_pg_a_parallel_of_saved_children_with_code_survives_worker_replacement` (two branches each running the saved pipeline; killed with both `validate` attempts completed and both `normalize` attempts submitted: the same 4 identities, 4 resolved rows, 6 submissions = 4 first + 2 re-attached, completed attempts never submitted again, one answer per branch). Every test asserts the scope each visit named. | 7 of 7 pass (the Parallel test 4 runs in a row); the whole prefix 44 passed, 0 failed (W3-2a's 37 + 7) |
| Worker real PostgreSQL, two processes (`pipeline_fanout_effect_pg_tests.rs`; production `PipelineNativeAgentAssembler`, real `CodeRuntimeFactory`, the real inspect → authorize → `assemble_checkpoint` reclaim) | `postgres_a_saved_child_lost_inside_its_first_code_attempt_is_recovered_by_its_journal` (`starts_at_code`), `postgres_a_saved_child_lost_inside_a_later_code_attempt_is_recovered_by_its_journal` (Code leaf after a State Modifier). The crash process dies at Main's visit once its Started record is durable and its visit names its scope. The reclaim's walk admits the frontier, and the attempt re-enters the same journal thread. With the walk change disabled, both fail at the recover phase. | pass |

Existing tests deleted, each only asserting the `ContainsCode` refusal:

- `map_fanout_tests.rs` `a_map_worker_whose_saved_pipeline_contains_code_is_refused_typed`, at origin line 880;
- `parallel_compiler_tests.rs` `a_branch_whose_saved_pipeline_contains_code_is_refused_typed`, at :485;
- `parallel_compiler_tests.rs` `a_branch_with_code_is_refused_inside_a_nested_child_too`, at :518.

The shapes they refused are now admitted, or refused by `NestedSavedCode` / `ContainsParallel`.

Existing tests changed, with no assertion changed:

- The `CompilerParallelBranches` literals in four test files gained `saved_code: BTreeMap::new()`.
- `composition.rs` tests: one `AdmittedSavedPipeline` literal gained `frozen_definition_sha256: None`.
- The W3-2a fake Main records each visit's `saved_child_scope`.

## Runs (PostgreSQL 18, disposable `postgres:18`)

- **Worker.** `cargo fmt --all -- --check`, `clippy --lib --tests --all-features -D warnings` and
  `cargo doc -D warnings` are clean.
- **Worker lib** with `ELITEA_TEST_DATABASE_URL` and the receipt guard set as CI sets them: 3182 passed, 0 failed,
  133 ignored.
  - The 3 `graph_receipts` tests need the URL's database to be `postgres` (their own guard); rerun so, they pass 3
    of 3.
- **Real-PG prefix** `code_owner_tests::` `--ignored`: 44 passed, 0 failed (after the Parallel kill test was added;
  fmt, clippy and doc rerun clean on a cold build).
- **CI guard** `check-worker-ignored-tests.mjs`: 134 ignored, 120 run by CI invocations, 14 allowlisted, 0 run by
  nothing.
- **Main.** `go vet ./...` is clean. `go test -p 1 ./internal/infra/storage/ ./internal/infra/db/repos/` passes.
- **Faults gate** passes.
- **Agent runtime** (`cd libs/rust && cargo test --offline --locked -p elitea-agent-runtime`): 1636 passed, 0 failed,
  1 ignored (lib 1633, plus 2 + 1 in the other targets). This change does not touch `libs/rust`.
- Every build directory was removed with `cargo clean` after its step.

## Performance

- **Main.** Per saved-child visit, the transaction adds:
  - one bounded YAML parse of the root source, memoized per registration (R8);
  - one indexed read of the capture and definition, `FOR SHARE`, by primary key;
  - the existing captured-source read;
  - one digest over at most 1 MiB.

  No new round trip outside the transaction.
- **Worker.** One provider allocation per use at compile time, and one string strip per visit. Admission adds O(Code
  nodes) per child at bind.
- **Not measured.** The +30 ms p99 per-child budget is W3-2a's, and the saved child's Code path is the same
  `RemoteCodeRuntime` path with one more provider call. A dedicated measurement is a follow-up (FU-320, registered).

## Durability

- Each saved-child Code attempt is journaled before its first effect, under its saved pipeline's thread and visit.
- Completed attempts replay. In-flight ones re-attach to their own identity.
- The root reclaim's walk admits a Code leaf only where that journal decides.
- Main's visit and intent rows are idempotent across a takeover (the takeover test).

## Resilience

- Bounded inputs:
  - the scope at most 4 KiB, decoded strictly;
  - threads at most 512 bytes;
  - fan-out at most 64 children;
  - the definition at most 1 MiB;
  - one level of saved child.
- Typed failures:
  - `code.registration_refused` with its stage (Main);
  - `pipeline.code_authorization_failed`;
  - `pipeline.code.deadline_insufficient`;
  - `pipeline.node_recovery.child_effect_unknown`, now also as an Agent member's failure code;
  - `graph.fanout.nested_saved_code`.

## Security

- **Authorization fails closed inside the effect transaction.** It runs after `lockAccess` locks the claim and job
  rows, in the same transaction as the visit, intent or read.
  - The scope is a selector, never a grant. Every field is compared against Main's own state: the claim access, the
    root input, the root's frozen source and the capture.
  - The capture alone is never accepted (B1).
- **Queries.** Parameterized SQL only. The capture join is `FOR SHARE`, so project deletion waits.
- **Parsing.** Untrusted structure is bounded: strict JSON at most 4 KiB, and the existing bounded YAML parsers.
- **Logs.** Only stage names and ids are logged.
- **Not applicable:** egress, credentials, URL building, Web rendering, config.
- **Checked against `rules/security.md`, item by item.**

  | Item | Applies? | How it is met |
  |---|---|---|
  | Trust and identity | yes | Identity comes only from the locked claim. |
  | Object-level authorization | yes | B2/B3 matrix, and the foreign-project unit test. |
  | Input bounds and amplification | yes | 4 KiB, 512 bytes, YAML node and depth limits. |
  | Injection | yes | Parameterized SQL. |
  | Egress and SSRF | no | |
  | Secrets | no | |
  | Supply chain | no | No dependency change. |

- **No new routes**, so the §3a authorization matrix does not apply. The visit, intent and Supervisor routes keep
  their checks; this change only adds a resolver behind them.

## Recovery guarantee rows (rules §2)

| Component × phase | Class | Enforced by | Proven by |
|---|---|---|---|
| Worker kill × saved child's Code, root case | R (re-attach to own identity) | visit-scoped journal (`compiler.rs:1902`), walk leaf (`session.rs:2028`, `:1969`) | `real_pg_a_saved_child_code_at_the_root_survives_worker_replacement`; two-process `postgres_a_saved_child_lost_inside_*_is_recovered_by_its_journal`; Main `TestATakeoverReRegistersSavedChildCode…` (a takeover re-attaches) |
| Supervisor kill × saved child's Code, root case | I (lost runtime re-run once, new identity) / R | the journal's runtime-lost retry (FU-214) | `real_pg_a_supervisor_losing_a_saved_child_code_attempt_at_the_root_reruns_it_once` |
| Platform-client Code item killed after dispatch, root case | C (reconciled from the owner journal, never re-run) / F `child_effect_unknown` without owner proof | owner query, visit rule | `real_pg_a_saved_child_platform_client_code_at_the_root_is_reconciled_never_rerun` |
| Worker kill × saved child's Code in a Map member (depth 2) | R | frozen occurrence + the member's family journals (`branch_journal_resolver`) | `real_pg_a_map_of_saved_children_with_code_survives_worker_replacement` (killed at 4 submissions: every started identity kept, 8 identities, 8 resolved rows); Main takeover test |
| Worker kill × saved child's Code in a Parallel branch (depth 2) | R (completed attempts replay, in-flight re-attach) | as above, member kind `parallel_branch` | `real_pg_a_parallel_of_saved_children_with_code_survives_worker_replacement` |
| Supervisor kill × depth 2 | I / R | as above | `real_pg_a_supervisor_losing_saved_child_code_in_a_map_reruns_each_once` |
| Platform-client item killed after dispatch × depth 2 | C / F (member failure names `child_effect_unknown`) | as above, `parallel.rs:2686` | `real_pg_a_saved_child_platform_client_code_in_a_map_is_reconciled_never_rerun` |
| Main × saved-child Code visit, intent and read authorization | fails closed | `code_source_saved_child.go:73` | B2/B3 tests |
| Worker × Code in a saved pipeline below another | F before start (typed, readable, nothing runs) | `composition.rs:392` | `code_below_a_second_saved_pipeline_is_refused_by_its_path` |
| Worker × Router loop beyond the Q2 sum | F per attempt (typed, before effect, never overruns) | W3-1 per-attempt admission | B6 test |

**No shape is admitted as F where R/I/C is possible.** The F entries are the W3-2a "no owner proof" case, a
pre-start refusal, and a per-attempt deadline. A Parallel branch running a saved pipeline with Code is admitted:

- **Main**: its depth-2 registration is proven (`…InAFanoutMember…`, branches 0 and 1).
- **Worker**: a Worker kill mid-Parallel is proven (`real_pg_a_parallel_of_saved_children_with_code_survives_worker_replacement`):
  completed attempts are never dispatched again, in-flight ones re-attach to their own identity, and each branch
  lands one answer under its own `parallel_branch` member scope.

The recovery walk refuses no shape this change admits:

- A saved child at the root parked on Code is now a leaf.
- A fan-out member has no walk; it resumes from its frozen occurrence.
- Deeper shapes are refused before start.

## Not done, limits

- **Real Main in the Worker.** No harness drives the Rust Worker against the Go Main (FU-321, exit work, under
  PLAN-REMAINING 0.6). B5's halves are proven separately: the Worker's real-PG tests use W3-2a's fake Main and
  Supervisor, and Main's real-PG tests use the production repository path with the Worker's request shapes. Both
  halves agree on the wire shape: the Worker test asserts every field Main resolves. The end-to-end proof is the
  browser pass.
- **Agent as tool.** An ordinary root Agent (not a pipeline) calling a saved pipeline as a tool still has no owner
  scope for that pipeline's Code, which fails closed as before (FU-319, registered).
- **Real-browser evidence: pending (orchestrating session).** Cases:
  - the ETL `silver_partition` Map, with one partition routed to the error output;
  - a Worker kill and a Supervisor kill;
  - main-chat reloads;
  - the root case (an Agent node running the saved pipeline with Code).

## Follow-ups

- **FU-316:** `RegisterSavedChild` is either wired or deleted. It must not stay a dead authority path.
- **FU-317:** a Map or Parallel of Code inside a saved child (`graph.fanout.code_below_root`), after FU-239.
- **FU-318 (registered, B4):** Code in a saved pipeline below another saved pipeline, proved through the chain from the
  root down. Today it is refused, `graph.fanout.nested_saved_code`.
- **FU-319 (registered):** an owner scope for Code in a saved pipeline run as an ordinary Agent's tool. The child thread
  is `<root>/static-v1:…`, and the edge is the tool call lineage.
- **FU-320 (registered):** measure the saved-child Code overhead against W3-2a's +30 ms p99 budget. It runs in this
  PR only if the browser pass shows a p99 regression; otherwise it waits for the polish pass.
- **FU-321 (exit work, PLAN-REMAINING 0.6):** a real Worker↔Main harness for the crash suite (#1261, Docker lane),
  shared with W3-2a.
- W3-2a's FU-310 to FU-315 are unchanged. Operator recovery of a saved child's Code, which is `child_effect_unknown`
  today, is FU-310.
