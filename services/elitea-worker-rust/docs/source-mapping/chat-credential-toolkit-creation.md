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
