# Copied skill runtime acceptance, 2026-09-14

## Source mapping

The implementation mapping remains in `internal-elitea-mcp.md`, under "Copy skills when creating a version".
Current Core `api/v2/versions.py` accepts `copy_skills_from_version_id`.
Current Core `utils/skill_utils.py::copy_skill_mappings` preserves the exact skill binding.
Main `internal/api/v2/applications/handler.go::CreateVersion` serves REST and internal MCP.
Main `internal/infra/db/repos/applications.go::CreateVersion` copies bindings within the version transaction.
Main `internal/application/agentexecution/skills.go` projects the selected version's attached skills into worker input.
Rust `src/agents/instruction_authority.rs` supplies `load_skill` through the admitted instruction plan.
Rust keeps runtime instruction activation ownership. Main keeps application and skill persistence ownership.

## Deployed proof

A fresh headed Chrome session opens new internal-MCP chat 581.
Temporary application 39 has source version 45 and one attached skill.
Skill 12 has default version 17 and pinned version 18, with different receipt markers.
The agent instructions and user question contain neither receipt marker.

Trace 7451 calls `post_elitea_core_versions` with application 39 and `copy_skills_from_version_id: 45`.
It creates version 46, named `copied-runtime`.
Independent skill-list reads return identical source and target bindings: skill 12, version 18.
No separate skill attachment call creates the target binding.

Trace 7454 calls `post_elitea_core_messages` for the copied version's participant in chat 582.
The call omits `llm_settings` and completes as execution `a7f779353b39d1c0de234d8e1409d624`.
The saved agent retains its configured model.
Message group 5934 stores successful trace 7456 for `load_skill`.
Its result identifies `skill:12:version:18` and returns the pinned instructions.
The final answer contains `COPIED_PINNED_SKILL_20260914`, with no default-version marker.
The browser displays the saved answer and retains it after reload.

Both temporary application and skill deletions return HTTP 204.
The successful script exits with status zero.
Evidence: `elitea-copied-skill-browser.py`, `elitea-copied-skill-acceptance.log`, and `elitea-copied-skill-browser.png`.
Earlier attempts expose test-harness version lookup, completion-marker, and response-shape errors; they are not passing runs.

Main image: `sha256:1111a68e414a4331a4b3448af440411f58dec5dd8d7b4cb2ef7889ea4d11a9d8`.
Rust image: `sha256:5d024c10addbb14f58be732f3e48059df1abaf1ad5dc4d46fcca5588c04ce78d`.
Web image: `sha256:6f6767ebb1dacfd2a5dd409aa05648b8e244059f65d1673eed587140ab59e893`.

This closes copied-binding runtime consumption for point 3b.
The browser Save As Version control, unpinned variants, and wider TG-16 scenarios retain their separate verification boundaries.
No schema changes occur.
