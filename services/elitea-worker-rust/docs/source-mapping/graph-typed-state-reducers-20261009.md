# Graph typed state reducers (Point 5, Wave 1, Track B) — 2026-10-09

Brief: `handoffs/point5-wave1-20261008/T-B-typed-reducers.md` (local). Branch `feat/graph-typed-state-reducers`.

A declared pipeline state variable can choose a bounded, typed reducer in its YAML descriptor:

```yaml
state:
  findings: {type: list, value: [], reducer: append}
```

The `reducer` key is admitted only by rehearsal builds (`graph-extensions-rehearsal`). Production builds refuse it, and
the Web editor mirrors that gate. Overwrite stays the default, and every overwrite-only definition keeps its digest
byte for byte.

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
| `:2446` | the gate `TYPED_REDUCERS_READY = cfg!(feature = "graph-extensions-rehearsal")` |
| `:893` | `validate_state(raw.state, TYPED_REDUCERS_READY)` |
| `:2774` (`typed_reducer`) | compile-time checks: the gate, builtins `input`/`messages`, known names, type compatibility, default within bounds |
| `:2815` (`validate_overwrite_channels`), called at `:936` | channels that must overwrite: Map destination, Parallel output, SplitOut/Aggregate output, HITL `edit_state_key`, StateModifier `variables_to_clean`, and every output, clean or edit key of a Parallel-owned or Map-owned node (their updates never pass the top-level guard) |
| `:459-464` (`PipelineGraphBuilder::node`) | the guard at the single registration point; it wraps every node only when the definition has a typed reducer |
| `:1744` | the typed ADK channel in `state_schema` |
| `:2344` (`with_initial_checkpoint`) | the initialization fix: a typed channel's default is inserted, not reduced onto itself |
| `:1407` (`compile_subgraph_with_runtime`) | nested child pipelines refuse typed reducers in v1 with `Unsupported` |
| `:949`, `:3023-3027` (`reducers_digest`) | the digest fold, only when a typed reducer exists: `elitea.graph.pipeline.state-reducers.v1\0` plus sorted, length-prefixed `(key, tag)` |

Other Worker files:
- `src/agents/graph/mod.rs`: re-exports `elitea_agent_runtime::graph::state_reducers`.
- `src/agents/graph/data_shaping.rs`: `decimal`, `exact_i64` and `exact_u64` delegate to `exact_number` with
  `Number::as_str()`. Shaping codes are unchanged (`number_code`).
- Test helpers widened to `pub(super)`, no behaviour change: `static_pause_tests.rs`, `shaping_pg_tests.rs`.

### `apps/elitea-web`: admission mirror, files not touched by #1084

| Path | Change |
|---|---|
| `src/features/pipelines/lib/graphAdmission.helpers.ts:259-313` | `state.reducer` rule and `blockingIssues`. A production build refuses (`compiler.rs:2784`). A rehearsal build mirrors each compiler refusal (`:2729` non-string, `:2789` builtin, `:2794` unknown name, `:2800` type) and turns a valid reducer into a non-blocking notice. |
| `src/features/pipelines/lib/graphAdmission.types.ts` | rule id `state.reducer` and optional `severity: 'warning'` |
| `src/features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts:100` | `TYPED_STATE_REDUCERS_ADMITTED`, the same rehearsal flag as SplitOut/Aggregate |
| `src/features/pipelines/lib/livePipelineGraphAdmission.ts:108` | `isAdmissible` counts refusals only |
| `src/features/pipelines/ui/settings/GraphAdmissionGate.tsx:140,170` | the Save veto counts refusals only; notices render in the same outlined `Alert` and list |
| `src/pages/pipelines/ui/CreatePipelineAdmissionAlert.tsx` | lists refusals only |
| `src/features/pipelines/lib/flow-editor/helpers/pipelineFlow.types.ts` | `YamlStateVariableSpec.reducer` |

No new `t()` keys: `en.json` is touched by #1084, and the notice reuses the plain-string admission messages.

## 3. Tests

| Suite | Result |
|---|---|
| `libs/rust`: `cargo test --offline --locked --workspace --all-targets --all-features` | 1,028 passed, 0 failed, 1 ignored (the opt-in reducer budget, §4) |
| `agent-runtime --features toolkit-sql` | 531 passed, 1 ignored |
| `agent-runtime --features test-preserve-order,toolkit-sql` | 531 passed, 1 ignored |
| Worker: `cargo test --offline --locked --all-targets --all-features --no-fail-fast` with the CI PostgreSQL env (`ELITEA_TEST_DATABASE_URL` → throwaway PG18, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`) | 1,858 passed, 0 failed, 71 ignored (pre-existing opt-in tests), 13 targets |
| Worker without features (`cargo test --lib -- production_builds state_reducer shaping`) | 44 passed, including `production_builds_refuse_any_reducer_key` and `production_builds_refuse_shaping_yaml_naming_the_gated_type` (CI's `--all-features` run cannot reach these) |
| `fmt --check` and `clippy --all-targets --all-features -D warnings` | clean in both workspaces, including `libs/rust` pedantic |
| Web: vitest `src/features/pipelines` + `src/pages/pipelines` (`--maxWorkers=4`) | 263 files, 2,828 passed, 1 expected-fail (pre-existing) |
| Web: `tsc --noEmit`, `oxlint` over `src`, `scripts/i18n-backfill.mjs --check` | clean / clean / OK |

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
  - the production refusal;
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
- **Worker `pipeline_tests.rs` (1):**
  `typed_reducers_restart_from_defaults_on_regeneration_and_on_a_new_turn`, through the production pipeline assembler.
  It proves the expert's regeneration ASSUMPTION and adds the new-turn case.
- **Worker `state_reducer_pg_tests.rs` (1 + child):** PostgreSQL process replacement mid-loop (§5).
- **Web:**
  - `graphAdmission.helpers.test.ts` (+2 and the catalogue);
  - `livePipelineGraphAdmission.test.ts` (+1);
  - `GraphAdmissionGate.test.tsx` (+1, production);
  - `GraphAdmissionReducerRehearsal.test.tsx` (4, rehearsal flag through `vi.stubEnv`).

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
| Worker × admission (compile) | **F**: typed refusal before any model or tool call (production: `unsupported_capability`; rehearsal: `invalid_configuration` for a bad declaration) | `compiler.rs:2774,2815,1407` | compiler tests |
| Worker × deterministic node update with a typed channel | **R**: completed visits are never re-applied; the next visit continues from the last checkpoint | `state_reducers.rs:233-269`; `compiler.rs:2344` | PG process replacement; pause test |
| Worker × deterministic node update refused | **F**: typed node failure, last state preserved, generic chat text plus support reference until Wave 2 | `state_reducers.rs:269-290` | overflow graph test; browser case 3 |
| Worker × static HITL pause and decision (P06) | **R**, unchanged; reduces once across the pause | — | pause test |
| Main, Web, NATS, gateway, sandbox supervisor | unchanged; no new effect or transport | — | — |

No cell of `docs/recovery-guarantees.md` changes, and no L is introduced.

## 6. Resilience

- **Bounded:**
  - 10,000 elements, 1,000 keys and 512 KiB per typed channel, checked before ADK applies the update;
  - declared defaults are checked at compile time against the same bounds;
  - limits are named constants in `agent-runtime` (`state_reducers.rs:21-25`, `exact_number.rs:11`).
- **Typed failures:**
  - `graph.state.reducer_type_mismatch`, `graph.state.reducer_limit`, `graph.state.reducer_overflow`;
  - the node message is `"<code>: <channel>"`.
- **No silent coercion:**
  - no scalar wrapping, no `null` clear, no `f64`;
  - the ADK closure keeps the current value and logs `error!` only on an invariant the guard already enforces.
- **Cancellation and lease loss:** unchanged. The guard runs after the inner node returns and adds no await or
  resource.
- **Gate:** production builds refuse any `reducer` key, and the Web refuses it too, so a production editor cannot store
  a pipeline its runtime refuses.

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
| Main, gateway, scheduler, subapp-host | `main-c0f2e5f9b-verify` | merged `main`; since then Main changed only `toolkitcatalogue/capability.go` (#1188) and the gateway only its master-key/CEL bound (#1190); neither touches pipeline state |

**Cases.** No response was mocked, and no case calls a model.

| Case | Pipeline / chat / executions | Result |
|---|---|---|
| 1. Router loop with `append` + `sum_int` in a persistent chat (brief 1) | pipeline 166 (`p_2` version 192), chat 880; executions `7c1b1985…`, `e8945fef…` (regenerate), `9dd854aa…` (second turn) | The answer reads `3 results: result 1, result 2, result 3`; after reload it is the same single answer. Regeneration and a second turn each answer the same 3 results, not 6. Their terminal checkpoints hold exactly `["result 1","result 2","result 3"]` and `count` 3. |
| 2. YAML editor notice, save and reload (brief 2) | pipeline 166 | After Save the editor shows an outlined warning, `state.count: the "sum_int" reducer can be changed in YAML only for now. The visual editor keeps it as written.` (and the same for `state.findings` / `append`). Save stays enabled. After a reload the re-dumped Yaml tab still contains `reducer: sum_int` and `reducer: append`, and the stored instructions contain both keys. |
| 3. `sum_int` overflow (brief 3) | pipeline 167, chat 881, execution `b627ef16…` (`FAILED`) | The chat shows "The runtime operation failed." with *Details for support* (`INTERNAL`, message id `5e111cbc-…`); the same after reload. Checkpoints: ordinal 1 `count …806`, 2 `…807` (= `i64::MAX`), 3 `i64::MAX` with `tick` pending; nothing after the refused update. The generic text is expected until the Wave 2 failure catalogue. |
| 4. Existing pipelines on a production-default build (brief 4) | PENDING | PENDING |

**Fixtures.**
- Pipelines 166 and 167 were created in the browser through the create page's YAML editor. Their chats and runs were
  started from the editor's Chat button.
- The database reads above are read-only `SELECT`s for evidence.

## 9. Reviews

- **`code-review` (high) on the full diff:** 6 findings. 5 were fixed:
  - the Web mirrors the production refusal;
  - `null`/non-string reducers now behave as in the Worker;
  - an allocation-free guard check;
  - exact test assertions;
  - reuse of the digest field encoding.

  1 was skipped: the create page lists refusals only, so it shows no notice for a reducer-only document on rehearsal
  builds. The brief's notice scope is the YAML editor.
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
