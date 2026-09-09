# Chat-driven internal MCP acceptance

## Required behavior

Gate 3 requires internal tools to create and update entities from chat.
The user selects Elitea MCP Tools in a new or existing conversation.
Main resolves the selected tools under the authenticated actor and project.
Rust invokes the tools and returns their actual results.
The editor or entity API then confirms the persisted entity and its updates.

A generated recipe, code sample, or claimed success does not prove persistence.
Direct endpoint tests and standalone Toolkit Test checks support this gate but do not replace it.

## Source mapping

Current Core paths start at `projects/centry/pylon_main/plugins/elitea_core`.
New platform paths start at the repository root.

| Current behavior | New platform owner | Verification boundary |
| --- | --- | --- |
| Core `utils/internal_tools.py::inject_mcp_toolkits` resolves enabled builder groups. | Main `internal/application/agentexecution` and `internal/runtimecomposition/agent_prebuilt_mcp.go` | Conversation and saved-version selection must produce frozen toolkit descriptors. |
| Core `utils/internal_tools.py::dedupe_internal_mcp_tools` avoids duplicate endpoints. | Main runtime tool projection | Manual and automatic category selections must not duplicate tool names. |
| Core `utils/internal_tools.py::resolve_internal_mcp_settings` supplies invocation authority. | Main trusted prebuilt materializer | Actor credentials are materialized after claim authorization, outside durable input. |
| Existing chat selection persists `meta.internal_tools`. | UI `apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxInternalTools.ts` | Send must wait for selection persistence. Reload must retain the selection. |
| New chat creation carries selected modules. | UI chat creation and send composition | Selection must survive creation of the first conversation. |
| Core builder endpoints execute entity operations. | Main `internal/api/v2/mcp/internal_*` adapters and shared REST services | Creation and updates must retain permissions, project scope, validation, and saved results. |
| SDK remote MCP tools execute selected operations. | Rust `src/toolkits/mcp.rs` and agent toolset assembly | Model-visible tools must match the selected categories and actual server schemas. |

## Gap found during integration

The previous running images do not include the current integration changes.
The source before this correction drops `internal_mcp` without adding automatic builder descriptors.
The current correction freezes the selected categories before it removes these control flags.
Manual prebuilt MCP configuration uses a separate path.
Thus, an enabled UI toggle alone does not establish that the model receives internal tools.

The current Core general group excludes the dedicated skill and project-context builder groups.
The requested new behavior includes those supported builders under the general chat toggle.
Their operations must remain scoped to the active project and authenticated actor.

## Required deployed evidence

For each supported entity family, retain these observations:

1. Select the internal tools and submit a creation request through chat.
2. Confirm the admitted input contains the selected builder descriptors without plaintext credentials.
3. Correlate the chat execution with the actual internal tool call and terminal result.
4. Open the resulting entity and verify the saved identifier and requested content.
5. Request an update through chat and verify the changed persisted state or revision.
6. Reload the conversation and verify selection, history, and result continuity.

Cover agents, pipelines, skills, project context, and the other published builder categories.
Exercise both a new conversation and an existing conversation.
Verify disabled selection, permission refusal, and foreign-project refusal separately.
Keep this gate open until the deployed evidence exists.

## Source correction and focused checks

Main creates allowlisted internal builder descriptors without saved toolkit IDs.
The general toggle includes skills and project context.
The nested application route materializes authorized runtime settings after claim authorization.
The new chat flow carries module selection into conversation creation.
Existing chat sends wait for selection persistence.
Rust accepts missing toolkit IDs only for the exact internal builder identity and metadata contract.
Saved external MCP and native toolkit references still require positive IDs.

The focused UI checks pass: two test files, ten tests.
The Rust snapshot checks pass: eight tests, including both chat input forms and malformed builder rejection.
These checks do not prove deployed tool invocation or entity persistence.

## Deployment findings on 2026-09-09

Main, Rust, and UI images now contain the integration changes.
The existing chat listing fails against the restored application schema before the repair.
Its query uses tenant `social_pins` and `entity_name` instead of shared `centry.social_pins` and `entity`.
The repair keeps project and actor predicates and requires no schema change.
Playwright confirms HTTP 200 and displays the existing conversation after deployment.

New chat creation also encounters mixed JSON types in an existing encrypted vault.
Exact secret lookup now decodes only the requested leaf and preserves the encrypted stored bytes.
An unrelated numeric field cannot block a missing context-manager default.
The secrets, folders, and conversation PostgreSQL tests pass against isolated test databases.

The first chat-driven internal MCP attempt fails during input materialization.
Main supplies its loopback HTTP origin to a worker in another container.
The standalone deployment now sets the existing origin option to its HTTPS platform edge.
TLS verification remains enabled.
The next attempt passes materialization and exposes the separate persona rejection described in `chat-personas.md`.
No saved skill or successful internal tool invocation is proven by these attempts.

After the persona repair, the worker reaches internal MCP connection setup.
The rehearsal MCP trust bundle lacks the runtime CA used by the platform edge.
The repaired bundle retains public roots and the mock CA, then adds the existing runtime CA.
The deployment initializes this bundle after runtime bootstrap.
Compose configuration validation passes.

The next deployed execution materializes nine internal toolsets and exposes 51 model tools.
Execution `582a31803284a498451502753683bdfa` then receives `model_gateway.rejected` on its first model request.
This proves tool discovery, but not tool invocation or successful chat completion.
The internal MCP acceptance gate remains open.

The gateway still uses the previous product database copy after the three-service deployment.
The gateway now uses the same active copy as Main, with its existing image and settings retained.
The earlier rejection record reports HTTP 400 and `client_error`.
The database mismatch is not proven to cause that model rejection.

## Model schema correction

The gateway returns a provider validation error for discovery schema property names containing square brackets.
The three names are `entities[]`, `statuses[]`, and `tags[]`.
These names belong to REST query encoding, not model-callable JSON arguments.
Main now exposes `entities`, `statuses`, and `tags` arrays through internal MCP.
The discovery adapter restores bracketed REST names before calling the existing entity discovery service.
Rust keeps the tool arguments and schema unchanged across the MCP boundary.

Current Core discovery endpoints and the shared Main discovery service remain the behavior reference.
New mapping owners are `internal/api/v2/mcp/internal_entity_discovery.go` and Rust `src/toolkits/mcp.rs`.
The catalog test checks property names across all internal categories.
The PostgreSQL parity test checks visible results and response envelopes against the existing service.
The query test verifies array values retain the REST names.

A controlled gateway request with all 51 original schemas returns HTTP 400.
The same request returns HTTP 200 after changing only these three schema names.
The Main package regression checks pass.
This controlled request does not replace the pending chat-driven persistence test.

## Automatic project scope and skill creation

The deployed greeting succeeds with the cynical persona and all internal tools enabled.
The next chat request invokes skill listing and creation.
Creation fails because the existing `skill_versions.uuid` column has no database default and requires a value.
Current Core `models/skill.py::SkillVersion` supplies `uuid.uuid4` through its ORM.
Main now supplies `gen_random_uuid()` explicitly in base, named-version, and publication inserts.
No schema migration is added.
The PostgreSQL test removes the fixture default and verifies distinct, non-null UUIDs for base and named versions.

The model also asks for a project ID that Main already knows.
Main's existing `callInternalTool` derives that ID from the authenticated endpoint and rejects conflicting supplied IDs.
The listing now removes `project_id` from internal model schemas without mutating the source catalog.
Other project selectors, such as explicit target-project operations, retain their separate contracts.
The catalog, authority, creation, versioning, and publication tests pass.
The new image must still pass chat-driven saved-entity verification.

The first UUID repair exposes another ORM default: `skill_versions.meta` is non-null without a database default.
The repository now writes an empty metadata object for base and named versions.
The fixture removes both defaults and enforces both non-null constraints.
All skill repository tests pass with this fixture.
Inspection confirms the remaining required columns receive explicit values or existing database defaults.
The application schema remains unchanged.

## Verified skill creation and update

On 2026-09-09, chat 540 creates `rust-gate3-joke-20260909` through `post_elitea_core_skills`.
The saved skill ID is 5 and its base version ID is 5 in project 2.
The database confirms the requested initial instructions, UUID, and metadata.
A subsequent chat request updates the instructions through `put_elitea_core_skill`.
A separate database read confirms the update retains both identifiers.
Playwright opens `/app/skills/all/5` and confirms the saved instructions in the editor.

The final instructions are:

> Write exactly one short family-friendly computer joke, then explain the wordplay in one sentence.

The model-facing schemas omit endpoint project IDs; Main supplies the authenticated scope automatically.
The existing chat history contains earlier supplied project information.
Therefore, a fresh chat without that information remains required to prove the complete automatic-project user flow.
Other entity families and permission-negative cases remain open.
