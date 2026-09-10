# Credential and toolkit creation from Rust chat

## Current platform sources

| Responsibility | Current source |
| --- | --- |
| Credential creation fields | `projects/centry/pylon_main/plugins/configurations/models/pd/configuration.py` |
| Saved credential selection | `projects/EliteaUI/src/[fsd]/features/credentials/ui/credentials-select/CredentialsSelect.jsx` |
| GitHub settings and selected tools | `projects/elitea-sdk/elitea_sdk/tools/github/__init__.py` |
| Toolkit persistence model | `projects/centry/pylon_main/plugins/elitea_core/models/elitea_tools.py` |

The credential picker stores `{elitea_title, private}`.
It does not store a numeric configuration ID or a JSON Schema `$ref` as the reference.
The SDK defines `selected_tools` as a list of tool names.
GitHub authentication fields belong to the referenced configuration.

## New platform sources

| Responsibility | New source |
| --- | --- |
| Internal configuration tools | `services/elitea-main/internal/api/v2/mcp/internal_configurations_catalog.go` |
| Internal toolkit tools | `services/elitea-main/internal/api/v2/mcp/internal_toolkits_catalog.go` |
| Agent-facing settings schema | `services/elitea-main/internal/api/v2/toolkits/discovery_schema.go` |
| Discovery policy and handler dispatch | `services/elitea-main/internal/api/v2/toolkits/handler.go` |
| Saved reference validation | `services/elitea-main/internal/api/v2/toolkits/settings_validation.go` |
| Native GitHub authentication | `services/elitea-worker-rust/src/toolkits/families/github/config.rs` |
| Complete result transport | `services/elitea-worker-rust/src/agents/events.rs` |

## Observed failure and correction

On 2026-09-10, chat 545 creates anonymous GitHub configuration 13 through the internal MCP tool.
Its stable title is `rust_gate3_public_github_20260910`.
The configuration uses the public GitHub API and contains no token or password.
This proves configuration creation, but does not prove secret sealing.

Toolkit creation fails after the model reads the served settings schema.
The UI catalogue preserves an earlier handwritten GitHub schema.
It describes inline `access_token` and object-shaped `selected_tools` fields.
Its credential definitions describe UI selectors rather than saved references.
The model consequently submits numeric IDs, schema pointers, and object-shaped selections.
These attempts do not satisfy Main's creation contract.

Agent discovery now uses the pinned SDK settings schema when the SDK owns the toolkit type.
It describes configuration properties as Main's accepted saved references.
It preserves metadata from the policy-filtered catalogue.
It leaves the regular REST UI schema unchanged.
No application table, migration, or credential value is changed by this correction.

## Verification

The toolkit and internal MCP handler package tests pass.
A pinned-catalogue test checks the GitHub list selection and saved-reference fields.
The test also checks that discovery leaves the REST catalogue and deployment policy metadata unchanged.
Main image `sha256:7a6a87178f50446c8fd6629046da3245025fae857331e5d733fd6d20116f0493` contains the correction.
The deployment retains the existing environment, database, and six mounts.
Chat 545 fetches the corrected schema and creates toolkit 30 on its next creation attempt.
The toolkit name is `rust-gate3-public-github-20260910`.
It stores repository `octocat/Hello-World`, `selected_tools: ["get_issues"]`, and the configuration title with `private: false`.
A database read verifies these persisted values and confirms configuration 13 contains no access token.
Toolkit Testing remains pending.

## Toolkit Testing route correction

The UI editor displays toolkit 30 and its saved credential correctly.
Its Test Tool selector offers only `get_issues`.
The first run returns `indexer service not available` before Rust execution.

`services/elitea-main/cmd/elitea-main/main.go` assigns toolkit handlers inside the `IndexStart != nil` branch.
The Rust rehearsal disables index ingestion and uses the agent command stream for standalone toolkits.
The composition root therefore leaves these handlers unset despite an available toolkit runner.
Toolkit handler assignment now occurs independently of index ingestion.
`services/elitea-main/internal/runtimecomposition/toolkit_route.go` remains the authority for the selected worker stream.
This change does not enable indexing or change database schemas.

Main image `sha256:980bb7f455b67fea40530ab3e4d9d924bdaf968cd82b7370e0b275096f9365eb` contains the route correction.
Main command, runtime composition, and toolkit handler tests pass before deployment.
Playwright reloads toolkit 30, selects `Get issues`, and presses `RUN TOOL`.
The UI displays `The tool ran and returned:` followed by actual public repository issues.
Rust logs record successful read-only `github.get_issues` execution under capability `toolkit.call_tool.v1`.
Execution `f4fe3c68e17ec2ed38c28f12b2da8719` uses the agent stream, with project 2 throughout.
The command ID is `77c4b3a29c69499cc2d7f3737d434556`.

The worker subsequently logs retryable `toolkit_delivery.control_unavailable` after publishing the result.
This completion-control failure remains under investigation.
The visible result proves execution and delivery, but does not prove final worker retirement or replay safety.

## Projection marker and GitHub UI settings

The output inbox stores the completed test result with `projected_at` unset.
Settlement requires this marker and therefore returns `ErrTerminalOutputNotReady`.
Both toolkit result repositories now call the existing `markOutputProjected` operation before committing their transaction.
The correction covers call-tool and available-tools results, including exact replay of existing rows.
Sixteen targeted repository tests pass without skips against disposable databases.
These checks cover the existing admission and settlement cases; deployed retirement verification remains required.

The current GitHub form also displays an embedding model, active branch, and base branch.
The SDK schema declares these fields, including `configuration_model: embedding` for model selection.
The handwritten Go schema omits these fields and exposes an obsolete inline access token.
`services/elitea-main/internal/api/v2/toolkits/type_catalogue.go` now selects the SDK settings for GitHub in the UI catalogue too.
The shared web fixture is regenerated through `ELITEA_WRITE_TOOLKIT_WEB_FIXTURES=1`.
The catalogue test checks all missing fields and the embedding model selector annotation.
Browser verification of defaults and persistence remains required.

Main image `sha256:99589118ae523adbd56e3fa279bd9a8a67cc9df8edd914d9990870d0dfb45474` contains both corrections.
Deployment succeeds during a claim-free interval with the strict active-claim guard unchanged.
After reload, the GitHub editor displays the embedding model and both branch controls.
The embedding selector remains blank despite the models API returning `text-embedding-ada-002` as the shared default.
The new branch controls also remain blank when saved settings omit their SDK defaults.
These default-selection and persistence gaps remain open.

The earlier execution still lacks a projection marker after deployment and retries control settlement.
The next UI run displays the earlier result; a fresh execution is not yet established by the evidence.
Do not count this replay as proof that the projection-marker correction completes a new run.
Further checks must cover fresh execution, recovery of the earlier row, and command retirement.

## Fresh test actions and configured model defaults

The former test identity hashes only the toolkit, arguments, actor, and runtime context.
Two deliberate clicks therefore reuse one execution and its previous result.
`internal/application/toolkitcalltool/service.go` now includes a caller request ID in this hash.
An absent request ID creates a new action through the existing ID generator.
`internal/api/v2/toolkitrun/response.go` accepts the bounded optional `request_id` field.
The UI API in `features/toolkits/api/toolkitTestRun.ts` creates one UUID per action.
An explicit retry can retain that UUID.
HTTP retries retain the serialized request body.
This changes admission identity, not the Rust execution contract or database schema.

The current UI reference is `projects/EliteaUI/src/components/EmbeddingModelSelect.jsx`.
It obtains the configured project embedding default and marks automatic selection separately from user edits.
The new `features/toolkits/ui/form/ToolBase/ModelSelectField.tsx` uses the existing model catalogue default.
It fills an absent selection and preserves an existing selection.
It passes `isAutoSelect` through the existing form callback.
The selector now has an accessible label.
The SDK branch defaults still require form initialization and persistence checks.

Twenty-seven targeted UI tests pass for the API and toolkit form.
The Go `toolkitcalltool` and `toolkitrun` packages pass.
The UI type check and focused lint pass.
The type check also exposes two incorrect assertions in the earlier streaming test.
Those assertions now use the existing `ToolAction` type.

Main image `sha256:c2b2aa35cf833aa686075d74ba7e7a72044cfbf96c07ecbdb51d5994901f552e` contains the action-identity repair.
UI image `sha256:d6eebfede3195e487e7e245c68113907e34fdfb2dd27f4667b053500ece27a0f` contains the matching caller and model selector.
Both deployments retain the existing environment and databases.
The strict active-claim guard remains enabled.

Playwright selects `Get issues` in toolkit 30 and presses `RUN TOOL` twice.
The first request ID is `59a60b0c-167b-4f4c-9cf0-6f4d84c47b06`.
Its execution is `be243c6773eda27af25cb99f73567b92`.
PostgreSQL confirms `SUCCEEDED`, a populated projection marker, and committed `SUCCEEDED` settlement.
The second request ID is `1236982f-0acb-414e-a03a-aa3b4b590e0d`.
It creates distinct execution `463616ca6de1a5820af031ff90bf10e1` and returns 100 public repository entries.
The browser displays `text-embedding-ada-002` and the saved pgvector configuration.
This proves fresh read execution, not authenticated credential use or the complete toolkit lifecycle.
The earlier execution `f4fe3c68e17ec2ed38c28f12b2da8719` still requires recovery.
PostgreSQL also confirms the second execution succeeds with its projection marker and committed settlement.

## Credential update, toolkit update, and saved-agent invocation

On 2026-09-10, chat 545 calls `put_configurations_configuration` for configuration 13.
Only its label changes to `Rust gate3 public GitHub updated`.
The stable title remains `rust_gate3_public_github_20260910`.
The configuration remains anonymous.

The chat calls `put_elitea_core_tool` for toolkit 30.
Its description becomes `Public GitHub read-only lifecycle verification`.
PostgreSQL confirms the repository, saved credential reference, and selected tools remain unchanged.
The chat then calls `patch_elitea_core_tool` to link toolkit 30 to agent 19, version 20.
The persisted `entity_tool_mapping` row selects only `get_issues`.
The instructions remain unchanged, and `entity_skill_mapping` retains skill 5.

Current references separate these operations too:
`projects/centry/pylon_main/plugins/configurations/api/v2/configuration.py` owns configuration updates.
`projects/centry/pylon_main/plugins/elitea_core/api/v2/tool.py` separates toolkit update from agent relation patch.
The new internal configuration and toolkit executors retain these boundaries.

Playwright submits a new request through saved-agent chat 543.
The agent invokes `get_issues` and uses returned issue 11148 in its answer.
Trace row 7279 belongs to message group 5820.
It records successful completion and stores 23,538 characters of tool output.
The answer remains visible after reload.
The tool accordion does not return after reload, despite the stored trace.

The existing trace list endpoint returns rows 7279 and 7280 for message group 5820.
The chat history loader requests only messages and conversation metadata.
It does not request `message_traces`.
The converter's comment claims trace support through message metadata, but this runtime stores normalized trace rows separately.
This is a confirmed UI history-loading gap, not a missing Rust result.
The current UI loads trace summaries separately through `buildTraceListParams` and `groupTraceStepsByGroupId`.
These functions are in `projects/EliteaUI/src/common/convertChatConversationMessages.js`.
Heavy step details remain separate and must load on demand.

## Secret lifecycle integration check

`internal_configuration_secret_lifecycle_test.go` exercises the internal configuration executor with the real PostgreSQL vault repository.
It creates a GitHub credential with a synthetic token in a disposable database.
It checks that responses and configuration rows contain no plaintext token.
It opens the encrypted vault and resolves the hidden reference to the expected token.
A label-only update preserves that reference and token.
A credential update rotates the resolved token.
No external GitHub request occurs in this test.

This new test and the existing configuration and toolkit lifecycle tests pass: three tests, no skips.
The existing toolkit test also covers relation selection replacement and unlinking.
These service checks do not prove authenticated Rust invocation through a secret-bearing toolkit.
That browser and worker gate remains open.
