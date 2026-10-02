# Main instruction snapshot authority

## Source contract

The current sources were read on 2026-09-09. This mapping describes Main admission and the shared SDK boundary. Rust durable activation and compaction are mapped by the instruction authority runtime work.

| Current source | Required behavior | Main owner |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/utils/predict_utils.py::get_project_context` | Read project configuration; default enabled to true; keep content bytes; derive content SHA-256 | `internal/infra/db/repos/agent_project_context.go::ResolveCurrentAgentProjectContext` |
| `projects/centry/pylon_main/plugins/elitea_core/utils/project_context_utils.py::prepare_project_context_delivery` | Empty or disabled content is absent. Empty activation description means eager instructions. Other content is disclosed on demand | `agentexecution/instruction_snapshots.go::freezeCurrentInstructionSnapshots`; Python `sdk_adapter.py::_project_context_delivery` |
| `projects/centry/pylon_main/plugins/elitea_core/rpc/chat_all.py` application and ad-hoc branches | Skip application pipelines and `meta.ignore_project_context`; use project content in ordinary chat | Shared `CurrentApplicationToolSnapshotService`; application and ad-hoc input builders |
| Core skill projection and stored `entity_skill_mapping` | Resolve skill and selected version content before invocation; retain invoked, attached and applied channels | `agent_chat.sql` skill projections; `agentexecution/skills.go::projectCurrentApplicationSkills` |
| SDK `runtime/clients/client.py::application`, `predict_agent`, `_inject_project_context` | Pass on-demand context to the existing middleware | Python `sdk_adapter.py::execute_application`, `execute_adhoc` |

Paths under `agentexecution` refer to `services/elitea-main/internal/application/agentexecution`. Python paths refer to `services/elitea-worker-python/src/elitea_worker`.

## Authority and persistence

The project and actor come from Main's authorized turn resolver. Nested version reads use the claim-bound project and actor. The project reader takes no schema, content, author, or revision from the client. It reads the project configuration in a read-only tenant transaction. A failed read fails admission. A missing or disabled configuration yields no snapshot. The shared freezer deletes a supplied `project_context` before it reads the source. Its resolver is a required constructor dependency.

Main freezes the exact content as UTF-8 and computes a lowercase SHA-256 hexadecimal revision. It does not use `updated_at`: a content digest identifies the bytes even when the source has no explicit version. Skill identities use `skill:<skill_id>:version:<skill_version_id>`. A legacy relation without a version uses `base`. The revision still pins its exact body. Skill version IDs, snapshot IDs, revisions and project scopes survive the invoked, attached and applied projections. Applied metadata carries no instruction body; the runtime resolves it against the frozen catalog.

Project Context uses `project-context:<project_id>:<configuration_id>`, with source scope `project:<project_id>`. `ProjectContextSnapshotV1` is field 64 of `AgentExecutionInputV1`; fields 40 through 63 remain reserved. The same snapshot is in a nested `version_details.project_context` response. That response retains the existing claim, lease and fencing checks in `internal/infra/storage/runtime_application_version.go`.

`InputBundleFactory.Build` serializes the typed snapshot into the existing immutable input data plane. It verifies the content revision before storage. Prompts do not enter Redis command envelopes. No database migration was added. Main does not invent an activation time: Rust derives `activated_run_id` from the initial immutable request version and persists the active tuple and frozen catalog in session state. Resume reuses that state even if a later Main read sees changed source content. Fresh independent turns read the current source again. Compaction prose is not the authority.

## Admission changes and compatibility

The four `project_context` exclusion predicates were removed from `agent_chat.sql` and sqlc output was regenerated. A configured context can now reach the freezer instead of causing an unsupported-turn response. Other admission predicates retain their existing behavior.

The Python contract reader validates the typed snapshot digest. The adapter forwards nonempty activation descriptions to the existing SDK `project_context` argument. For eager delivery it prepends the current Core `# Project Context` section to a copy of the application or ad-hoc instructions. No separate Python middleware or instruction engine was added.

## Verification

- `TestCurrentInstructionSnapshotFreezesSourceAndPreservesSkillProjection`: source identity, exact content, revision, and all three skill projection channels.
- `TestCurrentInstructionSnapshotDeliveryModesAndFailure`: eager, disabled, empty, pipeline, ignored and read-failure cases; forged input is removed.
- `TestCurrentInstructionSnapshotReachesApplicationAndAdhocBundles`: both builders, durable protobuf round trip, and digest mismatch rejection.
- `TestCurrentInstructionSnapshotsRequireSource`: missing resolver cannot produce a service.
- `TestPostgresCurrentAgentProjectContextSnapshotAndAdmission`: real tenant read, default enabled, source edits, immutable prior value and application admission with configured context. The test uses an isolated database.
- `TestNestedApplicationVersionServesTheFrozenClaimScopedDefinition`: real shared freezer through the claim-scoped HTTP route, nested project identity, content and revision.
- Python `test_project_context_snapshot_reaches_existing_sdk_boundary`: application and ad-hoc, eager and on-demand, copied source payload.
- Python `test_project_context_wire_snapshot_rejects_revision_mismatch`: typed wire decode and content mismatch refusal.
- Python public application and tool trace tests exclude the Project Context body from browser events.

Go and Python protobuf clients were regenerated with `scripts/contract/generate_proto.sh` and its pinned tool versions. sqlc generation used v1.31.1. Focused Python tests pass. The broader local `test_agent.py` run encountered an unrelated SDK import failure (`elitea_sdk.tools.utils` while loading the question tool); it is not evidence of full Python SDK runtime verification.

## Change history

2026-09-09: Add Main-owned instruction snapshots and field 64. Remove obsolete Project Context admission exclusions. Preserve the shared Python constructor contract. Add source, projection, bundle, database and nested-route tests. Durable Rust activation and compaction validation remain separately owned runtime checks.

2026-09-14: Reconcile the preserved work with the current branch after point 3 acceptance.
An isolated candidate contains the snapshot contract, its complete Main constructor and input-builder wiring, and Python compatibility changes.
It excludes pending Rust instruction-state work, legacy builder catalogue widening, and pipeline HITL history changes.
The pinned protobuf generator reproduces all 54 generated files in the current worktree.
This includes the previously pending `node_event.pb.go` comments for an already committed protocol field; those comments change no wire behavior.
sqlc v1.31.1 regenerates the exact staged `agent_chat.sql.go` snapshot.

The isolated candidate passes 82 focused Go checks with no skips, including the real PostgreSQL Project Context read and admission test.
Eight Python boundary and public-event redaction checks pass; 86 unrelated tests are deselected.
Go vet passes for agent execution, runtime storage, and composition.
Logs: `elitea-point4-authority-tests-final.log`, `elitea-point4-authority-python.log`, `elitea-point4-authority-vet.log`, and `elitea-point4-authority-proto.log`.
The first candidate includes an unrelated pending builder-catalogue test without its SQL changes and fails that test.
Removing that unrelated test from the candidate restores its matching committed baseline; the test and its SQL work remain preserved for the module gate.
No product database migration or deployment occurs in this reconciliation.
This commit protects the instruction snapshot boundary; it does not complete durable compaction or activate context settings in Main.
