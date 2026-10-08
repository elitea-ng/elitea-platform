# Pipeline terminal result: last real writer — 2026-10-08

Branch `fix/pipeline-terminal-result-last-writer`.
Problem: a pipeline that ends on a non-LLM node, or has no LLM node, showed the wrong chat answer or "Pipeline completed.".

## 1. Business behaviour and sources

| Behaviour | Current platform (business reference) | This change |
|---|---|---|
| Which value is the answer | Static topology. It takes the outputs of value-producing nodes that route to END, picks the last declared populated variable, then the last populated state variable in dict order (`elitea_sdk/runtime/langchain/langraph_agent.py` `collect_terminal_output_variables` ~252, `extract_terminal_state_output` ~286, `extract_state_fallback_output` ~185). | The **node that actually wrote last in this turn**, recorded at run time. The static chain stays only as a fallback. |
| Several outputs | The last declared one wins and the rest are hidden. | One JSON object with every output the node wrote, in declared order (user decision). |
| Records and objects | Compact `json.dumps` text, shown as plain Markdown. | Pretty JSON in a `json` code block. |
| Lists of chat content blocks | Joined text (`_is_content_block` ~118). | The same rule for objects. A list of plain strings in state is data and renders as JSON. |
| Scalars | `str()`, so `True` and `5`. | JSON text, so `true`, and the lexical number form. |
| Too large | No bound. | Truncated with a notice. Before this change the Rust worker showed "Pipeline completed." instead. |

**Deliberately not ported:**
- the "last declared wins" ambiguity;
- the dict-order state fallback that can surface an unrelated variable;
- Python repr text;
- the sentinel "Assistant run has been completed, but output is None" with a repr of the last message.

**Rust before this change** (`compiler.rs`, origin/main):
- Only literal `transition: END` counted, and only the first output key of each node.
- Every array was treated as content blocks, so a list of records rendered `""` and the chat showed "Pipeline completed.".
- An oversized result was dropped.

## 2. Changed paths

Worker (`services/elitea-worker-rust/src/agents/graph/`):

**`pipeline_result.rs` (new)**
- Trace channel key: `:18`.
- Result bound: `:21`.
- Trace size bound: `:27`.
- `trace_update`: `:89`.
- `ResultTraceNode`: `:110`.
- `ResultTrace::from_state`: `:172`.
- `into_bounded_text`: `:215`.
- `render_state_value`: `:264`.
- `render_traced_keys`: `:285`.
- Ordered JSON object: `:307`.
- Content-block test: `:328`.

**`compiler.rs`**
- The builder registration point `node()` wraps every top-level node: `:429`.
- `ensure_result_trace_bound` fails closed when a traced node was not bound: `:476`.
- Runtime channel: `:1316`.
- `result_trace_outputs`: `:1602`.
- `select_pipeline_result`: `:1825`.
- Static fallback skips empty defaults: `:1845`.
- Reserved key: `:2320`.

**Mirrors**
- Main: `services/elitea-main/internal/infra/storage/runtime_saved_code_input_policy.go:143` reserves the key.
- Web: `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/runtimeContract.constants.ts` (`ReservedStateKeys`), plus the docs pages `pipeline-node-reference.mdx` and `structure.mdx`.

No file changed by PR #1084 is touched. No dependency, protocol or migration changes.

## 3. Tests

| Suite | Tests |
|---|---|
| `pipeline_result_tests.rs` | 8: scalars, content blocks versus records, fenced objects, ordered multi-key object, char-boundary truncation, declared-output tracing, well-formed trace reading, wrapper passes errors and interrupts through |
| `pipeline_result_graph_tests.rs` | 7 real compiled pipelines (all names below) |
| `compiler_tests.rs` | `the_runtime_trace_selects_the_last_writer_before_the_static_chain`, `static_fallback_skips_empty_collections_but_a_traced_empty_write_shows` |
| `static_pause_tests.rs:126` | `static_pause_and_resume_keep_the_result_trace_of_the_turn` |
| `compiler.rs:2467` | `an_unclaimed_result_trace_entry_refuses_the_graph` |
| Main `runtime_saved_code_input_policy_test.go` | reserved-key case |
| Web `runtimeContract.constants.test.ts` | 26 reserved keys, trace key reserved |

The seven graph tests in `pipeline_result_graph_tests.rs` are:
- `terminal_list_of_records_is_shown_as_fenced_json`
- `router_branch_that_ran_wins_over_a_later_declared_stale_default`
- `cleaning_only_last_node_keeps_the_previous_real_writer`
- `a_nested_pipeline_subgraph_uses_the_same_result_trace`
- `several_written_outputs_render_as_one_object_in_declared_order`
- `a_fresh_turn_starts_without_the_previous_result_trace`
- `the_result_trace_channel_is_reserved_and_does_not_change_the_definition_digest`

Six graph tests were seen failing on the old code for the right reason, for example "Pipeline completed.", the stale default, or `"3"`.

Changed expectations in existing tests:
- `direct_tool_tests.rs` and `pipeline_tests.rs` (an MCP dict result): a dict result now renders as fenced pretty JSON instead of compact text.
- `result_policy_prefers_terminal_data_then_ai_messages_then_declared_state` passes unchanged.

Full suites, at commit `e4983711`:

| Command | Result |
|---|---|
| `cargo test --locked --all-targets --all-features --no-fail-fast` (PG18, receipt tests required) | 2,134 passed, 0 failed, 63 ignored (13 targets) |
| `cargo test --locked` | 1,944 passed, 0 failed, 2 ignored |
| `cargo clippy --locked --all-targets --all-features -- -D warnings`, `cargo fmt --check` | clean |
| `go test ./internal/infra/storage/...` (Main) | pass |
| Web vitest `lib/flow-editor/constants` | 48 passed |
| Web `typecheck`, `lint` | clean |

Skipped: the secure-NATS test (`ELITEA_REQUIRE_NATS_SECURE_TEST`). It needs the CI NATS test server.

## 4. Performance

- **Budget:** no extra database write or round trip per node. The trace is a small object, and it goes into the update the node already emits, so it is saved in the same checkpoint.
- **Measured:** at most 256 keys per trace (`pipeline_result.rs:27`), and one map lookup per node at build time.
- **Rendering:** runs once per turn, and is linear in the result size bounded by `MAX_PIPELINE_RESULT_BYTES` (512 KiB).
- **No per-row or per-chunk work** is added.

## 5. Durability

| Window | Rule | Proof |
|---|---|---|
| A static pause or HITL resume inside a turn | The trace is checkpointed with the node update, and resume keeps it. | `static_pause_and_resume_keep_the_result_trace_of_the_turn` |
| A new turn | Turns start from the defaults (`TurnCheckpointer`), so the trace is null. The previous turn's writer never leaks into the next answer. | `a_fresh_turn_starts_without_the_previous_result_trace`; browser chat 6 (§7) |
| A nested saved pipeline | The child graph is isolated and has its own trace. | `a_nested_pipeline_subgraph_uses_the_same_result_trace` |
| Existing checkpoints and pipeline versions | The definition digest is unchanged, because runtime channels are not digested. A missing trace falls back to the static chain. | `the_result_trace_channel_is_reserved_and_does_not_change_the_definition_digest` |

**Recovery guarantee:** Worker × output delivery is **R — Resume**. The answer is a pure function of the checkpointed state and its trace, so a replacement worker renders the same text. Main, NATS and the gateway are unchanged.

## 6. Resilience

| Rule | Mechanism | Proof |
|---|---|---|
| Bounded output, never dropped | `into_bounded_text` `pipeline_result.rs:215` | `oversized_results_are_truncated_on_a_char_boundary_with_a_notice` |
| Malformed or oversized trace selects nothing | `from_state` `:172`, at most 256 keys | `reads_only_a_well_formed_trace_without_internal_keys` |
| No silent loss of the trace | `ensure_result_trace_bound` `compiler.rs:476` | `an_unclaimed_result_trace_entry_refuses_the_graph`; every existing pipeline test compiles under the check |
| Errors and interrupts unchanged | `ResultTraceNode::execute` | `the_wrapper_adds_one_trace_update_and_passes_errors_and_interrupts_through` |
| No coercion of data | JSON text with the lexical numbers kept | `scalars_render_as_text_and_numbers_keep_their_lexical_form` |

## 7. Real-browser evidence

**Stack:** `elitea-p5a-shaping` (isolated, front `http://p5a-shaping.localhost:18110`). The Worker, Main and Web images were replaced with the branch images, using the same compose overlay pattern:

| Component | Image | Commit |
|---|---|---|
| Worker | `elitea-worker-rust:result-fix-e49837110bc8` (`04ed38f5dfd9`) | `e4983711`, default features |
| Main | `elitea-main:result-fix-8d6b08fcd2c4` (`b29a0de819c3`) | `8d6b08fc` |
| Web | `elitea-web:result-fix-8d6b08fcd2c4` (`a1f6c1052d41`) | `8d6b08fc`; later commits only touched Worker code and a Web citation |

**Fixtures:** pipelines 2–4 and conversations 5–7 were created **through the UI** by the seeded user `e2e-chat@autotest.local` in project 90107.

| Pipeline | Input | Chat answer | Checkpointed trace |
|---|---|---|---|
| 2 "Result Records": a state_modifier writes a list of records, then END (no LLM) | `[{"id":"A","qty":2},{"id":"B","qty":3}]` | A pretty JSON code block of the two records | `{"node":"parse","keys":["rows"]}`, execution `d11737af…` |
| 3 "Result Router": a router chooses `go_left` or `go_right`, both go to END, and `right` has the non-blank default "STALE right default" | `L`, then `R` | "LEFT ran for L", then "RIGHT ran for R" | `go_left/left` (`8ae6e4f6…`), then `go_right/right` (`078e3046…`) |
| 4 "Result LLM": the default LLM template, writing `messages` (regression check) | `Hello pipeline` | "MOCK: Hello pipeline" as plain text | `{"node":"LLM_1","keys":[],"messages":true}` (`443c13f8…`) |

- **After reload**, chats 5–7 show the same answers, one per turn.
- **Without the fix**, the old static rule would show "STALE right default" for input `L`, as the graph test proves.
- **Before the fix**, the same stack showed "Pipeline completed." for a terminal list of records. That was execution `1b304b22…` in the Track A rehearsal, recorded in `graph-shaping-split-out-aggregate-20261008.md` §7.
- **Not shown in the browser:** a node writing several outputs. No node type on this stack writes several declared outputs without a sandbox or a real model. The graph test `several_written_outputs_render_as_one_object_in_declared_order` covers it.
- **Environment note:** one message was lost during a short database timeout in Main ("context deadline exceeded" at 19:16:11Z, under load). The resend succeeded. This is unrelated to the change.

## 8. Security

| Threat | Mechanism | Proof |
|---|---|---|
| Item data in logs | Rendering is pure text building and logs nothing. | Code review; no `tracing` in `pipeline_result.rs` |
| A user state variable spoofing the trace | The key is reserved in the Worker (`compiler.rs:2320`), Main (`runtime_saved_code_input_policy.go:143`) and Web. | `the_result_trace_channel_is_reserved_and_does_not_change_the_definition_digest`; Main and Web tests |
| A Markdown or fence break-out from JSON | Pretty JSON lines start with whitespace, a quote or a bracket, and string newlines are escaped, so a value cannot close the fence. Strings render as before. | `objects_render_as_fenced_pretty_json` |
| Internal keys shown as the answer | `INTERNAL_RESULT_KEYS` and reserved keys are filtered from traces. | `reads_only_a_well_formed_trace_without_internal_keys` |
| Supply chain | No dependency change. | `cargo deny --all-features check advisories`: only findings already on main (h2 RUSTSEC-2026-0258, rsa RUSTSEC-2023-0071, yanked chacha20). `govulncheck ./internal/infra/storage/...`: 7 Go standard-library findings in the local go1.26.5 toolchain (fixed in 1.26.6), already present on main; 0 from this change. |

Checklist (`.claude/rules/security.md`):
- **Identity, authorization, egress, secrets:** not applicable. No route, identity, outbound call or credential is involved.
- **Input bounds:** the trace is bounded to 256 keys and the output to 512 KiB.
- **Strict schema:** the trace is read fail-closed.
- **Injection:** no SQL, shell, URL or template is built.

## 9. Follow-ups

- **ADK fan-in join** (existing defect, flagged as a separate task): a node reached by two direct edges waits for both. A nested pipeline with two branches to END never produces its result. Converging router branches may hang at the root, but this is not verified.
- **Structured Code nodes with `output: []`:** they write keys they did not declare, so they are not traced. The static chain applies.
- **Run-state views:** they may list the trace key next to the other reserved runtime keys. Those UI files belong to PR #1084.
