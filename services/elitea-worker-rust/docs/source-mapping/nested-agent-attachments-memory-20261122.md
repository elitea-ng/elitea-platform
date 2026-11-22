# Nested saved-agent attachments, agent memory, artifact, and memory toolkits (FU-353)

Date: 2026-11-22
Branch: `feat/fu353-nested-agent-tools` (base `4f9d63873`)
Commits: `529d1ee1c` (C1 Main nested agent memory), `a6a6fe828` (C2 Main hidden memory toolkit defense-in-depth), `4eceae59b` (C3 `agent-runtime` bucket-pinned artifact authority), `f57c675d0` (C4 Worker nested `artifact`/`memory` toolkits + nested saved pipelines), `7cb6082db` (C5 Worker nested `attachments` + `agent_memory` + version-skew).

## 1. Scope and approved decisions

Closes FU-353 under ADR-0044 (`own(root) ∩ P` monotonic non-widening delegation) without widening grants or breaking `GrantSet` digests:

- **D1(a) — Nested `attachments` (`elitea_attachments`):** Strict monotonic caller ceiling. A nested saved Agent binds `list_attachments`/`read_attachment`/`search_attachment` if and only if BOTH its caller permits `attachments` AND the nested Agent's own `meta.internal_tools` enables `"attachments"`. Ad-hoc roots inherit from the conversation without mutating the ad-hoc root's own toolset; saved ordinary roots and pipeline roots require `"attachments"` on their own shell (`profile.internal_tools().attachments_enabled()`). Narrowing propagates monotonically across multi-hop nested Agent and saved Pipeline chains, and the assembly cache key includes `caller_attachments_enabled` to prevent diamond cache poisoning.
- **D2(a1) — Nested `agent_memory` (`recall`/`remember`/`forget`):** Authorized server-side in Main on each claim-bound `agent-memory/{remember,recall,forget}` RPC via optional `(target_application_id, target_version_id)`. Main verifies bounded BFS reachability (max depth `10`, max visited `64`, cycle-safe, same-project strict check for both integer and string `project_id`) from the frozen root execution input, checks the target is not retired (`retiredAgents`), verifies 3-state folder visibility for the claim actor (`nestedApplicationHiddenFromActor`), parses the target version's own `meta.memory`, and records delegated provenance in audit reasons (`delegatedReason`).
- **D3(a) — Nested Agent Memory write safety:** Nested targets force `Attended = false` (so `auto_attended` lands in `pending` for human review) and cap `write: "auto"` by `max(root_tier, target_tier)` (`capAutoWriteForAgents`).
- **D4(a) — Delegated `artifact` bucket pinning:** `ArtifactToolAuthority::with_pinned_bucket()` pins every delegated artifact tool (`list_files`, `read_file`, `create_file`, `delete_file`, `read_multiple_files`, `grep_file`, `edit_file`, `append_data`, `get_file_metadata`) to its configured bucket, refuses cross-bucket `bucket_name`/`filepath` overrides without calling `PlatformWriter`, and refuses `create_new_bucket`. Nested agents and nested saved pipelines receive a plain pinned `ClaimPlatformWriter` authority (no Office, Web, or Browser hosts).
- **D5(a) — Nested internal tools boundary:** Nested agents bind `recall`/`remember`/`forget` tools only (no start-of-turn prompt recall, isolated `RecalledEntries::default()`, shared `UntrustedReads`). `skills_builder`, `project_context_builder`, `planner`, `subagents`, `data_analysis`, `pyodide`, and `image_generation` remain root-only.
- **D6(a) — Nested saved pipelines:** `governed_nested::materialize_nested_toolsets` receives pinned `artifacts` and claim-bound `index_tools` for both nested saved Agents and nested saved Pipelines (`bind_saved_pipeline_runtimes`), while Office/Web/Browser hosts stay absent on nested paths.

## 2. Implementation ownership (`path:line`)

| Layer | File | Ownership |
| --- | --- | --- |
| Main (RPC) | `services/elitea-main/internal/infra/storage/runtime_agent_memory.go` | Optional `TargetApplicationID`/`TargetVersionID` wire fields, `parseAgentMemoryTarget` pair check, `DelegatedFromApplicationID`, sink target mismatch guard |
| Main (AuthZ) | `services/elitea-main/internal/infra/db/repos/agent_memory.go` | `ResolveAgentMemoryRun` bounded BFS reachability (`resolveReachableNestedVersion`), strict same-project int/string check, retired & folder-hidden refusal, `capAutoWriteForAgents` `max(root, target)`, `Attended = false`, `delegatedReason` audit provenance |
| Main (Admission) | `services/elitea-main/internal/application/agentexecution/admission.go`, `services/elitea-main/internal/runtimecomposition/composition.go` | Defense-in-depth `HiddenMemoryToolkits` filtering on `execution_jobs.memory_toolkit_ids` at run admission (`filterHiddenMemoryToolkits`, fail-closed on lookup error) |
| `agent-runtime` | `libs/rust/agent-runtime/src/toolkits/families/artifact/{mod.rs,tools.rs,extra_tools.rs}` | `ArtifactToolAuthority::with_pinned_bucket()`, `check_bucket_scope` across all 10 artifact tools, `create_new_bucket` refusal |
| Worker (Nested toolkits) | `services/elitea-worker-rust/src/agents/{governed_nested.rs,application_tools.rs,ordinary.rs,pipeline.rs}` | Plumb plain pinned `ArtifactToolAuthority` and lazy `IndexToolAuthority` (`nested_application_count > 0`) into nested Agents and nested saved Pipelines (`D4=A`, `D6=A`) |
| Worker (Attachments & Agent Memory) | `services/elitea-worker-rust/src/agents/{assembly.rs,agent_memory_tools.rs,application_tools.rs,ordinary.rs,pipeline.rs}`, `services/elitea-worker-rust/src/transport/runtime_context.rs` | Nested `meta.memory` parsing, `#[serde(skip_serializing_if = "Option::is_none")]` target fields, `AgentMemoryAuthority::for_nested_target` (isolated `RecalledEntries`, shared `UntrustedReads`), monotonic `caller_attachments_enabled` with diamond-safe cache key `(identity, delegation_key, caller_attachments_enabled)`, reserved toolsets `elitea_attachments` + `agent_memory`, `parallel_applications` guard |

## 3. Delivery gate notes

- **Performance:** No extra database queries on root-only agent memory calls (`target_application_id` omitted). Nested reachability walks only the frozen root snapshot in memory (bounded at depth 10, 64 visited nodes) and performs targeted lookups only for the resolved child. Ordinary root `IndexToolAuthority` uses `LazyStepbackModel`, so no extra LLM or vector call occurs unless a nested tool actually executes.
- **Durability & Recovery:** Nested `agent_memory` and `artifact` writes are claim-bound RPCs with no new Worker checkpoint format or schema migration. Version skew is safe: **Main ships before the Worker** (`docs/upgrading/2026-11-22-0-nested-agent-attachments-memory.md`). Root calls omit both optional JSON fields (`DisallowUnknownFields`-safe on older Main); nested calls against an older Main fail closed with terminal `dependency_unavailable` → tool error `"memory_unavailable"` without falling back to root memory.
- **Resilience:** Principal hidden-toolkit lookup failure fails closed at admission. Nested `memory` toolkits without a vector token withhold cleanly (`IndexToolsWithheld::NoVectorToken`) without crashing assembly. Reserved toolset enforcement prevents user MCP/configured toolkits from shadowing or renaming `elitea_attachments` or `agent_memory`.
- **Security:** Enforces monotonic non-widening (`own(root) ∩ P`), server-side reachability + retirement + folder visibility + `max(root, target)` tier capping + `Attended = false`, pinned artifact buckets with zero cross-bucket `PlatformWriter` calls, isolated `RecalledEntries` (preventing nested `recall` from forging root `N·TAG` citations), and zero secret/payload logging.

## 4. Verification evidence

- **Go (Main):**
  - `go vet ./internal/infra/storage/... ./internal/infra/db/repos/... ./internal/application/agentexecution/... ./internal/runtimecomposition/...`: PASS
  - `go test -race -count=1 -run "TestRuntimeAgentMemory|TestResolveReachableNestedVersion|TestDelegatedReason" ./internal/infra/storage ./internal/infra/db/repos`: PASS (all unit tests, incl. `DisallowUnknownFields` version-skew, foreign int/string `project_id`, cycles, depth/node caps, provenance)
  - `go test -race -count=1 -run "TestMemoryToolkitIDs|TestRunMemoryToolkits|TestRunToolkitTypes" ./internal/application/agentexecution`: PASS (`4/4`)
  - PostgreSQL integration tests (`TestPostgresCapAutoWriteForAgentsUsesMaxRiskTier`, `TestPostgresNestedAgentMemoryIsolationAndOptOut`, `TestPostgresHiddenNestedMemoryToolkitExcludedFromVectorToken`): compiled cleanly; **skipped** in local sandbox because `ELITEA_TEST_DATABASE_URL` is unset (CI executes these against real PostgreSQL).
- **Rust (`libs/rust`, `--offline --locked --all-features`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked -p elitea-agent-runtime --all-targets --all-features -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked -p elitea-agent-runtime --all-features --lib artifact`: **57 passed, 0 failed** (incl. core & extra pinned-bucket cross-bucket refusal and unpinned root regression tests)
- **Rust (`services/elitea-worker-rust`, `--offline --locked --all-features`):**
  - `cargo fmt --all -- --check`: PASS
  - `cargo clippy --offline --locked --all-targets --all-features -- -D warnings`: PASS (`0` warnings)
  - `cargo test --offline --locked --lib --all-features -- agent_memory runtime_context_tests governed_nested_tests root_grant_tests assembly_tests a_saved_child_pipelines_artifact_node_assembles_and_office_stays_unsupported`: **118 passed, 0 failed** (incl. nested pinned `artifact`, nested `memory` toolkit, nested saved pipeline `D6=A`, ADR-0044 negative authz + `PrincipalBound` in Enforce/Report, strict monotonic `attachments` ceiling + multi-hop narrowing + ad-hoc root, target-scoped `agent_memory` + `write: off` + isolated `RecalledEntries` + root-only builder exclusion, reserved toolset shadowing refusal/aliasing, and Old Main version-skew refusal).
  - Note: 3 pre-existing Office-host loopback tests in `pipeline_artifact_tests` (`docx`/`pptx`/`xlsx`) bind localhost HTTP sockets blocked by the local sandbox; PostgreSQL Map tests in `root_grant_tests` skip cleanly when `ELITEA_TEST_DATABASE_URL` is unset.
- **Upgrade note gate:** `node scripts/ci/check-upgrading-notes.mjs`: PASS.
- **Secret scan:** `0` hits across `git diff 4f9d63873..HEAD`.
