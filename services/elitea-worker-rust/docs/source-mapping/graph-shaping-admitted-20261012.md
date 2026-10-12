# Split Out and Aggregate admitted by default; node-type admission parity — 2026-10-12

Branch `feat/graph-shaping-admitted`, from `main` `0f8fea2fe` (#1198). Decision (user, 2026-10-12, binding): the
Point 5 node families have **no feature flags**. The switches that gated `split_out` and `aggregate` are deleted from
code, build arguments and tests, not defaulted on. Earlier records
([graph-shaping-split-out-aggregate-20261008.md](graph-shaping-split-out-aggregate-20261008.md),
[pipeline-create-admission-readable-refusals-20261009.md](pipeline-create-admission-readable-refusals-20261009.md))
describe the gates as they were. The contract is [`../split-out-aggregate-contract.md`](../split-out-aggregate-contract.md).

## 1. Business behaviour and what was not ported

- **Behaviour.** A pipeline may contain `split_out` and `aggregate` nodes on every deployment: the editor offers them,
  Main accepts them on every save and run start, and the Worker compiles and runs them. The node semantics are
  unchanged (see the 2026-10-08 record).
- **The current platform** has no shaping node and no switch of this kind. Nothing was taken from it.
- **Removed, not ported:** the "this node type is not available on this deployment" refusal for these two types
  (Main `PIPELINE_NODE_TYPE_NOT_AVAILABLE` save refusal, Worker `NodeTypeNotAvailable` configuration error, Web
  `isDeploymentGatedNodeType`). Nothing can produce it any more. The registered failure kind
  `PipelineNodeTypeNotAvailable` and its message stay, because a direct Toolkit/MCP node on an unserved toolkit still ends
  with it (#1180).
- **Still refused:** a type no layer admits (for example `custom`, `function`) is refused as before.

## 2. Gates deleted

| Layer | Switch | Removal |
|---|---|---|
| Worker | Cargo feature `graph-extensions-rehearsal` and `SHAPING_INTEGRATION_READY` (`compiler.rs`) | Feature and constant deleted; `parse_pipeline_node` has plain `split_out` / `aggregate` arms. `graph-shaping-bench` stays as a pure benchmark feature (`Cargo.toml`), enabling nothing. |
| Worker image | `ARG WORKER_REHEARSAL_FEATURES` (`Containerfile`) | Deleted; it served only this gate. |
| Main | Build tag `graph_extensions_rehearsal` (`shaping_default.go`, `shaping_rehearsal.go`, two tagged tests, one tagged integration test) | All deleted. `admittedNodeTypes` in `pipelinelimits/limits.go` lists the types; one untagged set of tests. |
| Web | `VITE_GRAPH_EXTENSIONS_REHEARSAL` (`runtimeContract.constants.ts`, `Containerfile` `ARG` and `ENV`) | Deleted; both types are always in `CompilerAdmittedNodeTypes`. |
| Docs, compose, Helm, CI | Searched the whole tree | No compose file, Helm chart or workflow referenced any of the four switches. No workflow was changed. Older source mappings keep their historical wording. |

## 3. Changed paths

- Worker: `Cargo.toml`, `Containerfile`, `src/agents/graph/compiler.rs`, `src/agents/graph/mod.rs`,
  `src/agents/runtime.rs`, `src/execution/native_agent_lifecycle.rs`, `src/protocol/output.rs`,
  `src/agents/graph/{shaping_compiler_tests,shaping_pg_tests,pipeline_result_graph_tests}.rs` (run unconditionally),
  `src/agents/graph/shaping_bench.rs` (comment), new `src/agents/graph/node_admission_parity_tests.rs`.
- Main: `internal/domain/pipelinelimits/{limits.go,node_types_test.go}`, new `node_admission_parity_test.go`,
  deleted `shaping_default.go`, `shaping_rehearsal.go`, `node_types_production_test.go`, `node_types_rehearsal_test.go`;
  `internal/transport/runtimegrpc/output/server.go` (comment only); tests that used `split_out` as the example refusal now use
  `custom`: `api/v2/agentexecution/route_test.go`, `application/agentexecution/http_action_snapshot_test.go`,
  `api/v2/applications/pipeline_node_type_admission_postgres_integration_test.go`.
- Web: `Containerfile`, `runtimeContract.constants.ts`, `graphAdmission.nodes.ts`, renamed
  `GraphExtensionsRehearsal.test.tsx` to `GraphExtensionsAdmission.test.tsx`, new
  `runtimeContract.admissionParity.test.ts`, and the count or wording updates in `AddNodeMenu.test.tsx`,
  `ParallelAuthoringGate.test.tsx`, `runtimeContract.constants.test.ts`, `graphShapingAdmission.helpers.test.ts`,
  `CreatePipeline.test.tsx`.
- Shared contract: `libs/proto/contracts/pipeline-node-admission-v1.json`.
- Docs: `split-out-aggregate-contract.md`, `fanout-v2-runtime-design.md` (task 14 no longer says "gate flip" for these
  two types), this file and the README entry.

## 4. Admission parity (new, enforced in code)

`libs/proto/contracts/pipeline-node-admission-v1.json` is the one list of node `type:` values each layer admits:
`admitted` (12 types), `pending_wave_2a` (per layer) and `refused`.

| Layer | Test | What it asserts |
|---|---|---|
| Worker | `node_admission_parity_tests.rs` `the_compiler_admits_exactly_the_contract_node_types` | The string arms of `parse_pipeline_node`, read from `compiler.rs`, equal `admitted` plus `pending_wave_2a.worker`; every such type parses past the unknown-type arm; every `refused` type does not. |
| Main | `node_admission_parity_test.go` `TestSaveAdmissionMatchesTheSharedNodeTypeContract` | `AdmittedNodeTypes()` (the set `Check` uses) equals `admitted` plus `pending_wave_2a.main`; `Check` admits each and refuses each `refused` type with `PIPELINE_NODE_TYPE_UNSUPPORTED`. |
| Web | `runtimeContract.admissionParity.test.ts` | `CompilerAdmittedNodeTypes` equals `admitted` plus `pending_wave_2a.web`; every `refused` type is not admitted. |

**Pending drift, recorded and compared exactly.** `parallel` and `map` are not composed. The Worker parses both
and refuses them when the graph is bound; Main admits both; the Web editor lists `parallel` (the Add-node menu hides it
behind `FIXED_PARALLEL_AUTHORING_ENABLED`) and omits `map`. The file says `pending_wave_2a` must become empty when Wave 2a
composes Parallel and Map. A new type in one layer, or a pending type moving in one layer only, fails that layer's
test.

## 5. Tests

| Suite | Command | Result |
|---|---|---|
| Worker clippy | `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` | clean |
| Worker format | `cargo fmt --all -- --check` | clean |
| Worker, all features, real PostgreSQL 18 (`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`, disposable `postgres:18` container, removed after) | `cargo test --offline --locked --all-targets --all-features` | 1967 passed, 0 failed, 71 ignored (lib 1872 passed, 71 ignored; contract and sandbox-supervisor targets 95 passed). The two `shaping_pg_tests` ran against PostgreSQL. |
| Worker, DEFAULT features (shaping tests now ungated) | `cargo test --offline --locked --lib -- shaping split_out aggregate node_admission_parity pipeline_result_graph taxonomy_tests` | 106 passed, 0 failed |
| libs/rust | `cargo clippy --offline --locked --workspace --all-targets --all-features -- -D warnings`; `cargo test ... --workspace --all-targets --all-features` | clippy clean; 1608 passed, 0 failed (42 test targets) |
| Main | `go vet ./...`; `go test -count=1 ./...` in `golang:1.26.9-bookworm` | vet clean; 202 packages ok, 0 failing |
| Main, real PostgreSQL | `go test -run 'NodeTypeAdmission\|ExistingInadmissible' ./internal/api/v2/applications/` | pass, including a `split_out` create that is now stored |
| Web | `tsc --noEmit`; `npm run lint` (oxlint) | clean |
| Web | `vitest run --project node` (full) | 1704 of 1706 files, 17168 tests pass. Two failures, neither in this change: `src/shared/api/themePreference.test.ts` fails the same way on the unrelated checkout `elitea-platform-rust-consolidated`; `PipelineTestChatIdle.test.tsx` passed when rerun alone (it failed under load beside a Rust build). |
| Web, pipelines feature | `vitest run src/features/pipelines src/pages/pipelines` | 264 files, 2834 tests pass |

## 6. Performance

No hot path changes. The admission check is one map lookup (Main) or one `match` arm (Worker). Budgets, limits and
the shaping benchmark are unchanged: `graph-shaping-bench` still builds the `graph_shaping` bench and nothing else.
Measured: not re-measured, because no mechanism changed.

## 7. Durability

Admission does not change checkpoints, claims or journals. The recovery rule for the two nodes is unchanged: both are in
`recovery_frontier_supported`, and `shaping_pg_tests` (now unconditional) proves a replacement process resumes
without re-running SplitOut, against PostgreSQL 18 (`postgres_shaping_pause_replays_across_process_replacement`,
`postgres_shaping_replay_child`: passed).

## 8. Resilience

- Unknown node types are refused with typed errors at save (Main, `PIPELINE_NODE_TYPE_UNSUPPORTED`, 400, names the node) and
  at compile (Worker, `graph.pipeline.unsupported_capability`). Proven by `shaping_nodes_are_admitted_by_the_default_build`
  (Worker, `custom` refused), the Go parity test (`refused` list) and the Web parity test.
- The shaping limits (limit and limit+1) are unchanged and proven in the 2026-10-08 record. The browser pass in §10
  shows limit+1 refused with no output written.
- Drift is bounded by tests: the contract lists the only allowed differences.

## 9. Security (rules/security.md)

| Category | Applies | How it was checked |
|---|---|---|
| Trust boundaries and identity | No | No route, header or identity path touched. |
| Object-level authorization | No | No route or RPC added or changed. The save-time check runs in the same place as before, on the same write paths. Authorization tests are unchanged. |
| Input, parsing, amplification | Yes, narrowed | The new test reads a JSON file from the repository, not user input. The save-time YAML expansion and size bounds are untouched; the admitted set only gains two types that already had strict `deny_unknown_fields` configuration and the 64 KiB node cap. |
| Injection and construction | No | No SQL, template, shell, URL or path construction changed. |
| Egress and SSRF | No | Neither node performs I/O. |
| Secrets | Yes | Diff scanned for token, key and secret patterns before commit: none. Stack credentials stayed in local scratch files outside the repository and were never printed. |
| Supply chain and deployment | Checked | No dependency changed (`Cargo.lock`, `go.mod`, `package-lock.json` untouched), so no audit was required. A build argument was removed from two Containerfiles. |

Fail-closed behaviour is kept: an unknown type is refused, not coerced. No finding was produced.

## 10. Recovery-guarantee rows touched

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Main × admission (save and run start) | I — Idempotent retry (a pure function of the stored YAML) | `limits.go` `checkNodeType` and `admittedNodeTypes` | Go parity and node-type tests; Postgres create test |
| Worker × admission (compile) | I | `compiler.rs` `parse_pipeline_node` | `node_admission_parity_tests.rs`, `shaping_nodes_are_admitted_by_the_default_build` |
| Worker × tool/model/node execution of the two nodes | R — Resume (unchanged) | `recovery_frontier_supported`, ADK checkpointer | `shaping_pg_tests.rs` against PostgreSQL 18 |
| Shaping limit or data failure | F — Typed failure (unchanged); the chat text is generic | `ShapingError::into_graph_error` | §11 item 4; follow-up below |
| NATS, PostgreSQL, gateway, sandbox, Web/browser | Unchanged | No call added | – |

## 11. Real-browser evidence

**Stack.** Compose project `elitea-shaping`, browser host `shaping.localhost:18140`
(`STANDALONE_HOST=shaping.localhost`, OIDC mock on 19540, PostgreSQL 18141, all bound to 127.0.0.1; none were in use before).
Restored from the real-model dump `product-real-models-main-c0f2e5f9b.dump`; the shared migrations were brought to
this commit's head by `elitea-migrate`. Real models through the gateway (`gpt-5.4-mini`, credential `ai_creds`).
No rehearsal build argument was passed to any build.

**Images built from this branch's code commit `fa5b8d833`** (docs-only commit follows):

| Service | Tag | Image id |
|---|---|---|
| Worker | `elitea-worker-rust:shaping-fa5b8d833` | `dc6868575f4b` |
| Main (also migrate) | `elitea-main:shaping-fa5b8d833` | `bf8c907063a8` |
| Web | `elitea-web:shaping-fa5b8d833` | `24c4733e070a` |

The gateway, scheduler, sub-app host and mock images are the `main-c0f2e5f9b-verify` / `main-0def77b22-verify`
images already on the machine. The inventory engine uses the existing `p5r` image. The `elitea-inventory`
host could not start with the `c0f2e5f9b` sub-app host image (it does not serve the runner this compose file requests), so it
was stopped and the stack ran `--no-deps`. Inventory is unrelated to this change.

**§3b binary check** (`docker create` + `docker cp`, `strings`):

| Check | main `c0f2e5f9b` image | this branch |
|---|---|---|
| Worker contains `graph.pipeline.node_type_not_available` | 1 | **0** |
| Worker contains `graph-extensions-rehearsal` | – | 0 |
| Worker contains the arm texts `a SplitOut node is invalid` / `an Aggregate node is invalid` | yes | yes (now reachable) |
| Main contains `which is not available on this deployment. Remove or replace` | 1 | **0** |
| Main contains `which the pipeline runtime does not support` | – | 1 |
| Web `dist` contains `GRAPH_EXTENSIONS_REHEARSAL` or `is not available on this deployment` | – | 0 and 0 |

The Worker release binary carries no test strings, so the branch-specific proof is the removal of the gated-refusal
code string above, together with the image being built from the checked-out commit.

**Fixtures.** The pipeline was created in the UI at `/app/pipelines/create` as the mock-OIDC user `admin@centry.user`
(project 2, "Private"): name, description and the definition typed into the YAML editor (typed as one line of JSON,
which is YAML); the model `gpt-5.4-mini` was chosen in the editor. The seeded users and models come from the dump.

**Identities.** Pipeline 166, version 192 (project 2). Editor test chat executions:
`529e393e76b7b30a5b6ff1ac881e2c44` (success) and `bb3434e7216e9ece2529fff25ffc5f19` (limit+1 refusal).

| # | Acceptance | Observation |
|---|---|---|
| 1 | The default build offers the nodes | The Add node menu lists 12 types: Agent, Aggregate, Code, Decision, Human-in-the-loop, LLM, MCP, Printer, Router, SplitOut, State modifier, Toolkit. No rehearsal build argument. |
| 2 | Save-time admission | The create page saved a pipeline holding `split_out` and `aggregate` (no inline refusal, no Main refusal): pipeline 166. |
| 3 | YAML round trip | After saving, the editor showed the definition as YAML (key order normalized by the editor); after a reload the Flow view shows all four node cards, the YAML tab matches the stored `instructions` of version 192, and the model `gpt-5.4-mini` persisted (`llm_settings` read from the database). The stored YAML parses to the typed definition. |
| 4 | Run with a real model | Input `[{"id":"A","items":[{"sku":"x","qty":2},{"sku":"y","qty":1}]},{"id":"B","items":[{"sku":"x","qty":3}]}]` gave the answer text "The order has 3 lines and 6 units total." (execution `529e393e…`). Flow: `parse` → `split_out` (3 rows) → `aggregate` (`count_rows`=3, `sum_int`=6) → `llm`. |
| 5 | Limit+1 | With `limits: {output_items: 3}` and 4 lines the run failed with nothing written. The Worker logged `Data shaping node failed; no output was written node_id="split" error_code="limit_exceeded" config_field="limits.output_items" item=1 position=1`. **The chat showed the generic "The runtime operation failed. Details for support", not a readable limit message.** This is the pre-existing gap recorded on 2026-10-08 (readable shaping failures wait for the Wave 2 error registry); this PR does not change it. |
| 6 | Persistent chat | Not recorded. A persistent chat (conversation 881) was created from the editor's Chat button, but the typed message did not send in the narrow shared browser pane, and the pane was shared with another session. The Test chat in the editor is the run surface of record. |

The editor Test chat does not persist across a reload (known); the pipeline definition and model do.

**Teardown.** `docker compose -p elitea-shaping down -v --remove-orphans` ran after the evidence was recorded; the disposable
`postgres:18` test container was removed. The three tagged images remain on the machine.

## 12. Follow-ups

- Readable chat text for shaping limit and data failures (needs the Wave 2 error registry; same gap as 2026-10-08).
- Wave 2a: compose Parallel and Map, then empty `pending_wave_2a` in the contract (the three tests then compare one set).
- A persistent-chat browser run of the shaping pipeline, from a non-shared browser profile.
- `src/shared/api/themePreference.test.ts` fails on current checkouts independently of this change.
