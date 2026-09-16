# Internal MCP skill version projection

## Source mapping

| Current platform source | New platform owner | Required behavior |
| --- | --- | --- |
| Core `api/v2/skill.py::ProjectAPI.get` | Main `internal/api/v2/mcp/internal_skills_execute.go::internalSkillMap` | Keep each returned version attached to its own content and tags. |
| Core `models/pd/skill.py` version projections | Main `internal/api/v2/skills/handler.go::SkillVersion` and `internal/infra/db/repos/skills.go` | Preserve distinct version metadata after shared repository lookup. |
| SDK internal MCP consumption | Rust native MCP tool execution | Return the Main response without replacing version metadata. |

Paths under Core refer to `projects/centry/pylon_main/plugins/elitea_core` in the umbrella workspace.

## Implementation history

Main sync `c56d19e9` adds complete version lists to skill detail reads.
The existing internal MCP adapter replaces every version's tags with the base tags.
The correction converts each version's own tags into the existing MCP tag-object shape.
Empty and absent tag lists remain empty lists. The adapter does not modify repository values.

The focused fixture uses four versions with distinct, empty, and absent tags.
It also verifies the selected version details and unchanged source values.
Expanded repository test doubles refuse unsupported version calls instead of returning a false success.
Focused internal skill tests pass. Deployed Rust chat consumption remains a separate proof requirement.
