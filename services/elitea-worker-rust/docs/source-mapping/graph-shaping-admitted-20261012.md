# Split Out and Aggregate admitted by default; node-type admission parity — 2026-10-12

Branch `feat/graph-shaping-admitted`, from `main` `0f8fea2fe` (#1198), then merged with `main` `719d12178` (#1195 typed reducers, #1196 C2). Conflicts: this README, `graph/mod.rs` and `shaping_pg_tests.rs`; #1195's ungated `pipeline_pg_test_support.rs` was kept, and the replacing-channel row that #1195 skipped "where SplitOut itself is not admitted" (`state_reducer_compiler_tests.rs`) now runs, because SplitOut is always admitted. #1195 and #1196 add no node-type admission path (reducers are not node types; the parity tests were re-run unchanged). Decision (user, 2026-10-12, binding): the
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

All on the merged head (`5e7442ec9`, code identical to the browser images).

| Suite | Command | Result |
|---|---|---|
| Worker format and clippy | `cargo fmt --all -- --check`; `cargo clippy --offline --locked --all-targets --all-features -- -D warnings` | clean |
| Worker, all features, real PostgreSQL 18 (`ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1`, `ELITEA_TEST_DATABASE_GUARD=disposable-pg18`; disposable `postgres:18`, removed after) | `cargo test --offline --locked --all-targets --all-features` | 1997 passed, 0 failed, 71 ignored |
| Worker, DEFAULT features (shaping, reducer and admission suites, now ungated) | `cargo test --offline --locked --lib -- shaping split_out aggregate node_admission_parity pipeline_result_graph taxonomy_tests state_reducer` | 125 passed, 0 failed |
| libs/rust | `cargo clippy ... --workspace --all-targets --all-features -- -D warnings`; `cargo test ... --workspace --all-targets --all-features` | clippy clean; 1640 passed, 0 failed, 1 ignored (42 targets) |
| Main (Go 1.26.9) | `go vet ./...`; `go test -count=1` for `pipelinelimits`, `api/v2/applications` (real PostgreSQL 18), `api/v2/agentexecution`, `application/agentexecution`, `transport/runtimegrpc/...` | vet clean; all packages ok. The earlier full run of the branch (before the merge) passed all 202 packages. |
| Web | `tsc --noEmit`; `npm run lint` (oxlint) | clean |
| Web, pipelines feature | `vitest run src/features/pipelines src/pages/pipelines` | 265 files, 2843 tests. Alone on an idle machine the files pass. Under load (another session building) one timing-sensitive test of `PipelineTestChat*.test.tsx` failed per run, a different one each time; both files pass when rerun alone (13 of 13, twice). |
| Web, full node project (before the merge) | `vitest run --project node` | 1704 of 1706 files; `themePreference.test.ts` also fails on the unrelated checkout `elitea-platform-rust-consolidated`. |

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

**Configuration: the production profile** (binding rule of 2026-10-12: durability and resilience on by default, the same configuration as the `elitea-code1084` stack). Compose project `elitea-shaping`, browser host `shaping.localhost:18140` (`STANDALONE_HOST=shaping.localhost`; OIDC mock 19540, PostgreSQL 18141; all on 127.0.0.1, none previously in use), restored from the real-model dump `product-real-models-main-c0f2e5f9b.dump`. Real model `gpt-5.4-mini` through the gateway. It ran:

- Worker runtime (`/run/elitea/runtime.json`, derived from `deploy/runtime/worker-runtime.rust.json`): `agent_node_recovery: true` and `agent_model_checkpoint_recovery: true`, with the agent checkpoint connection, plus four `sandbox_runtimes` (Python, JavaScript and TypeScript on the Deno profile with `platform_client`, Rust on the Rust profile).
- Main Code owner recovery on: `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_ENABLED=true` with `ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG` (identity `dns:elitea-code-owner-main`, supervisor audiences `dns:elitea-sandbox-deno` and `dns:elitea-sandbox-rust`), and the workspace, platform and debug-artifact consumers enabled.
- The overlays `deploy/docker-compose.sandbox.yml` and `deploy/docker-compose.code-consumers.yml`, with the supervisor (`--target supervisor`, built from this head: `elitea-sandbox-supervisor:shaping-5e7442ec9`, `d2b463063d0a`) listening on both profiles (9446 and 9447), the AgentState database over TLS (`sslmode=verify-full`, PostgreSQL `ssl=on`).
- Material generated for this stack only, under the stack's own runtime CA (server certificates by `gen-sandbox-certs.sh`, a fresh Main Code owner client certificate, fresh platform content keys, Main's signing keyring, owner-only files in private volumes). Nothing was copied from another stack; the scratch material and volumes were deleted at teardown.
- Not part of the run: the inventory and DeepWiki sub-app hosts (the available `c0f2e5f9b` host image does not serve the runner this compose file requests); the stack ran `--no-deps` without them. They are unrelated to this change.

**Images** (code of `5e7442ec9`, no rehearsal build argument):

| Service | Tag | Image id |
|---|---|---|
| Worker | `elitea-worker-rust:shaping-5e7442ec9` | `ccd8e5c747a3` |
| Main (also migrate) | `elitea-main:shaping-5e7442ec9` | `18369e372837` |
| Web | `elitea-web:shaping-5e7442ec9` | `e1b308e20104` |
| Supervisor | `elitea-sandbox-supervisor:shaping-5e7442ec9` | `d2b463063d0a` |

**§3b binary check** (`docker create` + `docker cp`, `strings`; the earlier head `fa5b8d833` gave the same results):

| Check | main `c0f2e5f9b` image | this head |
|---|---|---|
| Worker contains `graph.pipeline.node_type_not_available` | 1 | **0** |
| Worker contains `graph-extensions-rehearsal` | – | 0 |
| Worker contains `an Aggregate node is invalid` (arm reachable) and `must use the overwrite reducer` (#1195) | – | 1 and 1 |
| Main contains `which is not available on this deployment. Remove or replace` | 1 | **0** |
| Main contains `which the pipeline runtime does not support` | – | 1 |
| Web `dist` contains `GRAPH_EXTENSIONS_REHEARSAL` or `is not available on this deployment` | – | 0 and 0 |
| Supervisor contains `graph-extensions-rehearsal` | – | 0 |

The Worker release binary carries no test strings, so the branch-specific proof is the gated-refusal code string that main has and this head lacks, together with the image being built from the checked-out commit.

**Fixtures (through the UI).** As the mock-OIDC user `admin@centry.user` (project 2, "Private"), at `/app/pipelines/create`: name `shaping-admitted-check`, a description, and the definition typed into the YAML editor (one line of JSON, which is YAML). The model `gpt-5.4-mini` was chosen in the editor. Pipeline 166, version 192. Users and models come from the dump.

**Identities.** Persistent conversation 880 (project 2). Executions: `611af815079c08bbd7abf3276fbfbefb` (success) and `6ae279d0b8abec6ff65dd7daf8a5ecdc` (limit+1 refusal). An earlier run of this branch on the first stack, without the production profile, is recorded in the git history of this file (executions `529e393e76b7b30a5b6ff1ac881e2c44` and `bb3434e7216e9ece2529fff25ffc5f19`).

| # | Acceptance | Observation |
|---|---|---|
| 1 | The default build offers the nodes | The Add node menu lists 12 types: Agent, Aggregate, Code, Decision, Human-in-the-loop, LLM, MCP, Printer, Router, SplitOut, State modifier, Toolkit. |
| 2 | Save-time admission | The create page saved a pipeline holding `split_out` and `aggregate` with no inline or Main refusal. |
| 3 | YAML round trip | After saving, the editor shows the definition as YAML; after a reload the stored `instructions` of version 192 and the model (`llm_settings` read from the database) match. |
| 4 | Real chat, active participant | The editor's Chat button created conversation 880 with the pipeline as participant. In the participants panel the pipeline was selected as the active participant (the composer then shows `shaping-ad… base`). Input `[{"id":"A","items":[{"sku":"x","qty":5},{"sku":"y","qty":7}]},{"id":"B","items":[{"sku":"z","qty":1}]}]` was sent. Answer from the real model: "There are 3 order lines and 13 units in total." (5+7+1 units over 3 lines.) |
| 5 | Reload | After a full reload of `/app/chat/880` the user message, the answer and an "Execution details" link are still there. |
| 6 | Limit+1 in the same chat | With `limits: {output_items: 3}` and 4 lines the run failed with nothing written (Worker log: `Data shaping node failed; no output was written node_id="split" error_code="limit_exceeded" config_field="limits.output_items"`, execution `6ae279d0…`). The chat shows the generic "The runtime operation failed." with "Details for support", and the same after a reload. **This is not a readable limit message**; see §12. |

**Teardown.** `docker compose -p elitea-shaping down -v --remove-orphans`, the three private volumes removed, the disposable `postgres:18` removed, the scratch material deleted. Only the tagged images remain.

## 12. Follow-ups

- **Readable chat text for shaping limit and data failures.** Today the chat shows only the generic "The runtime operation failed. Details for support" for a limit refusal; the typed cause (`limit_exceeded`, `limits.output_items`, node `split`) is visible only in the Worker log. Executions that show it: `6ae279d0b8abec6ff65dd7daf8a5ecdc` (conversation 880, production profile, this head) and `bb3434e7216e9ece2529fff25ffc5f19` (earlier run). The fix needs the Wave 2 error registry (a registered, data-free message for the shaping codes, relayed by Main). Deliberately left generic in this PR.
- Wave 2a: compose Parallel and Map, then empty `pending_wave_2a` in the contract (the three tests then compare one set).
- `src/shared/api/themePreference.test.ts` fails on current checkouts independently of this change.
