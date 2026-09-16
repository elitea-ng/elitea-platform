# Internal MCP skill contracts

Main owns these skill operations. Rust invokes their existing internal MCP tool names.
This change adds no Rust database writer and no new MCP skill operation.

## Source ownership

The source paths below refer to the current Centry `elitea_core` plugin.
They refer to its `api/v2`, `models/pd`, and `utils` directories.

| Current source | Required behavior | Main owner | Evidence |
| --- | --- | --- | --- |
| `api/v2/skills.py::PromptLibAPI.get`; `utils/skill_utils.py::list_skills_api` | Accept IDs, tags, version author, statuses, text, limit, and offset. Filter before count and pagination. | `internal/api/v2/mcp/internal_skills_catalog.go`; `internal_skills_execute.go::list`; `internal/infra/db/repos/skills.go::List` | `TestInternalSkillListAdmitsLegacyFiltersAndBounds`; `TestInternalSkillListFiltersBeforePaginationAndHidesFolderContentPostgres` |
| `utils/skill_utils.py::list_skills_api` | Match every requested tag across the skill's versions. Ignore unknown status names. Increase the limit for explicit IDs. | `SkillsRepo.List` | PostgreSQL list filter test above |
| `api/v2/skill.py::PromptLibAPI.get` | Honor `version_id`. Reject versions belonging to another skill. Keep selected and default IDs distinct. | MCP `get`; `internalSkillDetail`; `SkillsRepo.GetVersion` | `TestInternalSkillGetSelectsVersionWithoutChangingDefault`; `TestInternalSkillVersionSelectionAndAtomicUpdatesPostgres` |
| `models/skill.py::Skill.get_default_version` | Select `meta.default_version_id`, then `base`. Accept stored numeric IDs. | `SkillsRepo.getSkillWithVersions`; `skillDefaultVersionID` | PostgreSQL version selection test above |
| `api/v2/skill.py::PromptLibAPI.put`; `utils/skill_utils.py::update_skill` | Nested `version.id` selects the target. Omission selects the default. Commit skill metadata and version content together. | MCP `update`; `SkillsRepo.updateSkill` | `TestInternalSkillUpdateMergesPartialBodyAndChecksVersionOwnership`; PostgreSQL version selection test above |
| `api/v2/skill.py::PromptLibAPI.put`; `utils/skill_utils.py::update_skill_version` | With `version_id`, accept flat version fields and return version details. Preserve skill metadata. | MCP `update`; `SkillsRepo.UpdateVersion` | `TestInternalSkillFlatVersionUpdateKeepsSkillMetadata`; PostgreSQL version selection test above |
| `utils/skill_utils.py::{_ensure_version_updatable,_ensure_version_renamable,_update_version_fields}` | Reject published and embedded edits. Preserve base names. Merge metadata and replace selected tags. | `SkillsRepo.updateSkill` | PostgreSQL version selection test; existing repository version tests |
| `api/v2/skills.py::PromptLibAPI.post`; `api/v2/skill.py::PromptLibAPI.post` | Store the authenticated user as author. Store the project separately as owner. | MCP `create`; skills HTTP handler; `SkillsRepo.Create` and `CreateVersion` | `TestInternalSkillCreateMapsCurrentNestedVersionShape`; PostgreSQL author assertions; existing owner SQL test |
| `api/v2/skill.py` folder access decorator; `list_skills_api` folder exclusion | Exclude hidden skills before count, pagination, and instruction reads. | Shared `foldervisibility`; `SkillsRepo.List`; `getSkillWithVersions` | PostgreSQL filter test verifies hidden Get and GetVersion responses |
| `api/v2/skill.py::PromptLibAPI.post` registration | Do not publish version creation as an MCP tool without its legacy opt-in. | `internalSkillToolDefinitions` | `TestInternalSkillCatalogIncludesVersionSelectorsWithoutCreateVersion` |

## Contract boundaries

The six existing skill tool names remain unchanged.
The list retains `page` and `page_size` aliases for existing Main callers.
Explicit `limit` and `offset` select the legacy pagination form.
Limits are bounded to 1000 results, 100000 skipped rows, and 100 filter IDs.
Repository failures remain errors. They no longer become empty successful lists.
A metadata-only update does not rewrite a frozen version.
A flat version update cannot change the skill name or description.

The MCP executor supplies the authenticated actor for repository calls.
HTTP creation resolves the owning user from the authenticated principal.
Author fields are not accepted from JSON bodies.

## Verification

Run from `services/elitea-main` with `ELITEA_TEST_DATABASE_URL` configured:

```sh
go test ./internal/api/v2/mcp ./internal/api/v2/skills ./internal/infra/db/repos -run 'TestInternalSkill|TestCreateSkill|TestSkill' -count=1
```

The PostgreSQL helpers create and remove isolated test databases.
The focused suite passes against the local PostgreSQL service.
These tests cover Go handlers, MCP execution, shared repositories, and real PostgreSQL effects.
They do not constitute a browser or Rust-worker deployment rehearsal.
