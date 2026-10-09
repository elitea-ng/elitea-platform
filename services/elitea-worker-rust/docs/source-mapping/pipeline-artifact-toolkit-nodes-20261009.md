# Pipeline nodes on an attached artifact toolkit (2026-10-09)

Branch `fix/pipeline-direct-tool-frozen-scope`. Investigated on `1ab920dde` and rebased onto `origin/main` `5a46ae56f`, which includes #1180, #1181, #1182, #1185 and #1186.

A pipeline whose direct `toolkit` nodes use an attached `artifact` toolkit could not run from chat. The Worker
refused assembly with `native_agent.invalid_input`, "a pipeline direct tool node references a tool outside its frozen
scope", and the browser showed "The execution input is invalid." It was first seen on the reference stack
`elitea-verify-0def77b2` at 2026-10-09T14:16:13Z (project 2, pipeline 165, version 191).

## Root cause

Main froze the scope correctly. The defect was in the Worker.

| Question | Finding |
|---|---|
| Does the toolkit reach the frozen snapshot? | Yes. Main's projection joins `entity_tool_mapping` to `elitea_tools` with no type filter (`services/elitea-main/internal/db/sqlcgen/agent_chat.sql.go:2754-2781`). A NULL `entity_tool_mapping.selected_tools` means "no per-agent restriction", so `elitea_tools.settings.selected_tools` survives (`:2695-2719`). `FreezeCurrentApplicationVersion` derives `toolkit_name = RuntimeName("test") = "test"` (`services/elitea-main/internal/application/agentexecution/tools.go:270-286`). Artifact `unavailable_tools` is catalogue metadata only and is not used by the freeze. |
| Does the Worker admit the node? | Yes. `validate_tool_snapshot` matches the alias exactly, checks kind `Configured`, policy and `selected_tools` (`src/agents/pipeline.rs:287-331`). On the stack, the attached run passed stage `tool_scope`. |
| Where does it fail? | At stage `toolsets`. The pipeline assembler called `materialize_configured_toolsets_with_tokens_and_authorization`, which lends **no** `ArtifactToolAuthority` (`libs/rust/agent-runtime/src/toolkits/materialize.rs:87-99`). `materialize` therefore returns `UnsupportedToolkit` for `artifact` (`materialize.rs:165-169`). The materializer skips the family with a warning (`:121-135`), which the deployed log filter does not show. `build_direct_tool_resolver` then finds no `test` toolset and reports the misleading `invalid_direct_tool_scope`. |
| Why only the artifact family? | Its authority is the live execution claim, not a frozen credential (#906). Only the ordinary agent path lent the claim (`src/agents/ordinary.rs:301-305`). `configuration-toolsets.md` recorded the gap as "pipelines and nested applications still skip it". |
| Was the reported pipeline also missing its attachment? | Its first run (14:16:13Z) happened before the toolkit was attached (mapping created 14:16:58Z) and was never retried. Both states fail on `main`: unattached at `tool_scope`, which is correct, and attached at `toolsets`, which is the defect. Both are reproduced below. |

## Business behaviour

| Topic | Current platform (reference only) | New platform after this change |
|---|---|---|
| Direct `toolkit` node on an attached artifact toolkit | SDK `ArtifactWrapper` tools run from a LangGraph tool node | The root pipeline lends the claim-bound `ArtifactToolAuthority`. `create_file` and `list_files` run under the claim through Main's content listener. |
| Pipeline LLM node selecting artifact tools | Available | Available at the root pipeline. Before this change they were silently absent, and the node failed `Unavailable` at run time. |
| Output declared `dict`, tool returns text | Silently stored (LangGraph does not enforce the declared type) | **Not ported.** A typed `pipeline.result_invalid` stop: "a tool result does not match the node output mapping". No silent coercion, and the effect is not repeated. The reported YAML declares `created: {type: dict}` while `create_file` answers with text, so that YAML now stops at `mk` with this typed message. The same YAML with `created: {type: str}` completes. |
| Artifact toolkit in a saved child pipeline or nested agent | Available | Still skipped. A direct node there is now refused before anything runs, through #1180's readable-refusal path: cause `pipeline.direct_tool.toolkit_not_served`, then `RuntimeFailureKind::PipelineNodeTypeNotAvailable`, then the registered data-free message "This pipeline uses a node type that is not available on this deployment. Open the pipeline to see which node, then remove or replace it." It no longer gets the misleading frozen-scope text. See Follow-ups. |

## Changed paths

| Path | Change |
|---|---|
| `services/elitea-worker-rust/src/agents/pipeline.rs:1022-1023` | The admitted claim authority is owned behind one `Arc`. It is shared and never duplicated, because the artifact family keeps it past assembly. |
| `services/elitea-worker-rust/src/agents/pipeline.rs:453` | `bind_node_runtimes` takes `&Arc<ClaimBoundRuntimeContextAuthority>`. |
| `services/elitea-worker-rust/src/agents/pipeline.rs:493-510` | Lends `ArtifactToolAuthority(ClaimPlatformWriter(platform, claim))` to `materialize_configured_toolsets_with_artifact_authority`, as the ordinary path does. |
| `services/elitea-worker-rust/src/agents/pipeline.rs:2116-2118,2230-2235` | A frozen and admitted toolkit with no materialized toolset is refused as `UnsupportedCapability` (`unserved_direct_toolkit`), not as an out-of-scope input. |
| `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs` (`assembly_failure`) | Routes the new cause code to #1180's existing `PipelineNodeTypeNotAvailable` kind and message. No new message and no Main or Web change. |
| `services/elitea-worker-rust/src/agents/pipeline_artifact_tests.rs` (new) | Assembly, run and refusal proofs on the reported YAML and the real stored toolkit row. |
| `services/elitea-worker-rust/src/agents/graph/node_recovery_artifact_tests.rs` (new) | The real artifact `create_file` tool behind the node-recovery journal. |
| `services/elitea-main/internal/application/toolkitcatalogue/capability.go:123-128` | Comment updated: the artifact family materialises on the root pipeline too; nested positions refuse a direct node at assembly, and an LLM node there finds the tools unavailable at run time. |
| `services/elitea-worker-rust/docs/source-mapping/configuration-toolsets.md` | The artifact row records the new position and the remaining gaps. |

## Crate ownership (ADR-0027 layout)

| Mechanism | Owner | Changed here? |
|---|---|---|
| `artifact` toolkit family: the tools, `ArtifactToolkitConfig`, `ArtifactToolAuthority` | `libs/rust/agent-runtime` (`src/toolkits/families/artifact/`) | No |
| Lending the authority to the family during materialization (`materialize_configured_toolsets_with_artifact_authority`), and skipping a family whose authority is absent | `libs/rust/agent-runtime` (`src/toolkits/materialize.rs`) | No |
| `PlatformWriter` host trait | `libs/rust/agent-runtime` (`src/host`) | No |
| `ClaimPlatformWriter`, the Worker's implementation of the trait over the claim-bound runtime-context routes | Worker (`src/transport/platform_writer.rs`) | No |
| Root-pipeline wiring: owning the claim behind an `Arc`, building the authority, calling the lending materializer | Worker (`src/agents/pipeline.rs`, pipeline assembly) | **Yes** |
| Direct-node resolver refusal and its cause code (`UNSERVED_DIRECT_TOOLKIT_CODE`) | Worker (`src/agents/pipeline.rs`) | **Yes** |
| Mapping the cause to #1180's readable refusal (`assembly_failure`) | Worker (`src/execution/native_agent_lifecycle.rs`, protocol mapping) | **Yes** |
| Node-recovery journal for effectful direct tools | Worker graph (`src/agents/graph/direct_tool.rs`) over the `agent-runtime` node_recovery codec | No |

No shared-crate code or dependency changed, so `libs/rust` behaviour is identical. Both workspaces were still tested
in full (see below), and the Worker image was rebuilt and binary-checked.

## Tests

The first three were written first and failed on `main` (`1ab920dde`). The two assembly tests failed with the
exact production message "a pipeline direct tool node references a tool outside its frozen scope". The refusal
test got `InvalidInput` instead of `UnsupportedCapability`. The saved-child test was added after code review.

| Test | Proves |
|---|---|
| `pipeline_artifact_tests.rs:138` `artifact_direct_toolkit_nodes_assemble_against_the_attached_toolkit` | The reported YAML assembles against Main's frozen row: `selected_tools` lists the SDK catalogue including unserved names, plus `available_tools`, `embedding_model` and `pgvector_configuration`. Nothing reaches the platform at assembly. |
| `pipeline_artifact_tests.rs:149` `artifact_list_node_runs_under_the_claim_and_projects_its_listing` | A `list_files` node runs to completion and makes exactly one call, on `/runtime-context/artifacts/list`. The listing reaches the browser output. |
| `pipeline_artifact_tests.rs:186` `an_unserved_artifact_toolkit_is_refused_as_unsupported_not_out_of_scope` | Without a claim-bound platform the refusal is `UnsupportedCapability`, names the direct tool node, and never says "outside its frozen scope". |
| `pipeline_artifact_tests.rs:208` `a_saved_child_pipelines_artifact_node_is_refused_as_unsupported` | The production-reachable unserved position (a saved child pipeline) refuses its artifact direct node as `UnsupportedCapability` at root assembly. On `main` this was `InvalidInput`, "outside its frozen scope". |
| `native_agent_lifecycle.rs` `taxonomy_tests::an_unserved_direct_toolkit_ends_as_the_registered_deployment_message` | The unserved refusal ends as `UNSUPPORTED_CAPABILITY` with #1180's registered message, not retryable, through the real `assembly_failure` → `runtime_error_policy` chain. |
| `node_recovery_artifact_tests.rs:141` `artifact_write_runs_once_and_its_committed_result_is_replayed` | `create_file` is `!is_read_only()` and runs behind the fenced `Started` journal. One write; a lost step checkpoint replays the committed result with no second write. |
| `node_recovery_artifact_tests.rs:175` `a_started_artifact_write_without_a_result_is_never_written_again` | Crash between `Started` and the result: re-entry shows the reconciliation card twice, with 0 writes. |
| `node_recovery_artifact_tests.rs:196` `a_text_write_result_is_not_coerced_into_a_dict_output` | The reported `dict` declaration produces a typed `state_projection` failure after one write. Re-entry reconciles and does not write again. |

Counts (local, on the rebased branch, `--offline --locked`):
- Worker lib: 1746 passed, 0 failed, 71 ignored. The ignored set is the same DB-, Docker- and Kubernetes-gated tests as on `main`.
- All Worker targets (`--all-targets --all-features`): 1841 passed, 0 failed, 71 ignored.
- Worker `cargo fmt --check` and `cargo clippy --all-targets --all-features -- -D warnings` are clean.
- `libs/rust`, unchanged by this branch:
  - `cargo test --workspace --all-targets --all-features`: 1007 passed, 0 failed;
  - `elitea-agent-runtime` with `--features toolkit-sql`: 510 passed;
  - `elitea-agent-runtime` with `--features test-preserve-order,toolkit-sql`: 510 passed;
  - `fmt --check` and `clippy --workspace --all-targets --all-features -D warnings` are clean.
- DB-gated real-PostgreSQL journal proofs `state::postgres_checkpointer_tests::direct_tool_journal` (`--ignored`, throwaway pgvector 0.8.1/PG18 container): 6 passed. They cover crash after `Started`, process replacement, second-claim takeover and replay. The journal is family-agnostic, and the artifact write reaches it because `create_file` is effectful.

## Performance

| Rule / budget | Mechanism | Proving test | Measured |
|---|---|---|---|
| No extra work at assembly | The authority is two `Arc` clones. No platform round trip until a node calls a tool. | `pipeline_artifact_tests.rs:138` asserts no runtime-context path at assembly | 0 calls |
| One round trip per artifact call | One claim-bound POST per operation (`src/transport/runtime_context.rs:1095-1104`) | `pipeline_artifact_tests.rs:149` asserts exactly one path | 1 call per `list_files` |
| Bounded results | Existing family caps: 200,000-character read, list page limit, 2 MiB artifact response, direct-node and journal result bounds | Existing `toolkits::artifact_tests` and `direct_tool` bound tests | Unchanged |

## Durability

| Rule | Mechanism | Proving test |
|---|---|---|
| Durable intent before the effect | `create_file` is effectful (`libs/rust/agent-runtime/src/toolkits/families/artifact/tools.rs:647-649`, `create_file`). The direct node dispatches it only after the fenced `Started` append (`src/agents/graph/direct_tool.rs:432-437,551-571`). | `node_recovery_artifact_tests.rs:141`; real PG `direct_tool_journal` 6/6 |
| No repeat after an unknown outcome | A `Started` record without a result is never re-dispatched; the operator reconciliation card is shown. | `node_recovery_artifact_tests.rs:175,196`; real PG `crash_after_started_before_result_never_repeats_the_effect` |
| Committed result survives replacement | The journal result is replayed | `node_recovery_artifact_tests.rs:141`; real PG `committed_effect_result_survives_process_replacement` |
| Writer fencing | Every append and every content-listener call carries the claim fence | real PG `second_claim_takeover_fences_the_old_writer_and_keeps_the_started_effect` |

## Resilience

- Every failure is typed and readable.
  - Wrong declared type: `pipeline.result_invalid`, "a tool result does not match the node output mapping".
  - A family a position cannot serve: `unsupported_capability`.
  - A platform failure: the family's own readable text (`tools.rs:186-205`).
- No silent coercion. Text is never stored into a `dict` output (`direct_tool.rs:1579-1602`).
- A missing node-recovery writer refuses the effectful tool before any call (`direct_tool.rs:432-437`). The standalone and Helm configurations enable `agent_node_recovery` (`deploy/runtime/worker-runtime.rust.json:21`, `deploy/helm/elitea/templates/worker/configmap-runtime.yaml:66`).

## Security

| Category (`rules/security.md`) | Applies? | How checked |
|---|---|---|
| Trust boundaries / identity | Yes | The claim authority is minted only by Worker admission and is never cloneable or formattable. It is shared through `Arc`, never copied. Main derives project and actor from the claim, not the body: `services/elitea-main/internal/infra/storage/runtime_artifact_object_test.go:82` `TestArtifactRoutesActOnTheClaimsProjectAndActor`. |
| Object-level authorization | Yes, unchanged routes | Same claim-bound content routes as the ordinary path (#906); no new route or RPC. Foreign-project artifact access refused: `services/elitea-main/internal/api/router_security_test.go:480`. |
| Input / amplification | Yes | Existing family bounds (bucket and key shape, prefix length, read cap) and Main's `TestArtifactWriteRefusesOversizedAndUnknownBodies`. |
| Injection / construction | Yes | Paths are built with per-segment encoding (`runtime_context.rs:1100`); bucket and key travel in the JSON body, never in the path. |
| Egress | No | No outbound HTTP: calls go only to Main's private claim-bound listener. |
| Secrets | Yes | No credential is involved: the authority is the claim. New error strings are `&'static str` with no data. The diff secret scan was clean. |
| Supply chain | Yes | No dependency change. `cargo deny --all-features check advisories`: only pre-existing RUSTSEC-2023-0071 (`rsa` via `sqlx-mysql`, recorded unreachable in `dependency-advisories-20261008.md`). `govulncheck` on Main: no called vulnerabilities (pre-existing imported-only findings). |

## Recovery guarantee rows

Phase = tool call. Each row names the touched component.

| Component × phase | Class | Enforcing code | Proof |
|---|---|---|---|
| Worker × admission (assembly of artifact toolsets) | I | No effect at assembly. A re-claim re-assembles from the same frozen snapshot. | `pipeline_artifact_tests.rs:138` (0 platform calls at assembly) |
| Worker × tool call, effectful direct `create_file` | C at attempt level; F at run level (pre-existing G-WORKER-07) | Fenced `Started` journal (`direct_tool.rs:551-571`) | `node_recovery_artifact_tests.rs:141,175,196`; real PG `direct_tool_journal` 6/6 |
| Worker × tool call, read-only direct `list_files` | I | Read-only and not journaled; the re-run is side-effect free. | `pipeline_artifact_tests.rs:149` |
| Worker × tool call, artifact tool chosen by a pipeline LLM node | F (pre-existing P04 row, G-WORKER-04/10) | Model-boundary restore refuses `ToolMayHaveStarted` (`src/agents/model_checkpoint.rs:283-292`) | `../recovery-guarantees.md:271` |
| Main × tool call (content listener down mid-write) | F (pre-existing Main × P05 row) | The Worker gets a transport failure and the family answers with readable failure text. The journal records the attempt, which is never repeated. | `../recovery-guarantees.md:233`; `node_recovery_artifact_tests.rs:175` |
| PostgreSQL × tool call | F, no effect | The `Started` append fails before dispatch | real PG `superseded_claim_cannot_start_an_effect_in_an_unopened_activation` |
| NATS, LLM gateway, sandbox supervisor, Web | Not touched | — | — |

No new L.

## Real-browser evidence

Own standalone stack `elitea-dts` (`http://dts.localhost:18220/app/`), restored from the real-model dump
`product-real-models-main-1ab920dde.dump`. Mock OIDC login as `admin@centry.user` (user 3, Private project 2). All
fixtures were created through the UI: the pipelines through the editor, the toolkit attachment through Tools →
Toolkit, and bucket `test` through Artifacts → New bucket, because the fresh object store had none. The dump supplied
toolkit `test` (project 2, id 5). No response mocks.

Images: Main, Web and gateway `main-1ab920dde-verify`. Worker baseline `main-1ab920dde-verify`, binary sha256
prefix `095076e44d72b63f`. Worker fix `elitea-worker-rust:dts-artifact-scope`, image `sha256:1d43fd1fe29b…`, built
from this branch, which contains #1160 (`524165ed09`). Binary check: `docker create` + `docker cp` of
`/usr/local/bin/elitea-worker-rust`. The fix binary (sha256 prefix `703c5f9281bbe926`) contains the branch-only
string "cannot serve in this position" once; the baseline binary contains it zero times.

| # | Image | Pipeline (version) | Chat | Execution | Result |
|---|---|---|---|---|---|
| 1 | baseline | 164 (190), reported YAML, toolkit **not** attached | 874 | `c0ab0070a574c51074af02db95aef135` | "The execution input is invalid." Worker: stage `tool_scope`, `invalid_direct_tool_scope`. This is the correct refusal and matches the reference 14:16:13Z run. |
| 2 | baseline | 164 (190), toolkit attached (`entity_tool_mapping` 119, `selected_tools` NULL) | 875 | `a361b38af886a992a9870e0053baec64` | Same browser text. Worker: passed `tool_scope`, failed at stage `toolsets`. This is the defect. |
| 3 | fix | 164 (190), reported YAML unchanged, bucket created | 875 | `234bc6a41000f17b96b7f01cc53f5ce3` | Assembly completed. `mk` wrote `dts-baseline-edge.txt` once (Artifacts shows one 16 B object), then stopped with "The pipeline stopped because a tool result does not match the node output mapping. Check the required fields and their types. Later nodes did not run." (`pipeline.result_invalid`, declared `dict`). |
| 4 | fix | 165 (191), same nodes with `created: {type: str}` | 876 | `ca08e48c486a7b21e2bd7480918445fd` | SUCCEEDED. Reply shows the `list_files` listing: bucket `test`, `dts-fixed-edge.txt`, 16 bytes. It survives a full page reload, and Artifacts → `test` lists the file. |

Runs `feab3ecd…` and `9d4be1c4…` (fix image) were made before bucket `test` existed. Each family call answered with
readable failure text, which the `dict` output then refused with the typed message. This shows the
"no silent coercion" path rather than the fix.

Observed in passing (not changed here): after attaching a toolkit, the editor's **Chat** button raises "You have
unsaved changes" although the attachment is already persisted.

## Follow-ups

1. Lend the claim to the artifact family in a **saved child pipeline** (`bind_saved_pipeline_runtimes`,
   `src/agents/pipeline.rs:1955`) and in **nested agents** (`src/agents/application_tools.rs:3197`). The borrowed
   `&ClaimBoundRuntimeContextAuthority` must become a shared `Arc` through about ten signatures in `pipeline.rs`,
   `pipeline/composition.rs` and `application_tools.rs`. Until then those positions refuse a direct node with a
   typed `unsupported_capability`. An LLM node in those positions still finds the artifact tools unavailable
   only when it runs (`LlmExecutionError::Unavailable`), not at assembly.
2. The materializer's `agent_toolkit_skipped` warning comes from the `elitea_agent_runtime` crate, which the
   deployed `ELITEA_RUST_LOG` filter does not show. Surface it so a skipped family is visible in Worker logs.
3. Web: the false "unsaved changes" prompt after a toolkit attachment (see above).
4. Optional product decision: whether the editor should warn when a direct node's declared output type cannot hold
   the tool's result type (`create_file` → text).
