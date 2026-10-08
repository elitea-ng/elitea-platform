# Graph shaping: SplitOut and Aggregate (Gate 5c, first tier) — 2026-10-08

Track A of Point 5 Wave 1, branch `feat/graph-shaping-split-out-aggregate`.
Brief: `handoffs/point5-wave1-20261008/T-A-split-out-aggregate.md`.
Contract: [`../split-out-aggregate-contract.md`](../split-out-aggregate-contract.md).

Both node types are **not admitted in production**. The Worker compiler admits them only with the off-by-default
Cargo feature `graph-extensions-rehearsal`. The Web editor shows them only when `VITE_GRAPH_EXTENSIONS_REHEARSAL=true`.

## 1. Business behaviour and sources

| Behaviour | Source | Decision |
|---|---|---|
| Split a list field into one item per element, keep parent fields | n8n Split Out (`fieldToSplitOut`, `include` none/all/selected/except), Spark `explode`/`posexplode` | Ported as `split_out` with modes `list`/`row_field`/`rows_field` and `retain` none/all/only/except. Every row is an envelope `{parent_index, position, data}` (user decision U1), which Spark's `posexplode` informs. |
| Aggregate items back, group and summarize | n8n Aggregate and Summarize, Spark `groupBy().agg()` | Ported as `aggregate` with 8 explicit operations and `group_by`. Output rows are flat (user decision §6.4). |
| Rebuild split parents | n8n "split, process, aggregate" pattern | `regroup: parent` is the exact inverse of SplitOut `rows_field` + `retain: all` + `remove_source`. |
| Empty input | user decision U2 | One identity row without `group_by`. No rows with `group_by`. |

**Deliberately not ported:**
- n8n's silent wrapping of scalars into lists: a scalar is `type_mismatch`.
- n8n and JavaScript number coercion through floating point: integer operations use exact decimal arithmetic and
  reject `2.5`.
- Expression or template evaluation inside field paths: paths are RFC 6901 pointers only.
- The current platform has no shaping node. Nothing was taken from the Python SDK or the Pylon plugins.

## 2. Changed paths

Worker (`services/elitea-worker-rust/`):
- `src/agents/graph/data_shaping.rs`: shared core.
  - Bounded node YAML: `:43`.
  - Pointer parse: `:68`.
  - Limit ceilings: `:24`. Limit parsing: `:760`. Input and group checks: `:815`, `:823`.
  - Row budget: `:850`, `:874`, `:890`, `:899`, `:933`.
  - Exact decimals: `:495`, `:574`.
  - Canonical keys: `:606`, `:674`.
  - Envelope: `:463`.
  - Errors: `:1041`, `:1052`.
- `src/agents/graph/split_out.rs`:
  - Raw schema: `:51`. Validation: `:95`. Digest: `:178`.
  - `run`: `:225`. Per-parent split: `:258`. Retention: `:304`.
  - Node: `:347`.
- `src/agents/graph/aggregate.rs`:
  - Raw schema: `:79`. Validation: `:170`. Digest: `:265`.
  - `run`: `:613`. Grouping: `:688`. Regroup: `:777`.
  - Checked sum: `:478`. `first` policy fix: `:494`.
  - Node: `:926`.
- `src/agents/graph/compiler.rs`:
  - Gate: `:2013`. Admission: `:2025`, arms `:2066` and `:2069`.
  - Channel validation: `:2187`. Recovery frontier: `:797`. Transition binder: `:505`.
  - Digest kind tags: `:2427`, `:2428`.
- `src/agents/graph/shaping_bench.rs` and `benches/graph_shaping.rs`: rehearsal-only benchmark entry and harness.
- `Cargo.toml:19` (feature), `Cargo.toml:21` and `:85` (bench feature and target), `Containerfile:94` (`WORKER_REHEARSAL_FEATURES` build argument).

Web (`apps/elitea-web/`):
- `src/features/pipelines/lib/graphShapingAdmission.helpers.ts`: `regroup`, cross-list output names, `*_int` null
  enum, `collect_rows` without `none`, per-type limit keys, Worker citations.
- `src/features/pipelines/lib/graphExtensions.types.ts`: `values` 32,768 and `shapingLimitKeys`.
- `src/features/pipelines/ui/settings/graph-extensions/{AggregateSettings,AggregateOperations,SplitOutSettings,ExtensionLimits}.tsx`:
  regroup selector, help text, limit keys.
- `src/features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts:77`: rehearsal flag. The file also
  refreshes its `compiler.rs` citations.
- `Containerfile:32`: `VITE_GRAPH_EXTENSIONS_REHEARSAL` build argument, default `false`.

New `t()` keys are in `en.json`, added by `scripts/i18n-backfill.mjs`, because the CI en.json sync gate requires
them: `pipelines.graphExtensions.splitOut.envelopeHelp`, `pipelines.graphExtensions.regroup`,
`pipelines.graphExtensions.regroupHelp`, `pipelines.graphExtensions.aggregateEmptyHelp`,
`pipelines.graphExtensions.aggregateFlatDescription`.

`en.json` is the only file this PR shares with PR #1084. The five keys are appended at the end of the file, and #1084 changes
only one line near line 4494, so the two edits do not overlap. No other file changed by PR #1084 is touched.

## 3. Tests

Shaping suites, 86 tests (Worker, `src/agents/graph/`):

| File | Tests | Scope |
|---|---|---|
| `data_shaping_tests.rs` | 26 | pointers, policies, retention, envelopes, decimals, canonical keys, limits, budget, errors |
| `split_out_tests.rs` | 18 | schema, modes, retention, list policies, limits, digest, leak checks, node update |
| `aggregate_tests.rs` | 19 | schema, operations, policy matrix, integers, grouping, limits, regroup, digest, node update |
| `shaping_compiler_tests.rs` | 11 | gate on and off, channel validation, compiled pipelines, log capture, alias amplification, digest |
| `shaping_property_tests.rs` | 10 | P1–P7 at 2,000 seeded cases each, plus 3 adversarial cases |
| `shaping_pg_tests.rs` | 2 | PostgreSQL 18 process replacement |

Notes on the counts:
- `production_builds_refuse_shaping_yaml` runs only without the feature (`cargo test --locked`).
- `shaping_failure_log_child` is the child half of the log-capture test.
- Both PostgreSQL tests skip cleanly when `ELITEA_TEST_DATABASE_URL` is unset.

Full suites (local, 2026-10-08):

| Command | Result |
|---|---|
| `cargo test --locked` (default features) | 1,999 passed, 0 failed, 2 ignored |
| `cargo test --locked --all-targets --all-features --no-fail-fast` with PG18, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` | 2,198 passed, 0 failed, 63 ignored at `cb6b51d2`; 2,199 passed, 0 failed, 63 ignored after merging `origin/main` (`27d89c2e`, 13 targets) |
| `cargo clippy --locked --all-targets --all-features -- -D warnings` | clean |
| `cargo fmt --all -- --check` | clean |
| Web `vitest --project node src/features/pipelines` | 2,485 passed, 1 expected fail (an existing `it.fails`), 0 skipped |
| Web `npm run typecheck`, `npm run lint` | clean |
| `node scripts/i18n-backfill.mjs --check` | OK after the backfill (only the dynamic-key flags already on main) |

Skipped or limited:
- `ELITEA_REQUIRE_NATS_SECURE_TEST` was not set locally. It needs the CI NATS test server, so the secure NATS test
  skips here.
- `map_reduce_tests::reverse_completion_collects_original_order_and_preserves_structured_values` failed once under
  load average 20, in an earlier full run. It asserts a concurrent-worker count. It passed 10 of 10 isolated runs and
  in the final run. Track A does not touch Map code; this is a pre-existing timing-sensitive test.
- `cargo clippy` without `--all-features` reports dead code in `src/sandbox/*` (needs `sandbox-supervisor`). This
  exists on main; CI runs clippy with `--all-features`.

## 4. Performance

Budgets: brief (SplitOut 10,000 rows ≤ 20 ms, group-by into 1,000 groups ≤ 3 ms, memory ≤ `bytes` + one row) and
`experts/03-performance.md` §4 (regroup ≤ 2 ms).

Measured with `cargo bench --locked --features graph-shaping-bench --bench graph_shaping`. The bench entry module is
behind its own `graph-shaping-bench` feature, which no image enables. The machine is an
M-series laptop with load average 10–20 from other sessions. There are 200 timed iterations after warm-up. Times are
in ms.

| Case | p50 | p99 | Budget | Output bytes | Largest row |
|---|---|---|---|---|---|
| split_out list, 6,500 rows | 2.12 | 2.47 | 20 | 342,281 | 52 |
| split_out list, 10,000-element source (fails at `limits.values`) | 4.11 | 4.46 | 20 | – | – |
| split_out rows_field, 1,000 parents × 5, retain all | 2.40 | 2.70 | 20 | 288,901 | 57 |
| aggregate group_by, 10,000 rows → 1,000 groups, count + sum_int | 0.81–0.85 | 0.98–1.07 | 3 | 26,891 | 26 |
| aggregate regroup parent, 6,500 rows, collect | 1.04–1.18 | 1.19–1.93 | 2 | 48,281 | 50 |
| aggregate collect_rows, 2,000 rows | 0.43 | 0.48 | 5 | 36,899 | 36,897 |

- **Before the allocation fix,** group-by p99 was 3.0–4.2 ms, which is over budget. The fix (commit `c3015228`) reuses
  one scratch key per run and encodes scalars and plain integer text without the walk stack.
- **SplitOut row cap:** a SplitOut envelope holds at least 5 values. Under the `values` ceiling of 32,768, at most
  6,553 rows fit. The brief's "10,000 rows" case therefore fails fast at `limits.values` in 4.1 ms, and the largest
  admissible output was measured instead. This is recorded in the contract §1.
- **Memory:** the crate forbids `unsafe_code`, so a counting global allocator is not possible. Peak extra memory is
  bounded by construction: every row is charged before it is pushed (`data_shaping.rs:874`), so the output never
  exceeds `limits.bytes` plus the row being built. The table shows the measured output bytes and largest row.
- **Database writes:** each node writes one channel update per activation. The checkpoint history in §7 shows one
  write of `orders_out`.

## 5. Durability

| Crash window | Recovery rule | Proof |
|---|---|---|
| After the SplitOut checkpoint, before Aggregate | The replacement process resumes at the paused node; SplitOut does not run again | `shaping_pg_tests.rs:256` (PostgreSQL 18, separate process, new writer claim) |
| During a shaping node | No partial output: the node validates fully, then emits one update | `node_emits_one_update_and_leaves_source_untouched` (`split_out_tests.rs:544`), `node_emits_exactly_one_update_to_its_output` (`aggregate_tests.rs:1095`); browser failure run (§7, execution `8447…`) wrote nothing after `split` |
| Re-execution after recovery | Output is a pure function of state and config, so both nodes join `recovery_frontier_supported` | `compiler.rs:797`, `split_then_regroup_restores_the_parents_in_one_pipeline` (`shaping_compiler_tests.rs:83`), P6 determinism and checkpoint round trip (`shaping_property_tests.rs:327`) |
| Config drift between processes | Config digests with fixed domains, plus pipeline digest kind tags | `digest_is_stable_and_sensitive` (`split_out_tests.rs:497`), `config_digest_is_stable_and_field_sensitive` (`aggregate_tests.rs:1023`), `pipeline_digest_binds_the_shaping_configuration` |

PostgreSQL replay result: the final `orders_out` is byte-identical to an uninterrupted run. Checkpoints after the
resume wait only on `[join]` and then nothing, and `orders_out` changes from `[]` exactly once. A deliberate
two-writes expectation makes the test fail.

Limit: the continuation is accepted against the stored pause, but the test does not prove the continuation is
required. Resuming without it also passes, because the pause checkpoint is already marked as cleared.

## 5a. Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker crash or replacement after a shaping checkpoint | R — Resume | ADK checkpoint per step on the PostgreSQL checkpointer; `recovery_frontier_supported` admits both nodes (`compiler.rs:797`) | `shaping_pg_tests.rs:256`: the replacement process resumes, SplitOut does not run again, one write |
| Worker crash during a shaping node | R — Resume | The node writes nothing until its single update; the pre-node checkpoint stays the latest | `node_emits_one_update_and_leaves_source_untouched`, `node_emits_exactly_one_update_to_its_output`; the browser failure run wrote nothing after `split` |
| Shaping limit or data failure | F — Typed failure | `ShapingError::into_graph_error` `data_shaping.rs:1052`; input state is preserved | §6 tests; browser item 4 |
| Main, NATS, LLM gateway, sandbox supervisor | Unchanged | Shaping adds no call to these components | – |

## 6. Resilience

| Rule | Code mechanism | Proving test |
|---|---|---|
| Node YAML ≤ 64 KiB | `parse_node_yaml` `data_shaping.rs:43` | `node_yaml_is_bounded` `data_shaping_tests.rs:830` |
| YAML alias amplification fails fast | outer pipeline parse plus the 64 KiB node cap | `alias_amplification_in_a_shaping_node_fails_fast` `shaping_compiler_tests.rs:253` (8⁹ alias expansion refused in < 2 s with a typed error) |
| Unknown keys refused | `deny_unknown_fields` on every raw struct (`split_out.rs:51`, `aggregate.rs:79` and their nested types, `data_shaping.rs` `RawRetain`/`RawLimits`) | `config_refusals` `split_out_tests.rs:53`, `invalid_configurations_are_refused` `aggregate_tests.rs:116` |
| Limits only lower named ceilings | `LIMIT_CEILINGS` `data_shaping.rs:24`, `from_raw` `:760` | `limits_default_to_their_ceilings_and_only_lower_them` `data_shaping_tests.rs:572` (limit and limit+1) |
| Input checked before work | `check_input` `data_shaping.rs:815` | `too_many_input_rows_fail_fast` `shaping_property_tests.rs:435` (10,000 accepted, 10,001 refused) |
| Fail at the first violating row | `Budget::charge_row` `data_shaping.rs:874` | `budget_fails_at_the_first_violating_row` `data_shaping_tests.rs:698`; 400 KiB × 10,000 amplification fails at position ≤ 1 (`split_out_tests.rs:387`) |
| Collection fails fast | `charge_value` lower bound `data_shaping.rs:890` | `budget_value_charges_are_a_lower_bound_of_the_rows` `:734`, `limits_fail_at_the_first_violation` `aggregate_tests.rs:717` |
| No stack growth on adversarial depth | Iterative walks `data_shaping.rs:899`, `:606` | `canonical_keys_refuse_excess_depth_iteratively` `:537` (1,000 deep), `deep_element_fails_with_depth_limit` `shaping_property_tests.rs:417` |
| Checked i64, no floating point, no silent coercion | `exact_i64` `data_shaping.rs:574`, `checked_add` `aggregate.rs:478` | `exact_integers_are_checked_without_floating_point` `:442`, `integers_are_exact_and_checked` `aggregate_tests.rs:620`, P4 against an i128 reference `shaping_property_tests.rs:240` |
| Scalars never wrapped | `split_parent` `split_out.rs:258` | `scalar_at_path_is_a_type_mismatch_never_wrapped` |
| Typed readable errors | `ShapingCode`, `message` `data_shaping.rs:1041` | `shaping_errors_name_only_fields_and_indices` `:789` |
| Policies apply to every row | `aggregate.rs:494`, `split_out.rs:258` | `first_checks_every_row_after_it_found_a_value` `aggregate_tests.rs:414`, `empty_list_policies_still_check_every_parent` `split_out_tests.rs:282` |
| Monotonic limits | Budget arithmetic | P5 `shaping_property_tests.rs:302` |

The last row of "Policies apply to every row" came from the adversarial review and was fixed with tests first.
Cancellation, leases and deadlines are not changed: shaping runs inline and synchronously within one activation,
bounded by the budget, and graph cancellation applies at node boundaries.

## 7. Real-browser evidence

**Stack.** The isolated compose project is `elitea-p5a-shaping`. It was brought up with
`deploy/scripts/standalone-stack.sh` from this branch, using `STANDALONE_WORKER=rust` and an overlay that pins unique
image tags (no shared `:standalone` tag was retagged). It has its own ports: front 18110, OIDC 19510, PG 15510. The
browser host is `p5a-shaping.localhost:18110`, which keeps the session cookie apart from other localhost stacks.

Why this stack: the `elitea-rust-rehearsal` stack runs Redis and cannot run a Worker from `origin/main`, because #1081
deleted that transport. The NATS candidate stack was in use by another session. The user approved this choice.

**Images:**

| Image | Tag | Built from |
|---|---|---|
| Worker, rehearsal | `elitea-worker-rust:p5a-shaping-rehearsal-5adfbd226f8b` (`97a45b5eb8ee`) | `5adfbd22`, `WORKER_REHEARSAL_FEATURES=graph-extensions-rehearsal` |
| Web, rehearsal | `elitea-web:p5a-shaping-rehearsal-35fb15c622e2` (`d1eca3c968bc`), then `…-d3a67246a23b` | `VITE_GRAPH_EXTENSIONS_REHEARSAL=true` |
| Main | `elitea-main:p5a-shaping-5adfbd226f8b` (`cb1c057e9a22`) | this branch, Main code unchanged from `origin/main` |
| Gateway | `elitea-llm-gateway:p5a-shaping-5adfbd226f8b` | this branch |
| Worker and Web, production default | `elitea-worker-rust:p5a-shaping-default-d3a67246a23b`, `elitea-web:p5a-shaping-default-d3a67246a23b` | no feature, flag `false` |

**Fixtures.** Users, the runtime session and the offline mock LLM come from the script's `seed`, `seed-runtime` and
`seed-llm` steps, which write to the database. The pipeline and the conversations were created **through the UI** by
the seeded test user `e2e-chat@autotest.local`. The YAML was entered in the editor's YAML view.

**Identities.**
- Project 90107, pipeline 1, version 1 (saved in place).
- Editor test conversations 1–3, persistent conversation 4.
- Executions:
  - `1b304b22…`: editor test.
  - `a316d1bc…`: persistent success.
  - `c3ab02bc…`: empty input.
  - `8447cf47…`: limit failure.
  - `ae56c5d2…`: production-default refusal.

| # | Acceptance | Observation |
|---|---|---|
| 1 | Add from the node menu; save; reload; YAML retained; Flow↔YAML | The rehearsal menu lists 12 node types, including SplitOut and Aggregate. Adding an unconfigured SplitOut blocks saving with "See the errors on node SplitOut_1". The saved YAML in `p_90107.application_versions` parses equal to the authored YAML. After reload, the Flow view renders both nodes' settings, and the YAML view re-dumps them in block style. |
| 2 | Readable admission errors | `split.path: Use a non-empty RFC 6901 pointer with at most 512 bytes and 32 segments.`; `operations[0]: This name already appears in group_by. Output names are unique across both lists.`; `operations[1].field.null: Integer operations cannot keep null. Select error or skip.` Saving stays blocked. |
| 3 | Editor Test chat and persistent chat run `input → split_out → state_modifier → aggregate regroup: parent` | Input `[{"id":"A","items":[1,2]},{"id":"B","items":[3]}]`. Both surfaces show `orders_out = [{"id": "A", "items": [10, 20]}, {"id": "B", "items": [30]}]`. After a reload, the persistent chat shows the same single result. The checkpoints show one write of `orders_out` and `orders` unchanged. |
| 4 | Empty input and a limit violation | `[]` gives `orders_out = []`. With `limits: {output_items: 2}` and 3 items, the chat shows "The runtime operation failed. Details for support" (generic until Wave 2). The Worker logs `error_code="limit_exceeded" config_field="limits.output_items" item=1 position=0` with no item values. No checkpoint follows `split`. |
| 5 | A production-default build refuses both | The default Web menu lists 10 types, with no SplitOut or Aggregate. The stored pipeline shows ``type: "split_out" is not a node type this runtime can run — the whole pipeline is refused.`` The default Worker refuses the stored YAML with `native_agent.unsupported_capability`, and the chat shows "Configuration type is not supported." |

**Findings during the browser pass:**
- **Fixed in this PR:** the Aggregate help text still described the old separate group and value objects. It now
  uses `aggregateFlatDescription`, commit `d3a67246`. A test fails on the old text.
- **Not in scope:** a terminal list of objects renders as "Pipeline completed." because `select_pipeline_result`
  treats arrays as content blocks. The rehearsal used a final `state_modifier` that renders `orders_out` as text.
- **Not in scope:** the editor Test chat does not persist across reload; the persistent chat does.
- **Environment:** several localhost stacks in one browser profile overwrite each other's session cookie. Use a
  dedicated `*.localhost` host per stack.

## 8. Security

| Threat | Mechanism | Proof |
|---|---|---|
| Item data in errors, logs or events | Errors carry config field paths and indices only (`data_shaping.rs:1041`). The single `warn!` (`:1052`) has no value fields. | `errors_never_leak_data` (`split_out_tests.rs:464`), `errors_never_carry_data_values_or_pointer_text` (`aggregate_tests.rs:992`), log capture in a child process (`shaping_compiler_tests.rs:156`), browser Worker log in §7 |
| Expressions or templates in paths | RFC 6901 only, with `~0`/`~1` escapes (`data_shaping.rs:68`) | `pointer_parsing_follows_rfc_6901_and_the_bounds` (`data_shaping_tests.rs:47`) |
| Unbounded input or amplification | §6 limits | §6 tests |
| Writes to builtin, reserved or other channels | `validate_shaping_channels` (`compiler.rs:2187`); exactly one update | `state_channels_are_validated_at_compile_time` (`shaping_compiler_tests.rs:207`) |
| Gate bypass in production | `SHAPING_INTEGRATION_READY = cfg!(feature)` (`compiler.rs:2013`), the only parser of node types | `shaping_nodes_are_refused_as_not_enabled_while_the_gate_is_off`, `production_builds_refuse_shaping_yaml`, browser item 5 |
| Supply chain | No new dependency. `Cargo.lock` and `package-lock.json` are unchanged. | `cargo deny --all-features check advisories` and `npm audit --omit=dev`: only findings already on main (h2 RUSTSEC-2026-0258, rsa RUSTSEC-2023-0071 via sqlx-mysql, yanked chacha20; katex/mermaid low) |

Checklist (`.claude/rules/security.md`), by category:
- **Trust boundaries and identity:** not applicable. No identity is read, and no header or IP is trusted.
- **Authorization:** no new route or RPC. Channel authority is enforced at compile time (`compiler.rs:2187`) and
  proven by `state_channels_are_validated_at_compile_time`.
- **Input, parsing and amplification:**
  - size is bounded before parsing (64 KiB per node);
  - alias expansion fails fast (`alias_amplification_in_a_shaping_node_fails_fast`);
  - depth and collections are bounded (§6);
  - schemas are strict (`deny_unknown_fields`).
- **Injection and construction:** no SQL, shell, URL or template is built. Paths are RFC 6901 pointers only.
  Web renders through MUI with no raw HTML.
- **Egress and SSRF:** not applicable; there are no outbound calls.
- **Secrets:**
  - none in the diff (pattern scan of added lines: 0 matches);
  - no credentials are handled;
  - errors carry no data.
- **Supply chain:** no new dependency. The audits above report only findings already on main.

Authority, egress, credentials and effects are not applicable: shaping nodes read and write only the activation's
own checkpointed state, call nothing external and resolve no credential.

## 9. Reviews

- **Adversarial review** (subagent, before integration): one confirmed defect, `first` skipping its policies after
  the first value, plus one plausible gap, retention checks skipped for missing or null lists. Both were fixed with
  failing tests first, in commit `35fb15c6`.
- **`code-review` (high)** on the full diff: 6 findings.
  - Fixed in `cb6b51d2`: an exhaustive match in `validate_shaping_channels`, the contract error-format text, one
    group-value resolution per row, `Debug` on `Selected` in tests only, and the bench entry moved behind
    `graph-shaping-bench`.
  - Kept: the Aggregate envelope fast path. It accepts exactly what `parse_envelope` accepts (plain integer text,
    exactly three keys, an object `data`) and otherwise calls the core.
- **`security-review`** on the branch: no findings. Checked:
  - the compile-time gate and the build arguments;
  - channel authority (builtin, reserved and Map-owned paths);
  - RFC 6901-only paths and strict YAML;
  - error and log contents;
  - digest domain separation;
  - the bench module.

## 10. Open limits and follow-ups

- **Wave 2:** public failure kinds `pipeline.shaping_invalid`/`pipeline.shaping_limit` in `protocol/output.rs`,
  a catalog link from `data-shaping-node-catalog.md`, the Map cohort, and the gate flip after
  deployed acceptance.
- **Terminal result rendering:** list results render as "Pipeline completed." in chat (§7). This is a separate
  follow-up.
- **`Debug` derives:** `Pointer`, `RetainSpec` and `FieldSelection` derive `Debug`. They print configuration only,
  not item data. `Selected`, which holds item data, has `Debug` in tests only.
