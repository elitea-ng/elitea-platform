# Internal MCP drafting

## Source mapping

Core paths below start at `projects/centry/pylon_main/plugins/elitea_core` in the umbrella workspace.
Main paths start at `services/elitea-main` in this repository.

| Current platform source | Main owner | Required behavior |
| --- | --- | --- |
| Core `api/v2/generate_skill_draft.py::PromptLibAPI.post` | `internal/api/v2/drafts/drafts.go::GenerateSkillDraft` | Generate a validated skill draft. Require the skill-create permission. Do not save the result. |
| Core `utils/generate_skill_utils.py::fetch_skill_for_edit` | `internal/infra/db/repos/skills.go::GetVersion` | Select the requested version within the endpoint project and requested skill. Refuse a missing or foreign version. |
| Core `utils/generate_skill_utils.py::build_edit_skill_system_prompt` | `internal/api/v2/drafts/drafts.go::GenerateSkillDraft` | Include stored name, description, selected instructions, and tags. Ask for a complete revised draft. |
| Core `models/pd/generate_skill_draft.py` | `internal/api/v2/drafts/drafts.go` | Require both edit IDs together. Normalize the name. Bound output fields. Refuse unusable output. |
| Core `api/v2/generate_project_context_draft.py::PromptLibAPI.post` | `internal/api/v2/drafts/drafts.go::GenerateProjectContextDraft` | Generate or revise caller-supplied project background. Return `project_background`. Do not save the result. |
| Core service-prompt utilities and draft output contracts | `internal/api/v2/drafts/prompts.go` | Keep reviewed source prompts and validated output contracts. Use the shared Main completion service. |
| Core draft endpoint `mcp_tool` metadata | `internal/api/v2/mcp/internal_drafts.go` and `catalog.go` | Publish skill and project-context drafts. Keep application drafting unpublished. |
| UI `src/[fsd]/features/skill/ui/ai-edit-skill-modal/AIEditSkillModal.jsx` | Shared REST and MCP draft handler | Preserve `skill_id` and selected `version_id` edit intent. Return content for review before a separate save. |
| UI `src/[fsd]/features/settings/ui/project-context/ai-edit/AIEditProjectContextModal.jsx` | Shared project-context draft handler | Accept `current_project_background` as edit input. |
| SDK `elitea_sdk/runtime/utils/mcp_adapter.py::call_tool` | Main MCP `server.go::callInternalTool` | Return standard MCP text content. Mark handled failures with `isError`. Do not disclose infrastructure errors. |

## Published contract

The skills category adds `post_prompt_lib_generate_skill_draft`.
The project-context category adds `post_prompt_lib_generate_project_context_draft`.
Both require `user_description` and accept the shared optional `llm_settings` block.
The endpoint project remains authoritative. A conflicting `project_id` fails before model execution.
Main derives the actor from authentication. Caller-supplied actor and model-project fields do not select credentials.

Skill drafting uses `models.applications.skills.create`, as the current platform does.
Stored edits additionally require `models.applications.skills.details` before the repository reads protected instructions.
The shared skill repository excludes versions inside the caller's no-access folders.
Project-context drafting uses Main's existing REST permission, `models.project_context.edit`.
The current platform names that permission `models.project_context.generate`.
Main deliberately uses the seeded edit grant for the same admin and editor roles.
No new role grant or permission alias is introduced.

Skill edit IDs must be positive PostgreSQL integer keys. Numeric and string IDs are accepted.
Main reads the requested version through its existing scoped repository method.
A missing skill or mismatched version returns a safe not-found result before completion.
The draft operation does not call any create, update, or version-write method.

The current platform remains the business reference. Its service locator and Python prediction transport are not target architecture.
Main keeps source-owned prompts and uses its existing completion gateway.
Main retains its REST error policy: invalid input returns 400 and unusable model output returns 422.
Missing dependencies return 503. Repository and gateway failures return safe errors.
MCP converts unsuccessful handler outcomes into `isError` tool results.
The existing MCP boundary redacts all handler failures with status 500 or higher.

## Implementation history

The implementation starts from merged Main revision `c56d19e9`.
Main already owns draft prompts, completion, output validation, and REST routes at this revision.
It refuses stored skill edits and does not publish draft tools through internal MCP.

This change adds the scoped skill-version reader to the existing draft handler.
It composes one shared handler for REST and internal MCP.
The MCP adapter publishes two draft operations through the existing category and permission machinery.
Application drafting remains available through its existing REST route only.
Application edit-by-ID remains outside this change.

## Verification

The draft package tests cover source contracts, create output, edit input, identifier validation, and safe read failures.
The MCP tests exercise JSON-RPC calls through the in-process endpoint and real draft handler.
They check permissions, endpoint project, actor identity, model overrides, prior context, unusable output, and gateway failures.
Catalog tests check the exact published names and permissions.
These component tests use a fake completion service.

`TestInternalSkillDraftReadsSelectedVersionWithoutSaving` uses migrated PostgreSQL tables and the in-process MCP endpoint.
It verifies selected-version instructions, unchanged stored content, missing skills, foreign versions, and hidden skills.
It requires `ELITEA_TEST_DATABASE_URL`. A skipped run does not provide database evidence.
Its completion service remains a fake.

Run the focused suites from the repository root:

```sh
go test ./services/elitea-main/internal/api/v2/drafts ./services/elitea-main/internal/api/v2/mcp -count=1
go test ./services/elitea-main/internal/api -run 'TestEveryGatedPermissionHasAGrant|TestEliteaCoreGatedPermissionsAreSeeded' -count=1
```

Deployed model generation and Rust chat-driven MCP consumption require separate live verification.
This change does not claim either result from component or database fixtures.
