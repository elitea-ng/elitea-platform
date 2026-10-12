# Graph typed state reducers (Point 5, Wave 1, Track B) — 2026-10-09

Brief: `handoffs/point5-wave1-20261008/T-B-typed-reducers.md` (local). Branch `feat/graph-typed-state-reducers`.

A declared pipeline state variable can choose a bounded, typed reducer in its YAML descriptor:

```yaml
state:
  findings: {type: list, value: [], reducer: append}
```

The `reducer` key is admitted in every build of Worker, Main and Web, with no feature flag. This follows the
2026-10-12 decision that Point 5 node families are on by default (`PLAN.md` §8.1). Until then the key was
rehearsal-gated; §8a keeps that first browser pass as history. Overwrite stays the default, and every overwrite-only
definition keeps its digest byte for byte.

## 1. Business behaviour and sources

- **The current platform has no user-declared reducers.** The Python SDK builds the LangGraph state class in
  `elitea_sdk/runtime/langchain/utils.py:504-534` (`create_state`):
  - every declared user variable is a plain typed channel that overwrites;
  - only `messages` (`add_messages`) and internal channels (`hitl_decisions`, `parallel_tasks`, `_auto_routing`) carry
    reducers;
  - a descriptor is read as `value['type']`, so an unknown key such as `reducer` is silently ignored.

  Typed reducers are therefore a new Point 5 capability, not a port. Their semantics follow the decision log
  (`PLAN.md` §1.2 and `experts/04-data-shaping.md` §2.3).
- **Not ported:**
  - the SDK's permissive behaviour of ignoring unknown descriptor keys: the Worker keeps `deny_unknown_fields` and
    refuses unknown reducer names;
  - ADK's built-in `Append` and `Sum`. They wrap scalars, treat `null` as "keep", and coerce through `f64` with a zero
    fallback (`adk-graph-2.2.0/src/state.rs` `apply_update`).
- **The contract:**

| reducer | Channel type | Update | Result | Refusal |
|---|---|---|---|---|
| `overwrite` (default) | any | unchanged | ADK `Reducer::Overwrite`, no digest change | none |
| `append` | `list` | must be a JSON array; a scalar is never wrapped and `null` never clears | current ++ update, in order | `graph.state.reducer_type_mismatch`; `graph.state.reducer_limit` above 10,000 elements or 512 KiB |
| `sum_int` | `int` | an exact `i64` (`2.0` and `1e2` are integers, `2.5` is not) | `checked_add` | `graph.state.reducer_overflow`; `graph.state.reducer_type_mismatch` for non-integers |
| `merge` | `dict` | must be an object | shallow merge: update keys win, and `null` sets the key to null | `graph.state.reducer_type_mismatch`; `graph.state.reducer_limit` above 1,000 keys or 512 KiB |

## 2. Changed paths and which crate owns each mechanism (ADR-0027)

### `libs/rust/agent-runtime`: shared runtime semantics

`libs/rust/agent-runtime/src/graph/state_reducers.rs`:

| Item | Line | What it is |
|---|---|---|
| `MAX_APPEND_ELEMENTS`, `MAX_MERGE_KEYS`, `MAX_REDUCED_BYTES` | `:21-25` | the named limits |
| `ReducerFailure::code` | `:50` | stable, data-free codes |
| `reduce_checked` | `:92` | the reduction |
| `check_update` | `:126` | allocation-free guard check |
| `check_held` | `:151` | defaults and held values |
| `channel_reducer` | `:173` | the infallible ADK `Reducer::Custom` |
| `integer` | `:194` | exact `i64` from `Number`'s `Display` text |
| `json_len_within` | `:204` | capped byte length, independent of member order |
| `ReducerGuard` | `:233` | the node wrapper; `execute` at `:269`, interrupted output skipped at `:271` |

Tests: `state_reducers_tests.rs`.

`libs/rust/agent-runtime/src/exact_number.rs`:
- `MAX_INTEGER_DIGITS` `:11`, `decimal` `:39`, `exact_i64` `:120`, `exact_u64` `:133`. This is the exact JSON-number
  reader, moved here from the Worker's `data_shaping.rs` so typed reducers and data shaping share one parser.
- It works on number text. The Worker passes `Number::as_str()` (zero-copy under its `arbitrary_precision`); the
  runtime passes `Display` text, which is exact without that feature too. Nothing here depends on either `serde_json`
  feature, per the crate rule in `canonical`.
- Tests: `exact_number_tests.rs`.

Registration: `src/lib.rs` (`pub mod exact_number`) and `src/graph/mod.rs` (`pub mod state_reducers`).

### `services/elitea-worker-rust`: compiler admission and wiring

`src/agents/graph/compiler.rs`:

| Line | Mechanism |
|---|---|
| `:155` | `RawStateTypeDescriptor.reducer`; the struct keeps `deny_unknown_fields` |
| `:893` | `validate_state(raw.state)`; no gate, so the key is admitted in every build |
| `:2769` (`typed_reducer`) | compile-time checks: builtins `input`/`messages` (`:2780`), known names (`:2784`), type compatibility (`:2791`), default within bounds (`:2795`); a non-string `reducer` is refused by the descriptor parse (`:2724`) |
| `:2804` (`validate_overwrite_channels`), called at `:936` | channels that must overwrite: Map destination, Parallel output, SplitOut/Aggregate output, HITL `edit_state_key`, StateModifier `variables_to_clean`, and every output, clean or edit key of a Parallel-owned or Map-owned node (their updates never pass the top-level guard) |
| `:459-464` (`PipelineGraphBuilder::node`) | the guard at the single registration point; it wraps every node only when the definition has a typed reducer |
| `:1744` | the typed ADK channel in `state_schema` |
| `:2344` (`with_initial_checkpoint`) | the initialization fix: a typed channel's default is inserted, not reduced onto itself |
| `:1407` (`compile_subgraph_with_runtime`) | nested child pipelines refuse typed reducers in v1 with `Unsupported` |
| `:949`, `:3012-3016` (`reducers_digest`) | the digest fold, only when a typed reducer exists: `elitea.graph.pipeline.state-reducers.v1\0` plus sorted, length-prefixed `(key, tag)` |

Other Worker files:
- `src/agents/graph/mod.rs`: re-exports `elitea_agent_runtime::graph::state_reducers`.
- `src/agents/graph/data_shaping.rs`: `decimal`, `exact_i64` and `exact_u64` delegate to `exact_number` with
  `Number::as_str()`. Shaping codes are unchanged (`number_code`).
- Test helpers widened to `pub(super)`, no behaviour change: `static_pause_tests.rs`, `shaping_pg_tests.rs`.
- `src/state/postgres_checkpointer.rs:33-42` and `src/agents/graph/map_reduce.rs:31`: compile-time assertions that a
  typed channel at its bound fits one stored checkpoint (`MAX_REDUCED_BYTES` ≤ `MAX_DATABASE_PAYLOAD_BYTES`;
  `MAX_APPEND_ELEMENTS` and `MAX_MERGE_KEYS` ≤ `MAX_DATABASE_JSON_NODES`) and the whole-state fan-out boundary
  (`MAX_REDUCED_BYTES` ≤ `map_reduce::MAX_CHECKPOINT_BYTES`). A drift on either side fails the build.
  - `MAX_DATABASE_MAP_ENTRIES` bounds top-level state keys, not the keys of one object, so it is not a counterpart of
    `MAX_MERGE_KEYS`.
  - No per-channel byte bound exists elsewhere.
  - The 512 KiB value equals Track A's shaping byte ceiling and the Map per-item bound, but those bound different values
    and are not asserted equal.

### `apps/elitea-web`: admission mirror, files not touched by #1084

| Path | Change |
|---|---|
| `src/features/pipelines/lib/graphAdmission.helpers.ts:259-312` | `state.reducer` rule and `blockingIssues`, in every build. It mirrors each compiler refusal as save-blocking (`compiler.rs:2724` non-string, `:2780` builtin, `:2784` unknown name, `:2791` type) and turns a valid reducer into a non-blocking notice (`:2769`). `reducer: null` is no reducer, as in the Worker. |
| `src/features/pipelines/lib/graphAdmission.types.ts` | rule id `state.reducer` and optional `severity: 'warning'` |
| `src/features/pipelines/lib/livePipelineGraphAdmission.ts:108` | `isAdmissible` counts refusals only |
| `src/features/pipelines/ui/settings/GraphAdmissionGate.tsx:140,170` | the Save veto counts refusals only; notices render in the same outlined `Alert` and list |
| `src/pages/pipelines/ui/CreatePipelineAdmissionAlert.tsx` | lists refusals only |
| `src/features/pipelines/lib/flow-editor/helpers/pipelineFlow.types.ts` | `YamlStateVariableSpec.reducer` |

No new `t()` keys: the notice reuses the plain-string admission messages.

### `services/elitea-main`

Main never parses `state`, so it admits `reducer` in every build. A regression test keeps it that way:
`internal/domain/pipelinelimits/limits_test.go:115` (`TestCheckAdmitsTypedStateReducersInEveryBuild`, save and start,
with and without the `graph_extensions_rehearsal` tag).

### Worker test support

`src/agents/graph/pipeline_pg_test_support.rs` holds the generic PostgreSQL pipeline helpers (writer activation,
production runner, text continuation), moved without change out of the feature-gated `shaping_pg_tests.rs`. The
reducer process-replacement proof therefore runs in every build. The shaping test and its own gate are unchanged.

## 3. Tests

Final run on the branch merged with `origin/main` `84bdbb47e` (2026-10-12), with no feature flag:

| Suite | Result |
|---|---|
| `libs/rust`: `cargo test --offline --locked --workspace --all-targets --all-features` | 1,629 passed, 0 failed, 1 ignored (the opt-in reducer budget, §4), 42 targets |
| `agent-runtime --features toolkit-sql` | 764 passed, 1 ignored |
| `agent-runtime --features test-preserve-order,toolkit-sql` | 764 passed, 1 ignored |
| Worker: `cargo test --offline --locked --all-targets --all-features --no-fail-fast` with the CI PostgreSQL env (`ELITEA_TEST_DATABASE_URL` → throwaway PG18, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`) | 1,956 passed, 0 failed, 71 ignored (pre-existing opt-in tests), 13 targets |
| Worker with **default features** (`cargo test --lib -- state_reducer typed_reducers_restart production_builds`), CI PostgreSQL env | 21 passed, including `every_build_admits_typed_reducers`, the PostgreSQL process-replacement proof and the regeneration/new-turn proof |
| Main: `go test -race` on `internal/domain/pipelinelimits` with and without `-tags graph_extensions_rehearsal`; `go vet` | ok / ok / clean |
| `fmt --check` and `clippy --all-targets --all-features -D warnings` | clean in both workspaces, including `libs/rust` pedantic |
| Web: vitest `src/features/pipelines` + `src/pages/pipelines` (`--maxWorkers=4`) | 264 files, 2,836 passed, 1 expected-fail (pre-existing) |
| Web: `tsc --noEmit`, `oxlint` over `src`, `scripts/i18n-backfill.mjs --check` | clean / clean / OK |

`clippy` without features fails only on pre-existing dead code in `src/sandbox/` (not in this diff; CI runs
`--all-features`).

New tests:
- **`agent-runtime` `state_reducers_tests.rs` (18 + 1 ignored):**
  - every table row;
  - limit and limit+1 for elements, keys and bytes;
  - `i64::MAX + 1`, `i64::MIN - 1`, `u64::MAX`;
  - scalar-append refusal and the null merge;
  - the guard check agreeing with the reduction at every bound;
  - the ADK closure keeping the current value on an unchecked violation.
- **`agent-runtime` `exact_number_tests.rs` (4):** exact reading of `Display` text without `arbitrary_precision`.
- **Worker `state_reducer_compiler_tests.rs` (12):**
  - the guard: passes valid updates; fails with `code: channel` only, with a sentinel proving no value leaks; skips
    interrupted output;
  - every build admits each reducer;
  - the type and default matrix, builtins, and every overwrite-only channel (including a Map-owned output);
  - the nested-child refusal;
  - a Router loop that appends once per visit from a single default (T-B2, T-B5);
  - `interrupt_before` + `interrupt_after` around the reducing node: each resume reduces once (T-B4);
  - `sum_int` overflow fails the node and keeps the last state (T-B3).
- **Worker `state_reducer_digest_tests.rs` (3):**
  - golden digests of overwrite-only fixtures, computed with this file against the unmodified `origin/main` compiler
    (T-B6);
  - explicit `reducer: overwrite` keeps the golden digest;
  - the reducer fold is order-independent and tag-sensitive.
- **Worker `state_reducer_parity_tests.rs` (2):** one parser. Over 27 edge cases (i64 min/max, ±0, `2.0`, `1e2`,
  `1.50e1`, `100e-2`, `9223372036854775807.0`, values past both bounds, 21 digits, `2.5`, `1e-1`, a huge exponent),
  Track A's `data_shaping::exact_i64` (`Number::as_str`) and typed `sum_int` (`Display` text through
  `agent-runtime::exact_number`) must reach the same verdict under the Worker's `arbitrary_precision`. Any divergence
  fails the Worker's CI run.
- **Worker `pipeline_tests.rs` (1):**
  `typed_reducers_restart_from_defaults_on_regeneration_and_on_a_new_turn`, through the production pipeline assembler.
  It proves the expert's regeneration ASSUMPTION and adds the new-turn case.
- **Worker `state_reducer_pg_tests.rs` (1 + child):** PostgreSQL process replacement mid-loop (§5).
- **Web:**
  - `graphAdmission.helpers.test.ts` (+2 and the catalogue);
  - `livePipelineGraphAdmission.test.ts` (+1);
  - `GraphAdmissionGate.test.tsx` (+1: Save stays enabled and the notice shows);
  - `graphAdmissionReducer.test.ts` (3: valid reducers are notices, refusals are mirrored, `null` is none).
- **Main `limits_test.go` (1):** save and start admit typed reducers in every build.

**TDD record.**
- The unit and graph tests were written before the implementation compiled. The Web tests failed first for the right
  reasons (5 red).
- Mutation checks:
  - disabling the initialization fix turns the loop test red (`["seed","seed"]`) and the pause test red (lengths off by
    one);
  - removing the guard turns the overflow test red (the closure silently kept the value).

## 4. Performance

**Budget.** One typed update costs O(result bytes), bounded by `MAX_REDUCED_BYTES` (512 KiB), and runs once per
super-step: pipelines run with `max_concurrency(1)`. There are no extra database writes, round trips or events. The
reduced value is the same checkpoint state the overwrite channel would have held.

**Mechanism.**
- The guard checks `append` from element counts and serialized sizes without building the result (`check_update`,
  `state_reducers.rs:126`).
- The ADK closure builds the result once (`reduce_checked`).
- `merge` and `sum_int` reuse `reduce_checked`: at most 1,000 keys, or one integer.
- Overwrite-only pipelines take no new code path: `PipelineGraphBuilder::node` wraps only when a typed reducer exists.

**Measured.** `typed_update_budget_at_the_bounds` (opt-in, ignored by default), on a 10,000-element / 429,958-byte
list plus one element:

| Build | Guard check | Channel reduction |
|---|---|---|
| debug, shared host under two concurrent builds | 9.3 ms | 9.9 ms |
| release (`cargo test --release -p elitea-agent-runtime`) | 0.17 ms | 0.54 ms |

## 5. Durability

| Crash window | Rule | Proof |
|---|---|---|
| Initial checkpoint written, then crash | A typed default is stored once; restore merges no user input | `with_initial_checkpoint` `compiler.rs:2344`; loop test asserts the first checkpoint holds `["seed"]` |
| Node finished, update refused | The node fails before ADK applies anything; the last checkpoint is unchanged | `ReducerGuard::execute`; overflow graph test; browser case 3 (checkpoint 3 holds `i64::MAX` with `tick` pending) |
| `interrupt_after` on the reducing node, then resume | The update is applied once before the pause; resume input carries only runtime keys | pause test (6 pauses, +1 element per visit) |
| Process replacement mid-loop | Claim attempt 2 restores the durable checkpoint and continues; completed visits are never re-applied | `postgres_typed_reducers_accumulate_once_across_process_replacement` on real PostgreSQL 18: the write process stops after 2 of 3 visits; every checkpoint extends the previous one by at most one visit; history before the replacement is preserved; final `findings` = seed + 3 visits, `count` = 3 |
| Regeneration / new turn | Regeneration deletes the thread's checkpoints and starts from defaults; a new turn also starts from defaults | assembler test; browser case 1 (execution `7c1b…`'s checkpoints deleted by the regeneration `e894…`, which restarted at ordinal 12) |
| Nested child restore | Refused in v1 (`Unsupported`): ADK re-merges parent-mapped input through reducers | `nested_child_pipelines_refuse_typed_reducers_in_v1` |

### Recovery guarantees (component × phase touched)

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission (compile) | **F**: typed refusal (`invalid_configuration`) of a bad declaration before any model or tool call | `compiler.rs:2774,2815,1407` | compiler tests |
| Worker × deterministic node update with a typed channel | **R**: completed visits are never re-applied; the next visit continues from the last checkpoint | `state_reducers.rs:233-269`; `compiler.rs:2344` | PG process replacement; pause test |
| Worker × deterministic node update refused | **F**: typed node failure, last state preserved, generic chat text plus support reference until Wave 2 | `state_reducers.rs:269-290` | overflow graph test; browser case 3 |
| Worker × static HITL pause and decision (P06) | **R**, unchanged; reduces once across the pause | — | pause test |
| Main, Web, NATS, gateway, sandbox supervisor | unchanged; no new effect or transport | — | — |

No cell of `docs/recovery-guarantees.md` changes, and no L is introduced.

## 6. Resilience

- **Bounded:**
  - 10,000 elements, 1,000 keys and 512 KiB per typed channel, checked before ADK applies the update;
  - declared defaults are checked at compile time against the same bounds;
  - limits are named constants in `agent-runtime` (`state_reducers.rs:21-25`, `exact_number.rs:11`);
  - they are compile-time asserted against the checkpoint and fan-out bounds (§2).
- **Typed failures:**
  - `graph.state.reducer_type_mismatch`, `graph.state.reducer_limit`, `graph.state.reducer_overflow`;
  - the node message is `"<code>: <channel>"`.
- **No silent coercion:**
  - no scalar wrapping, no `null` clear, no `f64`;
  - the ADK closure keeps the current value and logs `error!` only on an invariant the guard already enforces.
- **Cancellation and lease loss:** unchanged. The guard runs after the inner node returns and adds no await or
  resource.
- **No flag:** Worker, Main and Web admit the key in every build. The Web mirrors exactly the compiler's refusals, so the
  editor cannot store a declaration the runtime refuses, and it cannot drop an admitted one unseen.

## 7. Security (`.claude/rules/security.md`)

| Category | Applies? | How it was checked |
|---|---|---|
| Trust boundaries, identity | No | No route, RPC or identity change |
| Authorization | No | No new read or write; reducer state lives in the existing per-session checkpoint |
| Input, parsing, amplification | Yes | YAML `reducer` is a closed set behind `deny_unknown_fields`. Defaults are bounds-checked at compile time. Updates are size-checked before reduction, and the byte count is capped and allocation-free. |
| Injection | Yes, minimal | The reducer name never reaches SQL, a template, a path or a shell. React renders the notices; there is no raw HTML. |
| Egress | No | — |
| Secrets, redaction | Yes | Errors and logs carry the code and channel name only. A sentinel test proves no value reaches the error. The browser case found no state value in the Worker logs (`grep` for the overflow value: 0). |
| Supply chain | Yes | No new dependency (no `Cargo.lock`/`package-lock.json` change). `cargo deny --all-features check advisories`: Worker RUSTSEC-2023-0071 (pre-existing, no fix); `libs/rust` RUSTSEC-2023-0071 and RUSTSEC-2024-0436 (pre-existing). `npm audit --omit=dev`: 2 low in `mermaid` (pre-existing). |

`security-review` on the branch found no high-confidence vulnerabilities (§9).

## 8. Real-browser evidence

### 8.1 Acceptance with no feature flag, on current `main` (2026-10-12)

**Stack.** My own standalone stack `elitea-p5-reducers`, built per `handoffs/real-model-stack/README.md`.
- Browser host `http://reducers.localhost:18620` (`STANDALONE_HOST`), OIDC mock `oidc.localhost:19720`, signed in as
  `admin@centry.user`.
- Database: `product-real-models-main-c0f2e5f9b.dump` restored into an empty database. `elitea-migrate` brought it to
  the branch head: shared 164, tenant 148.
- No shared stack was touched.

**Images.** Every service whose code changed since the dump was built from branch head `46949abfb` (`origin/main`
`84bdbb47e` merged in, plus the ungate commit). No image carries a rehearsal feature, build tag or `VITE` flag.

| Service | Image | Check |
|---|---|---|
| Worker | `elitea-worker-rust:p5r` `sha256:2e6f3455745b…4ee3` | Default features. Binary contains `graph.state.reducer_overflow`, `typed state value exceeds its byte bound` (from `agent-runtime`), `elitea.graph.pipeline.state-reducers.v1` and `a pipeline state reducer does not match its state type`. The removed gate text "typed state reducers are not available in this deployment" is absent. Branch contains #1160. |
| Web | `elitea-web:p5r` `sha256:b305e743b430…95cf` | Default build. Bundle contains `graph-admission-notice` and the notice text; the removed refusal "reducer is not available on this deployment" is absent. |
| Main (+ migrate) | `elitea-main:p5r` `sha256:1d33e7ea4163…f2ff6` | `e2e` target |
| Gateway, subapp-host | `elitea-llm-gateway:p5r`, `elitea-subapp-host:p5r` | — |
| Vector, inventory engine | `elitea-vector:p5r`, `elitea-inventory-engine:p5r` | Required by the current compose: the Worker waits on `elitea-vector`; the inventory engine is now Rust. |
| Scheduler, DeepWiki engine, mocks | the `main-c0f2e5f9b-verify` / `main-0def77b22-verify` images | Unchanged scheduler; the other services are not used by pipelines |

**Case.** No response was mocked, and no case calls a model.

| Step | Ids | Result |
|---|---|---|
| Author in the editor | pipeline 166 "p5 reducers ungated acceptance", stored as `p_2` version 192 | The create page's YAML editor admits a document with `count` (`sum_int`), `findings` (`append`) and `seen` (`merge`) in a Router loop. Save is enabled with no admission alert. After Save the editor shows the outlined YAML-only notice for all three channels. |
| Run from a persistent chat | chat 880, execution `c023cfdef74ebe0e6d56b5f35e30cbf2` (`SUCCEEDED`) | The answer reads `3 results: result 1, result 2, result 3; seen 3`. |
| Reload | chat 880; pipeline 166 | The chat shows the same single answer. The reopened editor's Yaml tab still holds `reducer: sum_int`, `reducer: append` and `reducer: merge`, with the notice and no save veto. The stored instructions hold the 3 keys. |
| Durable state | 14 checkpoints of the execution | Ordinal 1: the defaults once (`0`, `[]`, `{}`). Ordinal 14 (terminal): `count` 3, `findings` `["result 1","result 2","result 3"]`, `seen` `{"visit_1":"done","visit_2":"done","visit_3":"done"}`. |

**Fixtures.** Pipeline 166 and its chat were created in the browser. The database reads are read-only `SELECT`s. My
stack, its volumes and the throwaway PG18 test container were torn down afterwards.

### 8a. First pass, rehearsal-gated (2026-10-09; history)

**Stack.** My own standalone stack `elitea-p5-reducers`, built per `handoffs/real-model-stack/README.md`.
- Browser host: `http://reducers.localhost:18620`, with `STANDALONE_HOST`. OIDC mock: `oidc.localhost:19720`,
  signed in as `admin@centry.user`.
- Database: `product-real-models-main-c0f2e5f9b.dump` restored into an empty database (shared 158, tenant 148). No
  unmerged migration. The shared stack was not touched.

**Images.**

| Role | Image | Source and check |
|---|---|---|
| Worker (rehearsal) | `elitea-worker-rust:p5-reducers-rehearsal` `sha256:24e07de3bed5…b40f` | built from `01a8ba043` (branch head); contains #1160. The binary contains `graph.state.reducer_overflow`, `typed state value exceeds its byte bound` and `a typed state update reached the channel unchecked` (from `agent-runtime`), plus `elitea.graph.pipeline.state-reducers.v1` and `typed state reducers are not supported in a nested pipeline` (Worker). The production-only text "typed state reducers are not available in this deployment" is compiled out, which proves a rehearsal build. None of these strings exist on `main`. |
| Web (rehearsal) | `elitea-web:p5-reducers-rehearsal` `sha256:c105ef8c1ac7…287f` | built from `2379a745f`; Web content is identical at head. The bundle contains `graph-admission-notice` (`EditPipeline-*.js`) and the notice text (`livePipelineGraphAdmission-*.js`). |
| Worker (production-default, case 4) | `elitea-worker-rust:p5-reducers-default` `sha256:8e9517f9fd52…7aa6` | built from `01a8ba043` without features; contains #1160 and the production-only refusal text |
| Main, gateway, scheduler, subapp-host | `main-c0f2e5f9b-verify` | merged `main`; since then Main changed only `toolkitcatalogue/capability.go` (#1188) and the gateway only its master-key/CEL bound (#1190); neither touches pipeline state |

**Cases.** No response was mocked, and no case calls a model.

| Case | Pipeline / chat / executions | Result |
|---|---|---|
| 1. Router loop with `append` + `sum_int` in a persistent chat (brief 1) | pipeline 166 (`p_2` version 192), chat 880; executions `7c1b1985…`, `e8945fef…` (regenerate), `9dd854aa…` (second turn) | The answer reads `3 results: result 1, result 2, result 3`; after reload it is the same single answer. Regeneration and a second turn each answer the same 3 results, not 6. Their terminal checkpoints hold exactly `["result 1","result 2","result 3"]` and `count` 3. |
| 2. YAML editor notice, save and reload (brief 2) | pipeline 166 | After Save the editor shows an outlined warning, `state.count: the "sum_int" reducer can be changed in YAML only for now. The visual editor keeps it as written.` (and the same for `state.findings` / `append`). Save stays enabled. After a reload the re-dumped Yaml tab still contains `reducer: sum_int` and `reducer: append`, and the stored instructions contain both keys. |
| 3. `sum_int` overflow (brief 3) | pipeline 167, chat 881, execution `b627ef16…` (`FAILED`) | The chat shows "The runtime operation failed." with *Details for support* (`INTERNAL`, message id `5e111cbc-…`); the same after reload. Checkpoints: ordinal 1 `count …806`, 2 `…807` (= `i64::MAX`), 3 `i64::MAX` with `tick` pending; nothing after the refused update. The generic text is expected until the Wave 2 failure catalogue. |
| 4. Existing pipelines on a production-default build (brief 4) | Worker swapped to `elitea-worker-rust:p5-reducers-default` `sha256:8e9517f9fd52…7aa6` (no rehearsal feature; built from `01a8ba043`; its binary contains the production-only "typed state reducers are not available in this deployment" and the shared reducer strings). Existing record from the dump: pipeline 153 "PR1140 numeric node id" (version 179, overwrite-only), chat 882, execution `bd0c87f6…` (`SUCCEEDED`). Negative control: pipeline 166 in chat 880, execution `2d1d833e…` (`FAILED`). | The existing pipeline answers `numeric-id-ok`; after reload the same single answer. The reducer pipeline is refused at start: the chat shows "Configuration type is not supported." (the existing generic `unsupported_capability` text; naming the cause is follow-up F2), and the Worker log has `native_agent.unsupported_capability`. One INFO-level `session.invalid_scope` on `get` belongs to the succeeded execution: the normal probe before a new chat's session is created. |

**Head vs. images.** Branch head `8e843f0f2` differs from the image source `01a8ba043` only by the source mapping, the
parity test and the compile-time limit assertions. There is no runtime change.

**Fixtures.**
- Pipelines 166 and 167 were created in the browser through the create page's YAML editor. Their chats and runs were
  started from the editor's Chat button.
- Pipeline 153 is an existing record of the restored dump. Its chat 882 was started from its editor.
- A misplaced click opened the dump's chat 867 once. Nothing was sent: 0 executions and 0 messages in that window,
  checked in the database.
- The database reads above are read-only `SELECT`s for evidence.

## 9. Reviews

- **`code-review` (high) on the full diff:** 6 findings. 5 were fixed:
  - the Web mirrors the compiler's refusals (at the time, also the then-existing production gate);
  - `null`/non-string reducers now behave as in the Worker;
  - an allocation-free guard check;
  - exact test assertions;
  - reuse of the digest field encoding.

  1 was skipped: the create page lists refusals only, so it shows no notice for a reducer-only document. The brief's notice scope is the YAML editor.
- **`security-review` on the branch:** no high-confidence vulnerabilities.
- **The ADR-0027 layout rule (2026-10-09)** moved the semantics and the exact-number reader into `agent-runtime`
  after the first review. The moved code was re-tested in both workspaces (§3).

## 10. Open limits and follow-ups

1. **Worker log filter drops `elitea_agent_runtime` events** (`src/diagnostics.rs:211` builds `elitea_worker_rust=<level>`).
   - The guard's refusal warning, and every other moved runtime module's logs, are invisible in deployed Workers.
   - Found in browser case 3. It is pre-existing since the extraction and is not changed here.
   - Proposed as a separate task.
2. **The chat failure text is generic** ("The runtime operation failed.") until the Wave 2 failure catalogue, which
   lives in `protocol/output.rs` (touched by #1084). The typed code reaches the node failure only.
3. **Web reducer editing** (`StateDrawer`, `dumpYaml`) waits for Wave 2. Until then the notice says the key is
   YAML-only. A canvas edit keeps it: `stateVariableSpec.helpers.ts` spreads the descriptor.
4. **Nested child pipelines refuse typed reducers in v1.**
5. **Code-recovery × reducer test (B2)** is Wave 2; it needs `node_recovery_runtime_tests.rs` (touched by #1084).
6. **SplitOut/Aggregate (`data_shaping`) are node executors and belong in `agent-runtime`** under ADR-0027. They now
   use its shared exact-number reader; moving the rest is a separate task.
7. **Possible new-turn accumulation.** A new turn on a completed persistent thread restarts typed channels from their
   defaults, which is the same as overwrite channels today. Accumulating across turns would need an explicit product
   decision.
