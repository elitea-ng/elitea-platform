# Main integration, 2026-09-23

## Scope and source baseline

Feature parent: `dc7190cfdc6d7e858c0f64ee4b4415f9c0a712bd`.
Fetched main: `6537ca309ddc358770b4d0c1cdaeb3337ab25adb`.
The merge imports 21 commits and changes 1,213 incoming paths.
The incoming main history includes waves #945, #959, #962, #984, #987, #990, and PgBouncer #997.
Current-platform code remains a behavior reference. This merge adds no legacy runtime dependency.
The isolated branch is `feat/rust-main-integration-20260923`.
The primary worktree and its deferred Gate 5 edits remain unchanged.

## Contract reconciliation

| Source owners | Merge decision | Verification boundary |
| --- | --- | --- |
| `src/agents/{ordinary,application_tools,pipeline,session}.rs` | Keep claim-fenced independent model scopes, instruction authority, compaction, and four-call automatic continuation. Add main pipeline children and builder/artifact authority. | Rust library and pipeline-child component tests. |
| `src/toolkits/{tool_binding,delegated_auth,mcp}.rs`; `src/agents/{ordinary,sensitive_tools,tool_namespacing}.rs` | Use canonical toolkit bindings as the only provider-name authority. Derive main user notices from exact bindings. Preserve undiscovered authorization catalogs. | Collision dispatch, scoped guards, MCP discovery, and authorization tests. |
| `src/agents/{direct_hitl,application_pipeline}.rs` | Retain main multi-call decisions and child graph pauses. Preserve authorization declines, proxy results, replay normalization, and independent checkpoints. | Direct HITL, application pipeline, and restored-session component tests. |
| `src/agents/{runtime,graph/node_events}.rs`; `src/execution/native_agent_lifecycle.rs` | Keep exact error codes, safe diagnostics, and generalized model-failure propagation. Add main lazy MCP authorization notices. | Runtime taxonomy and graph failure tests. |
| `src/agents/events.rs`; Main `internal/infra/db/repos/agent_trace.go` | Use main frame-aware success chunks. Retain bounded structured-error chunks and both persistence readers. | Complete UTF-8 output, digest, reassembly, and event tests. |
| `src/transport/{anthropic_facade,openai_compatible_facade}.rs` | Move incoming image support into the existing feature facades. Retain typed failures and model budgets. | Both provider image tests and transport tests. |
| Main `internal/application/agentexecution/{instruction_snapshots,projectcontext,adhoc,start}.go` | Preserve typed revisioned project context. Use main text injection only when no typed snapshot exists. | Application/ad-hoc bundle and project-context tests. |
| Main `internal/api/v2/eliteacore/{project_context,search_options}.go` | Keep existing dedicated handlers, activation rules, and actor-scoped metadata. Do not restore older inline handler copies. | API and generated-contract tests. |
| Main router, repositories, runtime composition | Combine notifications, image reads, edit-regeneration, builder/artifact routes, skill metadata, and existing Rust toolkit routes. | Full Main Go suite and vet. |
| Web chat and toolkit owners | Preserve both chunk protocols, multiple MCP cards, real auth modal, delegated credentials, shared auto-selection, voice, TTS, and mentions. | Typecheck, targeted Vitest, endpoint checks, and production build. |
| Web comparison controls | Compare `budget_mode`, not the retired `max_context_tokens` setting. | Comparison-control regression tests. |
| `Cargo.toml`, `Cargo.lock`, deployment templates | Keep incoming pinned dependencies, Rust RMCP auth, 8 MiB requests, and diagnostic release line tables. Combine SSE and toolkit-discovery settings. | Cargo checks, Helm lint, and chart rendering. |

The original provider-alias implementation from main is superseded by the canonical binding owner.
Its user notice remains. Regression coverage now exercises that notice and exact canonical dispatch together.
The retired Anthropic gateway test file stays deleted. Its new image cases run against the current Anthropic facade.
Generated Go SQL and API bindings come from the merged query and OpenAPI sources.
No generated binding conflict is resolved by hand.

## Migration collision and deployment hold

Main owns shared migration 125 for token lifecycle.
Feature migrations 125 through 127 move to the free versions 126 through 128.
Their SQL bytes remain identical. Existing feature migrations 129 through 132 retain their paths.
The merged heads are shared 132, tenant 138, and agentstate 3.
No database or migration ledger changes during this merge.

| Feature filename before merge | Merged filename | Unchanged SQL SHA-256 |
| --- | --- | --- |
| `0125_toolkit_execute_read.sql` | `0126_toolkit_execute_read.sql` | `c2497df59b29fec9230c0164e2e1e44d9c1f5d0d0bf08d7ae1dfd0aea4535e71` |
| `0126_mcp_prebuilt_parameter_schema.sql` | `0127_mcp_prebuilt_parameter_schema.sql` | `e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289` |
| `0127_mcp_oauth_clients.sql` | `0128_mcp_oauth_clients.sql` | `62bf7e4c22d2bba3375b216fd578aa1fdb57b1243dada093126d2076b8445515` |

`internal/infra/db/migrate/manifest.go` hashes the complete SQL bytes and rejects duplicate versions.
`ledger.go` keys applied rows by target kind, target ID, and version.
It validates the migration name and checksum before allowing further migration work.

A main-origin ledger through shared 125 matches the merged manifest prefix.
Its source upgrade path then includes feature 126 through 132.
A feature-rehearsal ledger with toolkit execution at 125 fails the merged name check.
It requires an explicit, reviewed ledger reconciliation before upgrade.
Existing earlier rehearsal-ledger differences can require further reconciliation.
The unit fixture verifies prefix acceptance and rejection of the unreconciled feature version.
It does not prove a database upgrade or authorize ledger edits.
**Deployment remains on hold until the actual target ledger and schema are reconciled and verified.**

## Verification

- Rust full test targets: 1,294 tests pass, including 1,206 library tests, with no ignored tests.
- Strict Rust Clippy passes for all targets.
- Main: 167 tested packages pass. The run records 10,786 test/subtest passes and 1,672 skips.
- Twenty-one Main packages contain no tests. Skipped PostgreSQL and external-service checks are not deployment evidence.
- Main `go vet ./...` passes.
- Python agent-event tests: 28 pass with repository-generated protobuf modules on `PYTHONPATH`.
- Helm lint passes. Chart rendering passes with an explicit test origin and egress posture.
- Web typecheck, clean API generation, endpoint manifest tests, and production build pass.
- Web targeted tests: 1,079 pass with three expected failures, as reported by the web merge owner.

The broader Rust run also checks the authoritative 8 MiB input boundary with regenerated signed fixtures.
The first Rust run found a dropped undiscovered-MCP authorization catalog from an automatic merge.
The canonical materializer now preserves the catalog; its regression passes.
The full Main run found missing router fixture authorization and one omitted route expectation.
Both fixtures now match the merged contract, and the full suite passes.

Three feature pipeline image baselines remain because the merged visual scenario still asserts the MCP control.
Their full browser snapshot acceptance remains unverified.
A fresh headed browser reads existing chat 658 through the merged dev UI.
The partial answer and actionable context error remain after reload; the context dialog opens, with zero page errors.
This read-only smoke uses the unchanged rehearsal backend and does not prove merged-stack execution.
No merged backend is deployed. No database migration, container restart, push, or pull request occurs here.
The prior live model-error proof belongs to the feature parent, not this merged build.
Diagnostics retain bounded active span names and native capture at the runner boundary.
They do not guarantee complete suspended-future ancestry or the originating error stack.
Live context streaming remains unverified on the merged stack.

## Deferred primary work

A read-only patch check uses the primary worktree's tracked dirty diff.
Only `internal/db/queries/agent_chat.sql` and generated `internal/db/sqlcgen/agent_chat.sql.go` fail to apply.
Reconcile the deferred SQL change semantically after integration, then regenerate sqlc bindings.
Main already admits `skills_builder` and `project_context_builder`; the deferred patch also admits the singular `skill_builder` compatibility key.
The deferred continuation query adds agent type, response metadata, and exact application-version joins.
The remaining tracked deferred edits apply cleanly in that check.
Untracked deferred files remain solely in the primary worktree.
This check does not stage, copy, discard, or modify those files.

## Original conflict inventory

- `apps/elitea-web/e2e/snapshots/visual/pipeline-editor.visual.spec.ts-snapshots/pipeline-editor-empty-light-visual-linux.png`
- `apps/elitea-web/e2e/snapshots/visual/pipeline-editor.visual.spec.ts-snapshots/pipeline-editor-empty-visual-linux.png`
- `apps/elitea-web/e2e/snapshots/visual/pipeline-editor.visual.spec.ts-snapshots/pipeline-editor-interrupts-disabled-visual-linux.png`
- `apps/elitea-web/scripts/check-endpoint-manifest.test.mjs`
- `apps/elitea-web/src/entities/message/lib/normalise.ts`
- `apps/elitea-web/src/features/agents/ui/AgentVersionControls.test.tsx`
- `apps/elitea-web/src/features/chat-messages/lib/chatStreamFrame.ts`
- `apps/elitea-web/src/features/chat-messages/lib/chatStreamReducer.test.ts`
- `apps/elitea-web/src/features/chat-messages/lib/chatStreamToolFrames.ts`
- `apps/elitea-web/src/features/chat-messages/lib/convertMessagesToChatHistory.ts`
- `apps/elitea-web/src/features/chat-messages/ui/chat-box/ApplicationAnswer.tsx`
- `apps/elitea-web/src/features/chat-messages/ui/chat-box/ChatMessageList.types.ts`
- `apps/elitea-web/src/features/settings/ui/ai-personality/settingsProfileForm.test.ts`
- `apps/elitea-web/src/features/settings/ui/ai-personality/settingsProfileForm.ts`
- `apps/elitea-web/src/features/toolkits/api/toolkits.test.ts`
- `apps/elitea-web/src/features/toolkits/api/toolkits.ts`
- `apps/elitea-web/src/features/toolkits/ui/ConfigurationTab.tsx`
- `apps/elitea-web/src/pages/agents/ui/EditApplicationConfigurationPanel.tsx`
- `apps/elitea-web/src/pages/toolkits/__tests__/testRouter.tsx`
- `apps/elitea-web/src/pages/toolkits/lib/credentialPicker.tsx`
- `apps/elitea-web/src/shared/api/generated/model/applicationVersionDetail.zod.ts`
- `apps/elitea-web/src/shared/api/generated/model/projectContext.zod.ts`
- `apps/elitea-web/src/shared/api/generated/model/projectContextUpdateRequest.zod.ts`
- `apps/elitea-web/src/shared/api/generated/model/searchOptions.zod.ts`
- `apps/elitea-web/src/shared/api/generated/model/secretListItem.zod.ts`
- `apps/elitea-web/src/shared/api/generated/model/toolkitCreateRequest.zod.ts`
- `apps/elitea-web/src/shared/api/generated/toolkits/toolkits.msw.ts`
- `apps/elitea-web/src/shared/api/generated/toolkits/toolkits.ts`
- `apps/elitea-web/src/shared/i18n/en.json`
- `apps/elitea-web/src/widgets/chat-box/ui/ChatBox.tsx`
- `apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxSend.helpers.ts`
- `deploy/helm/elitea/templates/main/_helpers.tpl`
- `services/elitea-main/internal/api/generated/api.gen.go`
- `services/elitea-main/internal/api/router.go`
- `services/elitea-main/internal/api/v2/agentexecution/route.go`
- `services/elitea-main/internal/api/v2/applications/handler.go`
- `services/elitea-main/internal/api/v2/eliteacore/handler.go`
- `services/elitea-main/internal/application/agentexecution/continue.go`
- `services/elitea-main/internal/application/agentexecution/regenerate.go`
- `services/elitea-main/internal/application/agentexecution/start.go`
- `services/elitea-main/internal/db/queries/agent_chat.sql`
- `services/elitea-main/internal/db/sqlcgen/agent_chat.sql.go`
- `services/elitea-main/internal/db/sqlcgen/configurations.sql.go`
- `services/elitea-main/internal/domain/execution/model.go`
- `services/elitea-main/internal/infra/db/migrate/manifest_test.go`
- `services/elitea-main/internal/infra/db/repos/agent_trace.go`
- `services/elitea-main/internal/infra/db/repos/applications.go`
- `services/elitea-main/internal/infra/db/repos/conversations.go`
- `services/elitea-main/internal/infra/storage/content_server.go`
- `services/elitea-main/internal/runtimecomposition/composition.go`
- `services/elitea-main/internal/runtimecomposition/current_rust_worker_toolkit_capability_snapshot.json`
- `services/elitea-main/internal/runtimecomposition/worker_toolkit_capability_test.go`
- `services/elitea-worker-python/tests/unit/test_agent_events.py`
- `services/elitea-worker-rust/Cargo.lock`
- `services/elitea-worker-rust/Cargo.toml`
- `services/elitea-worker-rust/SOURCE_PARITY.md`
- `services/elitea-worker-rust/src/agents/application_tools.rs`
- `services/elitea-worker-rust/src/agents/assembly_tests.rs`
- `services/elitea-worker-rust/src/agents/direct_hitl.rs`
- `services/elitea-worker-rust/src/agents/events.rs`
- `services/elitea-worker-rust/src/agents/events_tests.rs`
- `services/elitea-worker-rust/src/agents/graph/resume.rs`
- `services/elitea-worker-rust/src/agents/ordinary.rs`
- `services/elitea-worker-rust/src/agents/ordinary_tests.rs`
- `services/elitea-worker-rust/src/agents/pipeline.rs`
- `services/elitea-worker-rust/src/agents/runtime.rs`
- `services/elitea-worker-rust/src/agents/sensitive_tools.rs`
- `services/elitea-worker-rust/src/agents/session.rs`
- `services/elitea-worker-rust/src/agents/session_tests.rs`
- `services/elitea-worker-rust/src/bootstrap.rs`
- `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs`
- `services/elitea-worker-rust/src/toolkits/delegated_auth.rs`
- `services/elitea-worker-rust/src/toolkits/materialize.rs`
- `services/elitea-worker-rust/src/transport/anthropic_facade.rs`
- `services/elitea-worker-rust/src/transport/anthropic_gateway_tests.rs`
- `services/elitea-worker-rust/src/transport/openai_compatible_facade_tests.rs`


## Primary worktree handoff

Merge `821d7280810baa57c9452ae1ac8199f5ed68d88a` was fast-forwarded onto
`feat/rust-worker-continuation` in the original consolidated worktree.
All 11 pre-existing modified or untracked files were copied with SHA-256 hashes
and a binary tracked patch before handoff. Git's automatic preservation stash
was retained after restoring the tracked edits.

Only the predicted SQL query and generated binding paths conflicted during
restoration. The nine builder allowlists retain both `skill_builder` and
`skills_builder`, plus `project_context_builder`. The deferred continuation
fields and exact application-version joins remain; bindings were regenerated
with sqlc. Deferred edits and the local recovery configuration remain uncommitted.
No database schema or migration ledger was changed.

The restored-work check exposed a catalogue admission mismatch: SQL retained the
legacy singular builder key while main's Go catalogue guard knew only the plural
key. The local restored patch now admits both, with a catalogue regression case.
The affected `agentexecution`, repository, and `eliteacore` Go packages pass.
This compatibility reconciliation remains with the deferred working-tree patch;
it is not part of the clean merge's committed runtime changes.


## Isolated database reconciliation proof

The actual rehearsal product ledger was read after integration. Of 138 recorded
shared/tenant receipts, only shared versions 125–127 differ from the merged
manifest. Each is byte-identical to its new version 126–128. Tenant projects 1
and 2 are at version 137. Worker-state versions 1–3 already match all merged
checksums and require no migration.

A fresh custom-format backup was restored into an isolated database. The
unchanged merged migrator first refused the copy with the expected name mismatch
at `shared/0125_token_lifecycle.sql`. A single transaction then locked the ledger,
verified all three original names and SHA-256 hashes, checked destination slots,
and moved only their version numbers to 126–128 through unused temporary numbers.
Names, checksums and application timestamps were not rewritten. No product or
worker rows were removed or recreated.

The unchanged migrator then applied shared 125 (token lifecycle) and tenant 138
(trigger authentication mode) for both projects. A second run succeeded without
further changes. These migrations come from merged main; this reconciliation
introduces no additional product schema.

Exact row fingerprints before/after match across twelve chat, application,
application-version and skill tables. Private-project counts include 631 chats,
2,473 message items, 2,299 text records, 48 applications, 56 versions and two skills.
The public project retains five chats and twelve message/text records.

Local evidence: `elitea-rehearsal-ledger-20260923.csv`,
`elitea-rehearsal-ledger-diff-20260923.json`,
`elitea-reconcile-main-ledger-20260923.sql`,
`elitea-clone-migrations-20260923.log`, and
`elitea-migration-copy-data-check.json`, under `/private/tmp`.
The recoverable backup is `elitea-before-main-ledger-20260923.dump`.
At this verification boundary the active rehearsal ledger is unchanged;
coordinated replacement with matching images remains required.
