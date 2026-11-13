# Node settings core: State Modifier, Router, Split Out, Aggregate (5b-1 PR A)

Branch `feat/5b1-node-settings-core`, cut from `origin/main` `b6633e6e6`. Plan:
`handoffs/point5-wave1-20261008/5b1/PLAN.md` (approved), review with decisions D1–D15 and conditions C1–C7:
`5b1/REVIEW-PLAN.md`. PR B (Code: retry, timeout, journal settings layer, Main owner-recovery identity) is a
separate PR and is not in this one.

## 1. Business behaviour

### Taken from n8n (the reference for per-node settings; the current EliteaUI has no per-node retry or on-error UI)

| n8n setting | Here | Families in PR A |
|---|---|---|
| Retry On Fail, Max Tries, Wait Between Tries | `retry_on_fail`, `max_tries` (2..=5), `wait_between_tries_ms` (100..=300000, default 1000) | refused (`retry_not_allowed`): the bodies are pure and synchronous, nothing is transient. Code gets them in PR B. |
| On Error: Stop Workflow / Continue / Continue (using error output) | `on_error: stop \| continue \| error_output` + `error_transition` | admitted |
| Always Output Data | `always_output_data` | admitted |
| Execute Once | `execute_once`, labelled **"Run once per execution"** | admitted |
| Timeout | `timeout_ms` (1000..=900000) | refused (`timeout_not_allowed`, D9): synchronous CPU cannot be pre-empted |

### Deliberately different from n8n and from the brief

- **Execute once (D8).** n8n's "Execute Once" runs a node on the *first input item only*. A pipeline node does not
  iterate over items, so that meaning does not exist here. "Run once per execution" means: the node's body runs on
  its first visit in a turn; a later visit in the same turn runs no body, changes no state and takes its normal
  transition. The brief's "reuse the stored first result" is replaced: no copy of the result is stored, outputs keep
  their current values.
  - **A Router replays its first route.** A Router has no output to keep; its result *is* its route. So a skipped
    revisit takes the route its first visit took (recorded in the run-once set next to its id,
    `node_settings_runtime.rs` `mark_once`), not the default and not a fresh evaluation of the condition. A Router
    loop whose first visit chose to loop again therefore keeps looping until the step bound stops it
    (`a_run_once_router_takes_its_first_route_again`). Accepted by the orchestrator (fix round 1, decision 3).
- **Always output data on success (accepted design, delta review).** n8n's "Always Output Data" emits an empty item
  when a node produces none. Here, on a success the layer fills with a neutral value only an output the body wrote
  as null; an output the body left out keeps its state and is never reset. On a loop revisit that means the value
  from the previous visit stays (a State Modifier that writes only its first output never clears the others). On a
  settled failure every declared output gets its neutral value.
- **Wait minimum 100 ms (D10).** n8n allows 0. Unspaced retries amplified the FU-302 outage.
- **Error output (D5).** The failure is one `node_errors` entry plus a `goto(error_transition)`; there is no
  `<node>.error` key and no second output payload. The handler reads `node_errors[-1]`. The legacy Code
  `failure_handler` is unchanged.
- **Failures as results (§8.7).** A continued failure is never published as a success: it is always recorded in
  `node_errors`, and `always_output_data` writes neutral values (`""`, `[]`, `{}`, `0`, `false`, null) only on
  explicit opt-in (risk 4).
- **Not ported.** No silent coercion: a malformed block, an out-of-range value or a setting the family cannot honour
  refuses the document on save (Main, Web) and at compile (Worker) with the same readable text. No settings on
  fan-out-owned nodes (5b-5), on LLM/Decision/Toolkit/MCP/Agent/Parallel/Map/Code (later slices) or on pauses.

## 2. Changed paths

| Tree | Path | Owner of |
|---|---|---|
| contracts | `libs/proto/contracts/pipeline-node-settings-v1.json` | keys, bounds, family table, refusal texts |
| contracts | `libs/proto/contracts/pipeline-node-settings-v1-vectors.json` | 45 shared admission vectors (all three layers) |
| contracts | `libs/jsonschema/runtime/v1/pipeline-node-error-entry.schema.json` + 5 fixtures | the `node_errors` entry (D4) |
| agent-runtime | `libs/rust/agent-runtime/src/graph/node_settings.rs` (+ `_tests.rs`) | the one policy type: parse, bounds, family table, entry, neutral values, run-once set, digest |
| agent-runtime | `libs/rust/agent-runtime/src/graph/router.rs:127,213` | `default_route()` accessor |
| agent-runtime | `libs/rust/agent-runtime/src/graph/fanout/plan.rs:319` | `safe_message` crate-visible (reused, not copied) |
| Worker | `src/agents/graph/node_settings_definition.rs` | compile admission (extract, declare `node_errors`, admit, digest) |
| Worker | `src/agents/graph/node_settings_runtime.rs` | `SettingsNode<N>` run-time layer |
| Worker | `src/agents/graph/compiler.rs:1040,1043,1089,1856,1989,2770,3496,3558,3780`; `:950` (C4), `:1754` (D3) | wiring |
| Worker | `src/agents/session.rs:3129`, `src/agents/pipeline.rs:2876` | execution id plumbing |
| Worker | `src/agents/graph/application.rs:241`, `src/agents/runtime.rs` | `variable_keys()`, error classification |
| faults | `libs/go/faults/registry/worker.json` (+ generated `codes_gen.go`, `docs/errors/README.md`) | 7 codes |
| Main | `services/elitea-main/internal/domain/pipelinelimits/node_settings.go`, `limits.go:138,175` | save/start check |
| Main | `services/elitea-main/internal/runtimecontracts/schemas_test.go:28` | entry schema held to the strict lint |
| Web | `apps/elitea-web/src/features/pipelines/lib/nodeSettings.helpers.ts` | contract mirror + admission issues |
| Web | `.../lib/flow-editor/helpers/nodeErrorRoute.helpers.ts`; `useConnectNodes.ts`, `deletionOperations.helpers.ts`, `parsePipelineTraversal.helpers.ts`, `flowEditor.helpers.ts` | error handle edge (C6) |
| Web | `.../ui/settings/NodeSettings.tsx`, `.../ui/nodes/NodeErrorOutputHandle.tsx`; `StateModifierNode.tsx`, `RouterNode.tsx`, `ExtensionNodeShell.tsx` | panel and handle |
| Web | `.../lib/graphAdmission.helpers.ts`, `graphAdmission.types.ts`, `graphShapingAdmission.helpers.ts`, `runtimeContract.constants.ts` | admission wiring, reserved run-once key |
| Web | `.../ui/state/runStateValues.ts`, `RunStateDialog.tsx` | "Failed · continued" (D12) |
| Web | `apps/elitea-web/src/shared/i18n/en.json` | 9 keys (`i18n-backfill --check` OK) |

No new dependency in any tree.

## 3. Design as built

### Contract and admission

- One block per node, `settings:`, stripped from the raw node **before** the strict family parser
  (`node_settings_definition.rs:63`), so every family keeps `deny_unknown_fields`. It runs before the recovery
  extract, so it sees whether the node also declares `recovery:`/`failure_handler:` (D1, `with_recovery`).
- Parse (`node_settings.rs` `NodeSettings::parse`): strict serde (`deny_unknown_fields`), scalar types, then the
  bounds and couplings in a fixed order. Family admission (`check_family`) is a separate step against the contract
  table. The Worker, Main (`node_settings.go:219,287`) and Web (`nodeSettings.helpers.ts` `graphNodeSettingsIssues`)
  check in the same order and stop at the first refusal, so the three layers name the same refusal for one
  document. The shared vectors prove it (45 of 45 in every layer).
- Every refusal is `Node <id>: <text>` (document-level: `<text>`), Worker code `pipeline.node_settings.invalid`,
  Main `PIPELINE_NODE_SETTINGS_INVALID` (400, `Refusal()` recognises it, `Check` and `CheckStart`).
- **C5:** the Router check reads whether the raw YAML names `default_output` (`node_settings_definition.rs:87`),
  never the parsed value (the parser fills `END` when absent). Main checks the raw key (`node_settings.go:318`), the
  Web checks `node.default_output === undefined`. Vector "router without default_output refuses continue (C5)"; the
  Worker test `a_router_without_an_explicit_default_refuses_continue` also shows an explicit `default_output: END`
  is admitted.

### `node_errors` (D2, D3, C3, C4)

- Declared only when some node has `on_error != stop` (`node_settings_definition.rs:114`): `list`, default `[]`,
  reducer `append`. A user's own `node_errors` must then declare `reducer: append` explicitly (fix round 1,
  decision 2: a list without a reducer is refused with a text that names the fix); with no continuing node it is
  never checked (**C3**; Worker `a_user_node_errors_key_is_checked_only_when_a_node_continues`,
  `a_user_node_errors_needs_an_explicit_append`, vectors "node_errors as a string without a continuing node (C3)",
  "... as a list without reducer: append (decision 2)" and the two refused ones). Not globally reserved.
- **D3:** a nested child that owns `node_errors` is exempt from the X3 typed-reducer refusal
  (`compiler.rs:1754`); any other typed reducer in the child is still refused (Worker
  `a_child_that_owns_node_errors_compiles_nested_and_hides_its_list`).
- **C4, both directions:**
  - a document that owns `node_errors` refuses an Agent node that maps `node_errors` in (`input_mapping`) or out
    (`output`) — Worker `node_settings_definition.rs:161`, Main `node_settings.go`, Web `nodeErrorsIssue`; vectors
    "node_errors out of a nested pipeline (C4)" and "node_errors into a nested pipeline (C4)";
  - a child that owns `node_errors` removes it from its mappable variables (`compiler.rs:950`), so a parent mapping
    into it is refused typed by the existing `InvalidTask` check (`application.rs` resolve) and the child's entries
    are never mapped back out: the parent sees the child's result or typed failure only.

### Settings layer (deterministic families, §3.2)

- `SettingsNode<N>` (`node_settings_runtime.rs`) wraps a State Modifier, Router, Split Out or Aggregate only when its
  settings are non-default (`compiler.rs:1989` `bind_settled`). Every other node binds exactly as before.
- A body error is settled only when it is the node's own work failing (`settled_by_settings`,
  `node_settings_runtime.rs:40`): `NodeExecutionFailed`, or `Other` that is not a `pipeline.node_recovery.*`,
  `pipeline.node_errors.*`, `pipeline.node_settings.*` or `graph.*` code. Interrupts, checkpoint, recursion,
  channel and node-timeout errors always propagate (D6; test `only_a_body_failure_is_settled`).
- `continue`: one entry appended, declared outputs untouched or neutral (`always_output_data`), the normal
  transition; a Router writes its default label to `router_output` and takes `default_output`. `error_output`: the
  same entry plus `goto(error_transition)`. One node output, so one super-step update, checkpointed by the graph.
- The entry (`node_settings.rs` `NodeErrorEntry::to_value`, schema `pipeline-node-error-entry.v1`):
  `node`, `node_type`, `status: failed`, `handled`, `error{code, message_safe, support_ref}`, `retryable: false`,
  `attempts: 1`, `input_step`. Code and sentence come only from the registry (`family_failure`), so a live and a
  replayed entry are byte-identical; `input_step` references the input checkpoint step. The AR test
  `an_entry_matches_the_schema_fixture_byte_for_byte` pins the bytes to the canonical schema fixture.
- `support_ref = support_ref_for(execution_id \0 node \0 step)` (`node_settings.rs` `node_error_support_ref`).
- The 65th entry fails the node typed `pipeline.node_errors.exhausted` with one WARN line; nothing is dropped.

### Execute once (D8) and the execution id (design point for review)

- **Where a node reads the execution id.** It does not, today. `ExecutionConfig` carries only `thread_id`, metadata
  and the parent context (`adk-graph 2.3.0 src/node.rs:20-36`); the Worker's execution id lives in the session plan
  (`session.rs` `plan.execution_id`) and the claim-scoped context (`transport/runtime_context.rs:359`), and LLM nodes
  reach it through their factory (`pipeline.rs:1900`). **Smallest plumbing (done):** an
  `execution_id: Option<Arc<str>>` on `PipelineNodeRuntimes` (`compiler.rs` `with_execution_id`), set at the root
  (`session.rs:3129`, `plan.execution_id`) and for saved children (`pipeline.rs:2876`, the claim context). Only the
  settings layer reads it (support reference and operator line); without it (tests, debug compile) the thread id
  scopes the support reference. The execution id is stable across a claim takeover (the generation changes), so a
  replayed entry is byte-identical.
- **The run-once set does not need the execution id.** A turn's graph state is already per execution: the turn
  checkpointer gives a fresh turn only its own execution's frontier and a continuation the frontier an earlier
  execution left (`libs/rust/agent-runtime/src/graph/turn_checkpointer.rs:13-17`). So the set
  `__elitea_executed_once_v1 = {"nodes": {id: route|null}}` lives in graph state, overwrite channel, declared only
  when some node runs once, reserved in the Worker (`compiler.rs:3496`) and the Web. Scope: a turn and its
  continuations (a HITL resume is the same turn), a saved child's own visit (its own thread). ≤ 128 ids (= the node
  bound). The plan's `{execution_id, nodes}` shape was simplified to `{nodes}` for this reason (deviation 1).
- A Router records the route of its first visit, so a skipped revisit takes it again; other families take their
  declared transition.

### Digest

- `settings_digest` covers only non-default blocks (by node id, length-prefixed, domain
  `elitea.pipeline.node-settings.v1`) and `bind_digest` is the identity for `None`
  (`compiler.rs:3558`). An all-default block is the same document as no block (Worker
  `documents_without_settings_keep_their_digest`). The existing golden digest tests (state reducers, recovery,
  static pause) are unchanged and green.

### Web (§3.9)

- "Settings" section after the node fields of State Modifier, Router, Split Out and Aggregate, collapsed by default:
  On error (`SingleSelect`, as `MapSettings`), Always output data and Run once per execution (`BaseSwitch` rows, as
  `CommonInterruptSettings`). Retry and timeout rows are not shown for these families (contract table). A setting
  returned to its default leaves no `settings:` key (`writeNodeSettings`; `batchUpdateYamlNode` deletes
  `settings: undefined`).
- Inline refusals come from `graphNodeSettingsIssues` through the existing `NodeAdmissionIssues` on the node card.
- Error handle: `CustomHandle` `node_error_output`, labelled "Error output", shown only for `on_error: error_output`,
  placed like the Router's default output. **C6:** connecting writes only `settings.error_transition`
  (`useConnectNodes.ts` first dispatch rule, `connectErrorRoute`), never `transition`; deleting the edge clears
  `error_transition` (`processEdgeDeletion`), and the panel then names the missing route; deleting the target node
  clears it (`handleNormalNodeDeletion` → `clearDeletedErrorTarget`). The flow builder draws the edge from YAML
  (`parsePipelineTraversal.helpers.ts`).
- `node_errors` is treated as a declared list in the editor when a node continues (D2), so a Router reading it is
  not flagged.
- D12: the run-state dialog marks a step whose node has a `node_errors` entry with the existing failed step styling
  and the tooltip "<node> · Failed · continued" (`continuedFailureNodes`). No new protocol event.

## 4. Conditions C1–C7 (as they apply to PR A)

| Condition | PR A status | Where | Proof |
|---|---|---|---|
| C1 timeouts vs deadlines | The deterministic families refuse `timeout_ms` (`timeout_not_allowed`), so no node timeout exists in PR A. The run deadline, deadline admission and `ElapsedLimit` are never continued: a `NodeTimedOut`, recursion, checkpoint or interrupt error propagates (`node_settings_runtime.rs:40`). Rule recorded for PR B: a node's own `timeout_ms` expiry is a node failure (`AttemptTimeout`, `node_recovery.rs:35,58`) that `continue`/`error_output`/retry apply to; a timeout with unknown effects goes to `Reconcile` (`node_recovery.rs:350-375`), never another try. | `node_settings.rs` `check_family`; vectors "timeout 1000 passes the bound" (refused `timeout_not_allowed`) | `only_a_body_failure_is_settled`, vectors |
| C2 retry conditions | No family admits retry in PR A. Table below for PR B. | — | — |
| C3 user `node_errors` | done | `node_settings_definition.rs:114`, `node_settings.go`, `nodeErrorsIssue` | Worker `a_user_node_errors_key_is_checked_only_when_a_node_continues`; 5 vectors; Main `TestNodeSettingsLeaveDocumentsWithoutBlocksAlone` |
| C4 nested mapping both directions | done | `node_settings_definition.rs:161`, `compiler.rs:950` | Worker `a_continuing_parent_never_maps_node_errors_into_or_out_of_a_child`, `a_child_that_owns_node_errors_compiles_nested_and_hides_its_list`; 2 vectors |
| C5 Router continue needs an explicit default | done, raw key in all three layers | `node_settings_definition.rs:87,202`, `node_settings.go:318`, `nodeSettings.helpers.ts` `NODE_CHECKS` | Worker `a_router_without_an_explicit_default_refuses_continue` (key absent, parser fills END); 3 vectors |
| C6 error handle | done | `nodeErrorRoute.helpers.ts`, `useConnectNodes.ts`, `deletionOperations.helpers.ts` | Web `nodeErrorRoute.helpers.test.ts` (5 tests) |
| C7 Main first | upgrade note `docs/upgrading/2026-11-11-0-node-settings-core.md` leads with it | — | — |

### C2: retry conditions (for PR B; nothing is retried in PR A)

`settings.retry_on_fail` compiles into `NodeRecoveryPolicy` with the four `RetryCondition`s
(`libs/rust/agent-runtime/src/graph/node_recovery.rs:88-93`). It never widens what an explicit `recovery:` may retry,
because `plan_after_failure` stays the only authority and runs its effect checks before any retry condition:

| Condition | Admitted | Why (path:line) |
|---|---|---|
| `DependencyUnavailable` | yes | maps from `NodeFailureClass::DependencyUnavailable` only (`node_recovery.rs:56`); a retry still needs `NoExternalEffect` or an uncommitted idempotent effect (`:372-373`) |
| `RateLimited` | yes | `:57`; same effect gate |
| `AttemptTimeout` | yes | `:58`; a timed-out job with an unknown or unclassified effect is `Reconcile` (`:350-370`), never retried (C1) |
| `WorkerInterrupted` | yes | `:59`; same effect gate |
| every other class (`InvalidInput`, denials, `Cancelled`, `LeaseLost`, `RuntimeLost`, `Unknown`, …) | no | `retry_condition()` is `None` (`:60-70`); control stops end first (`:344-346`), denials stop (`:347-349`); `RuntimeLost` keeps its own allowance (FU-214) |

## 5. Tests (counts)

Failing-first evidence: `handoffs/point5-wave1-20261008/5b1/evidence/failing-first-{worker,main,web}.txt`.
- Worker: with the settings layer unbound (a temporary `FAILFIRST_5B1` toggle, removed before commit), 7 of 19
  failed (every run-time behaviour); admission tests need the new types and cannot compile without them.
- Main: with `checkNodeSettings` unwired, the vectors and the typed-refusal test failed.
- Web: with the wiring files reverted to `HEAD`, 39 of 58 failed (vectors, C6, D2).

| Suite | Command | Result |
|---|---|---|
| agent-runtime lib | `cargo test --offline --locked -p elitea-agent-runtime --lib --all-features` | 1856 passed, 0 failed, 1 ignored (pre-existing); 16 new |
| Worker lib (PG 18, CI env: receipts required, disposable guard) | `cargo test --offline --locked --lib --all-features` | see §5a; 23 new (`node_settings_tests` 21, `node_settings_pg_tests` 2) |
| Worker real PostgreSQL | `node_settings_pg_tests` (two processes per scenario: `after`, `before`, `once`) | 3 of 3 scenarios |
| Main | `go test -race ./internal/domain/pipelinelimits/ ./internal/runtimecontracts/`, `go vet` | ok; 4 new tests, 45 vectors |
| faults gate | `cd libs/go/faults && go test -count=1 ./...` | every package ok |
| Web | `npm run test:unit`, `npm run typecheck`, `npm run lint`, `node scripts/i18n-backfill.mjs --check` | see §5a |
| Clippy, fmt | Worker `--all-targets --all-features -D warnings`; agent-runtime `-p elitea-agent-runtime --all-targets --all-features -D warnings`; `cargo fmt --check` both | clean |

### 5a. Final counts

After merging `origin/main` at `9c8069fa0` (#1937 FU-291 had not merged yet; merge it when it lands):

| Suite | Result |
|---|---|
| agent-runtime lib, `--all-features` | 1856 passed, 0 failed, 1 ignored (pre-existing) |
| Worker lib, `--all-features`, PostgreSQL 18 (disposable container), CI env | 3248 passed, 0 failed, 134 ignored (all pre-existing; this branch adds no `#[ignore]`) |
| Worker node settings | 23 of 23 (21 unit, 2 real PG: parent and child process) |
| Worker clippy `--all-targets --all-features -D warnings`, fmt, `cargo doc --no-deps --all-features` with `RUSTDOCFLAGS=-D warnings` | clean |
| agent-runtime clippy and rustdoc | clean |
| Main `go vet`, `go test -race` (`pipelinelimits`, `runtimecontracts`) | ok |
| faults gate `go test ./...` | every package ok |
| Web `npm run test:unit` | 1950 files, 19115 passed, 6 expected fail, 1 skipped (pre-existing). One earlier run had one failure in `src/widgets/chat-box/ui/ChatBox.stickyTarget.test.tsx` (not touched here); it passed alone and in this full run. |
| Web typecheck, lint, `i18n-backfill --check` | clean |

The ignored-test guard script (`scripts/ci/check-worker-ignored-tests.mjs`) needs the `yaml` package, which is not
installed at the repository root locally; checked instead by grep: no `#[ignore]` added.

## 6. Performance

| Budget (PLAN §9) | Mechanism | Proof / measurement |
|---|---|---|
| No settings: zero new work, same digests, no new channel | non-default only: no wrapper (`compiler.rs:1989`), no `node_errors`/run-once channel (`compiler.rs:1856`, `node_settings_definition.rs:114`), identity digest (`node_settings.rs` `bind_digest`) | `documents_without_settings_keep_their_digest`; existing golden digest tests unchanged. The 128-node compile bench guard was **not** added (follow-up FU-347). |
| Settled failure: 0 extra DB round trips | one node output in the step's own update | `a_continued_failure_writes_no_extra_checkpoint`: a continued failure writes as many checkpoints as the same run succeeding |
| Wrapper overhead p99 ≤ 50 µs | entry built from ids, codes and the step number, no state serialisation | not measured (FU-347) |
| Run once: ≤ 128 ids, 0 body work on a skip | `mark_executed_once` bound; skip returns an empty output | AR `the_run_once_set_holds_128_ids_and_refuses_the_129th`; Worker `a_run_once_node_runs_once_in_a_three_visit_loop` |
| `node_errors` ≤ 64 entries × ≤ 1 KiB | `MAX_NODE_ERRORS`, `MAX_NODE_ERROR_ENTRY_BYTES` | `the_node_errors_bound_holds_at_64_and_fails_typed_at_65`; AR entry 1024/1025 |
| Main: no extra parse, O(nodes) | `checkNodeSettings` in the existing single parse, one map lookup per node without a block | by construction (`limits.go:175`) |

## 7. Durability

| Crash window | Rule | Proof |
|---|---|---|
| Worker lost after a settled failure's step checkpoint | the entry, outputs and route are durable; the replacement never re-runs the node | real PG `after`: one entry, the same bytes as the paused checkpoint |
| Worker lost before the failing node's step | the replacement runs the pure body, settles it; the entry is a function of (scope, node, input step) | real PG `before`: one entry, support reference equal to `node_error_support_ref(thread, node, step)` |
| Worker lost after a run-once node's first visit | the run-once set is in the same checkpoint | real PG `once`: 3 visits across two processes, body ran once |
| Version skew | Main first (C7); an old Worker refuses `settings:` typed (strict family parser), an old Main stores it unchecked but the new Worker refuses invalid blocks at compile | upgrade note |

## 8. Resilience

- Every bound has a named constant, a limit and a limit+1 test in every layer that mirrors it (AR, vectors in
  Worker/Main/Web).
- Typed failures only: `pipeline.node_settings.invalid` at compile, `pipeline.node_errors.exhausted` at run time;
  integrity and control errors are never continued.
- A malformed `node_errors` channel counts as full, so it is never appended to (`node_error_count`).
- No silent coercion: a block the family cannot honour refuses the document.

## 9. Security

| Category (`rules/security.md`) | Applies | How checked |
|---|---|---|
| Identity / trust boundaries | no new identity; settings add no identifier and no authority | — |
| Object-level authorization | `error_transition` can only name a node of the same document outside any fan-out, or END | Worker `every_admission_refusal_is_typed_and_names_the_node` (self, unknown, Map-owned); vectors |
| Input bounds and amplification | the block is inside the existing 512 KiB document cap and the YAML expansion budget; strict keys, scalar types | AR `malformed_blocks_are_refused`; vectors "an unknown key", "a quoted number" |
| Injection | no SQL, URL, shell or template is built from settings | by construction |
| Egress | none | — |
| Secrets and data in logs | one INFO line per settled failure with code, node id, handled, attempts, support reference, execution id; the entry holds registry text only | Worker `a_continued_failure_logs_one_data_free_operator_line` (child-process capture: a sentinel input value and the failing template text appear in no line) |
| Effects | the deterministic bodies have none; a re-run is byte-identical | real PG `before`/`after` |
| Supply chain | no new dependency | — |

Scanners (`cargo deny`, `govulncheck`, `npm audit`) are run by the orchestrating session with the reviews.

## 10. Recovery-guarantee rows (delivery gate §2)

| Component × phase | Class | Enforced by | Proof |
|---|---|---|---|
| Main × admission (save/start with `settings`) | I (typed 400, writes nothing) | `node_settings.go:219` from `limits.go:175` | Main vectors (45), `TestNodeSettingsRefusalIsTypedAndBranchable` |
| Worker × admission (compile) | F typed, before any effect | `node_settings_definition.rs:63,144` | Worker vectors, refusal tests |
| Worker × deterministic step, fail + `continue`/`error_output` | R | `SettingsNode` one update | real PG `after` and `before` |
| Worker × run-once revisit | R | run-once set in the step checkpoint | real PG `once` |
| Worker × `node_errors` full | F typed `pipeline.node_errors.exhausted` | `node_settings_runtime.rs:115` | `the_node_errors_bound_holds_at_64_and_fails_typed_at_65` |
| Web × reload | R (settings and state from storage) | YAML round trip | `nodeErrorRoute.helpers.test.ts` round trip; browser §11 (pipelines 171 and 174 after a reload) |
| Code rows (P10 retry, wait, timeout, owner recovery) | — | PR B | — |
| Worker × settled failure inside a saved child pipeline (D15) | — | PR B (FU-348) | not claimed here: the compile and mapping rules are unit-tested only |

## 11. Browser evidence

Real-model disposable stacks with the production profile, Claude in Chrome as `admin@centry.user`. Fixtures
(pipelines) were created through the app's API; every setting change and every connection was made in the UI.

- **Stack p5b1.** Images Main, Worker and Web `p5b1-1eb1b57e2`; Web later `p5b1-9f6b8d1b0`. Verify-profile 50/0.
  | Case | Result |
  |---|---|
  | 1 `continue` + Router on `node_errors` (chat 887) | the Router read the entry and took its route |
  | 2 `error_output` (chat 889, pipeline 174) | the handler answered "pipeline.state_modifier.failed at shape" |
  | 3 run once in a loop (chat 890) | visits = 3, body runs = 1 |
  | 5 the same loop without run once (chat 891) | body runs = 3 |
  | 7 Main refusals | every invalid block 400 with the readable text; an empty `settings: {}` saves (201) |
  | W1, W2 (fixed in `3ecc056eb`, §17) | pipeline 174: the On error select shows "Continue (using error output)" before and after a reload; the Error output handle and its stored edge to `handler` render |
  | W3 / D12 | pre-existing, FU-351 (§14). Pipeline 184 (failing State Modifier `continue`, then an LLM node): the timeline shows only the LLM step; the answer "1 error was recorded." proves `node_errors` reached the LLM. D12 is unit-tested only in this PR |
- **Stack p5b1c, on the merged head `8919362c2`.** Images Main and Web `p5b1-8919362c2`, Worker `p5b1-1eb1b57e2`
  (the merge changes no PR A Worker code; Worker lib 3370 passed on the merged head). Binary checks: Main
  `PIPELINE_NODE_SETTINGS_INVALID` present; the Web bundle holds the W1/W2 fix (`t.retry_on_fail===!0`). Verify-profile
  50/0.
  - **C6, connecting the handle (pipeline 171, created without `settings:`).**
    - Choosing "Continue (using error output)" shows the inline refusal "needs an error transition" and the Error
      output handle.
    - Main refuses a save of that incomplete block (HTTP 400 "Node shape: On error 'error output' needs an error
      transition.", checked through the API).
    - Dragging the handle onto `handler` clears both refusals. The saved YAML holds `settings: {on_error:
      error_output, error_transition: handler}`, with `transition: done` and every other node unchanged.
    - After a reload, the error edge and `shape → done` are drawn and the select shows the stored value.
    - A run answers "handled: pipeline.state_modifier.failed".
- Both stacks are torn down with their volumes.

## 12. Fixtures

All test fixtures are inline YAML or the shared vectors file; nothing was created through the UI or the database.

## 13. Deviations from the plan and follow-ups

1. Run-once set shape `{nodes: {id: route|null}}` without `execution_id`: the turn checkpointer already scopes graph
   state to the execution (§3). A Router records its route so a skip is deterministic.
2. Registry codes were added in the commit that first raises them (the faults gate refuses unused codes), not in
   commit 1.
3. The Web settings section uses a text button and `Collapse` (no new visuals); it is placed after the node fields,
   not after `CommonInterruptSettings` (State Modifier, Router and the shaping nodes render none).
4. Not done in PR A: the 128-node compile bench guard and the wrapper p99 measurement (FU-347); a real-PG test of
   a settled failure inside a saved child (D15; the compile and mapping rules are unit-tested) (FU-348).
5. Main reads Map/Parallel ownership from direct references (worker, branches); the Worker's ownership walk is the
   authority for exotic documents.

## 14. Fix round 1 (orchestrator review of `a9fcc41e1`)

Evidence: `handoffs/point5-wave1-20261008/5b1/evidence/round1-failing-first.txt` (tests and vectors written before
the fixes; every listed test failed first). Merged `origin/main` `94862584d` (#1937) first.

### Decisions

| | Change | Where | Proof |
|---|---|---|---|
| 1 | An empty `settings: {}` is a no-op on every family (no wrapper, same digest); any key on a family without settings is refused `family_not_yet`, even at its default value | Worker `node_settings_definition.rs` `extract`; Main `checkNodeSettings`; Web `parseBlocks` | `an_empty_block_is_a_no_op_on_every_family`; vectors "an empty block on a Code node / HITL node is a no-op", "a default value on a Code node is refused" |
| 2 | A user-declared `node_errors` needs `reducer: append` once a node continues; new text "When a node continues on error, a declared node_errors must be a list with reducer: append." | Worker `declare_node_errors`; Main; Web `nodeErrorsIssue`; contract | `a_user_node_errors_needs_an_explicit_append`; vector "... without reducer: append (decision 2)" |
| 3 | Router replay of its first route documented next to "Run once per execution" | §1 | — |
| 4 | FU-5B1-A → FU-347, FU-5B1-B → FU-348 (both land in PR B); D15 marked "PR B" in §10 | this file | — |

### Code-review findings

| # | Verdict | Fix | Failing-first test |
|---|---|---|---|
| 1 | confirmed: the static pause catalog's successors left out `error_transition` | `compiler.rs` `static_pause_catalog` adds the error route (and END) | `a_pause_after_a_routed_failure_admits_the_error_route` |
| 2 | confirmed: on success `fill_outputs` overwrote a State Modifier's other declared outputs | fills only keys the body wrote as null (`node_settings_runtime.rs` `fill_outputs`) | `always_output_data_never_overwrites_state_on_success` |
| 3 | confirmed: renaming a node did not update `settings.error_transition` | `NodeCardHeader.rename.ts` `errorRouteRename` | Web `renaming an error route target` |
| 4 | confirmed: `error_transition` was not normalized (numeric ids) and Main read only string ids | Worker `normalize_settings_route`; Main `graphIDScalar`; Web `pipelineNodeIdentifiers` rule | vectors "numeric ids: an error route to a numeric node / to itself" |
| 5 | confirmed: `node_errors` could be the fallback final answer | `compiler.rs` `result_policy` excludes it when settings own it | `node_errors_is_not_a_result_fallback` |
| 6 | confirmed for the text builder: an id that is not a graph id was echoed | `NodeSettingsRefusal::message` reads "A pipeline node" for it, as Main and the Web | AR `an_invalid_id_is_named_a_pipeline_node`. The Worker end-to-end test `a_malformed_id_is_never_echoed` passed before the fix too (the over-long id is refused earlier), so it is a regression guard only |
| 7 | confirmed: the support reference omitted the thread when an execution id was set | scope = execution id + thread (`node_settings_runtime.rs` `settle`) | `the_support_reference_names_the_thread` |
| 8 | confirmed: number widths differed (u16/u64 vs int64 vs any) | every layer: an integer within +/-(2^53-1) gets the bound's range refusal (negative included), any other number is malformed (`node_settings.rs` `count`/`bounded`, Main `optionalInt`, Web `isCount`) | AR `out_of_range_integers_are_range_refusals_in_every_width`; 6 vectors |
| 9 | confirmed: leaving error output left a stale error edge | `NodeSettings.tsx` removes the node's error-handle edges when the key goes | Web `removes the error output edge when On error leaves error output` |
| 10 | confirmed: "Failed · continued" marked every visit | `continuedFailureSteps` marks the visit whose own state appended an entry naming its node. Keyed on the append, not `input_step`: the timeline steps carry no checkpoint step (`RunStateDialog.tsx` `RunStateTimelineStep`) | Web `marks only the visit whose own state appended the entry` |

### W3 (browser pass): empty Run-details timeline for State Modifier/Router-only pipelines: pre-existing

Traced read-only on `origin/main` `2b72df86e`: the timeline gets its rows only at
`apps/elitea-web/src/features/pipelines/lib/flow-editor/helpers/parseRunsByEvent.agentHandlers.ts:35,90,133`
(LLM and tool start events) and `parseRunsByEvent.edgeHandlers.ts:52,71` (`agent_on_transitional_edge`, dispatched at
`parseRunsByEvent.helpers.ts:82`). No Worker, shared-runtime or Main code on main emits `agent_on_transitional_edge`
(no occurrence in `services/elitea-worker-rust`, `libs/rust` or `services/elitea-main`), and State Modifier and Router
emit no LLM or tool event, so such a run has no timeline row on main either. PR A does not touch that data path (no
diff to `parseRunsByEvent.*` or `useRunEvent.ts`); it only reads the rows (`continuedFailureSteps`,
`apps/elitea-web/src/features/pipelines/ui/state/runStateValues.ts`, used in `RunStateDialog.tsx`). So no PR A family
(State Modifier, Router, Split Out, Aggregate) ever gets a timeline step of its own, and `continuedFailureSteps` has
no step to mark. **W3 = pre-existing, registered as FU-351; not fixed in PR A.** Browser on p5b1, pipeline 184 (a
failing State Modifier with `continue`, then an LLM node): the timeline shows only the LLM step, and the LLM's answer
("1 error was recorded.") proves `node_errors` reached it. **D12 ("Failed · continued") is therefore proven by unit
tests only in PR A** (`runStateValues.test.ts`); there is no reachable browser proof until FU-351, or a PR B Code step
if Code emits a tool-start frame. Details in §17.

## 15. CI hygiene (fix round 1)

- **FU-349** (`postgres_a_map_item_whose_finished_pipeline_tool_was_edited_later_is_refused_typed`, from #1937).
  Cause: the crash watcher was a task on the crash process's current-thread runtime polling every 20 ms; it could
  see the finished nested thread only after the member Agent had saved its next model request (which holds the
  nested result), so the reclaim replayed that request and never met the edited thread. Fix (test only): the
  watcher polls on its own OS thread, runtime and connection every millisecond. Before 3/10 passed on this branch
  (main's baseline 2/6 failing); after 10/10 and 10/10; the whole `fanout_effect_pg_tests` module 109 passed.
  Evidence `5b1/evidence/fu349-flake.txt`. Made deterministic in the delta review (§16, D).
- **FU-345** (`ChatBox.stickyTarget.test.tsx`, "sends to, attributes the answer to, and gates attachments...").
  Root cause: the Attach row is enabled by the agent's "Allow attachments" AND the deployment's attachment types
  (`useAllowedAttachmentTypes`, `GET /elitea_core/index_types/prompt_lib/2`). The test never pinned the second, so
  the global generated MSW mock answered with faker data whose categories are each randomly present; when none
  was, the deployment "accepts no attachment types" and the row stayed disabled whatever the agent allows (traced:
  the agent gate had opened, the row read `noFileTypesAvailable = true`). Not a UI race; the component is
  unchanged and the assertion stays strict. Fix: the test pins a deterministic answer. Before 15/20 local runs;
  an empty answer pinned fails 3/3; after 20/20.

### Counts after fix round 1 (branch merged with `origin/main` `94862584d`)

| Suite | Result |
|---|---|
| agent-runtime lib `--all-features`; clippy, fmt, rustdoc `-D warnings` | 1860 passed, 0 failed, 1 ignored (pre-existing); clean |
| Worker lib `--all-features`, PostgreSQL 18 (disposable container), CI env (receipts required, disposable guard); clippy, fmt, rustdoc `-D warnings` | 3310 passed, 0 failed, 147 ignored (all from main); clean. `node_settings*` 30 of 30 |
| Worker ignored-test guard (`scripts/ci/check-worker-ignored-tests.mjs`, run from a scratch copy of `scripts/ci` with its own `npm ci`) | 147 ignored: 133 run by 10 CI invocations, 14 allowlisted, 0 run by nothing, 0 errors |
| Main `go vet`, `go test -race` (`pipelinelimits`, `runtimecontracts`) | ok; 56 shared vectors |
| faults gate `go test ./...` | `faults` ok, `cmd/faultsgen` no test files, `grpcrecover` ok, `httperr` ok, `logx` ok |
| Web typecheck, lint, `i18n-backfill --check`, `npm run test:unit` | clean; 1950 files, 19128 passed, 6 expected fail, 1 skipped |

Scanners (no lockfile or manifest differs from `origin/main`, so every finding is pre-existing): `cargo deny
--all-features check advisories` fails on main's lockfiles alike (Worker 7 vulnerability + 4 unmaintained, libs/rust
8 + 8); `govulncheck` reports 2 Go standard-library vulnerabilities of the local toolchain (go1.26.9); `npm audit`
21 moderate. This branch adds none.

## 16. Delta review (`541653d82..fb4119fb0`)

Evidence: `5b1/evidence/round2-failing-first.txt`.

| Item | Fix | Proof |
|---|---|---|
| A (i) | `continuedFailureSteps` counts each entry once (node, input step, support reference, handling) and attributes it to the nearest step at or before its first appearance whose node is the entry's own node: an `interrupt` marker or the next node's step never takes it | Web `attributes an entry to its own node even when an interrupt marker shows it first` (failed first) |
| A (ii) | An entry whose node has no step at or before its first appearance (recorded before a resumed timeline began) is never attributed. Partial rebuttal: the previous code already attributed an older entry only to a step of its own node, so "every older entry goes to the first stateful step" held only for that node; the new test passes before and after. Residual, documented in the code: the timeline steps carry no checkpoint step, so a timeline that begins with a revisit of the very node whose failure predates it is marked; seeding from "the state before the first rebuilt step" is not possible (the dialog has none) and ignoring every entry of the first stateful step would drop a fresh run whose first node fails | Web `ignores entries recorded before a resumed timeline began` |
| B | One rule in all three layers: the Worker's legacy-label rewrite (`normalize_legacy_label`) applies to the ids, Map workers, Parallel branch nodes and `error_transition` the settings check reads. Main `legacyGraphID` in `graphIDScalar` and branch nodes; Web `legacyGraphId` in `settingsView`. `graphIDScalar`'s comment now says so. The Web's own id rule still flags a legacy label separately, as before | 3 vectors ("legacy labels: ..."): Main and Web failed first, the Worker already passed |
| C | `SettingsCatalog::owns_key` is the one predicate, used for the result terminal keys, the fallback keys, the mappable child variables and the nested reducer refusal; a node whose `error_transition` is END is a terminal result candidate like `transition: END` | `the_result_policy_reads_the_error_route_and_never_the_owned_list` (failed first) |
| D | FU-349 made deterministic. A kill when the gateway receives the Agent's next request cannot work: that request is saved before it is sent (`model_checkpoint.rs` ModelPending), and with that kill point the edited test passed 0/10. Instead the parent installs, for the crash phase only, a trigger that holds the member Agent's write of the tool result (a session event with the nested answer as a function response; its next model request is saved after it), so the finished nested thread is always durable first and the result never is; `client_connection_check_interval` rolls the held insert back when the process dies. The watcher's query error ends the process with its own code (never a busy loop); the dead `let _ = pool;` is gone | edited 10/10 and 10/10; stored-result 10/10 and 10/10; module run in the gates |
| E | `error_transition` moved below `uses_execute_once`'s doc; the stale `optionalInt` comment line deleted | — |
| Finding 4 | Accepted as design: see §1 "Always output data on success" | — |

### Counts on the merged head (`origin/main` `2b72df86e`: #1945 FU-323a and #1950)

| Suite | Result |
|---|---|
| agent-runtime lib `--all-features`; clippy, fmt, rustdoc `-D warnings` | 1861 passed, 0 failed, 1 ignored (pre-existing); clean |
| Worker lib `--all-features`, PostgreSQL 18, CI env; clippy, fmt, rustdoc `-D warnings` | 3357 passed, 0 failed, 147 ignored (all from main); clean |
| FU-349 kill tests on this head | edited 10/10, stored result 10/10 |
| Worker ignored-test guard | 147 ignored: 133 run by 10 CI invocations, 14 allowlisted, 0 run by nothing, 0 errors |
| Main `go vet`, `go test -race` (`pipelinelimits`, `runtimecontracts`) | ok; 59 shared vectors |
| faults gate | `faults` ok, `cmd/faultsgen` no test files, `grpcrecover`, `httperr`, `logx` ok (files regenerated after the merge) |
| Web typecheck, lint, `i18n-backfill --check`, `npm run test:unit` | clean; 1953 files, 19161 passed, 6 expected fail, 1 skipped |
| FU-345 test after #1950 | the `index_types` pin survived the merge; 20/20 |

Scanners: lockfiles equal `origin/main`; `cargo deny` (Worker 7 vulnerability + 4 unmaintained, libs/rust 8 + 8),
`govulncheck` (2 Go standard-library, local go1.26.9) and `npm audit` (21 moderate) are all pre-existing.

## 17. Browser pass on stack p5b1 (images `p5b1-1eb1b57e2`): Web fixes

Evidence: `5b1/evidence/browser-w-failing-first.txt` (4 tests failed first).

| | Root cause | Fix | Test |
|---|---|---|---|
| W1 the On error select showed "Stop pipeline" for a stored `on_error: error_output` | The panel read the block through the strict admission parser (`readNodeSettings` → `parseNodeSettings`); an incomplete block (`error_output` before its route is connected) is refused there, so the panel fell back to every default | `readNodeSettings` reads each field on its own (`nodeSettings.helpers.ts`); the refusal stays the admission issue's job | `NodeSettings.test.tsx` "displays On error for ..." (error output without a target, continue, an invalid block) and "shows the stored toggles even while the block is incomplete" |
| W2 no error output handle | The same read: the handle is shown only for `on_error: error_output`, which the strict read hid while the route was missing, so the route could never be connected (C6). With a stored valid route the handle and its edge already rendered in tests (`StateModifierNode.test.tsx` "with a stored target" passed before the fix; `nodeErrorRoute.helpers.test.ts` "is drawn from the error handle") | as W1 | `StateModifierNode.test.tsx` "renders the error output handle without a target yet" / "with a stored target", "renders no error output handle for continue or stop"; connecting: `nodeErrorRoute.helpers.test.ts` (C6) |
| W3 empty "Run 1 details" timeline for a State Modifier/Router-only pipeline | Pre-existing, not PR A. The run timeline gets its entries only from LLM and tool events and from `agent_on_transitional_edge` (`apps/elitea-web/src/features/pipelines/lib/flow-editor/helpers/parseRunsByEvent.helpers.ts:76-84`; entries pushed at `parseRunsByEvent.agentHandlers.ts:35,90,133` and `parseRunsByEvent.edgeHandlers.ts:52,71`). The Rust Worker emits no `agent_on_transitional_edge` at all (no occurrence in `services/elitea-worker-rust/src` or `libs/rust`), and the deterministic families emit no LLM or tool event, so such a run has no timeline step and no state to show. Populating it is a new protocol event, which 5b-1 does not add (D12) | — | — |

## 18. Merge with `origin/main` `fdab93771` (2026-11-13)

The only conflict was `en.json`, appended on both sides; both were kept. Counts on `8919362c2`:

| Suite | Result |
|---|---|
| Worker clippy `--all-targets --all-features -D warnings` | clean |
| Worker lib `--all-features` | 3370 passed, 0 failed, 147 ignored |
| Main `go build ./...`, `pipelinelimits` | ok |
| faults gate | `faults`, `grpcrecover`, `httperr`, `logx` ok; `cmd/faultsgen` no test files |
| Web tsc, oxlint, `i18n-backfill --check` | clean |
| Web pipelines + i18n vitest | 247 files, 2779 passed, 1 expected fail |
