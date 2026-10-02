# Internal MCP draft default model, 2026-09-11

Live chat calls expose a missing default-model contract in Main's shared completion client.
Draft calls with explicit models succeed. Calls without `llm_settings` send an empty model and fail at the gateway.

`services/elitea-main/internal/api/v2/predict/default_model.go` now resolves omitted models through `CurrentModelCatalogService`.
This reuses project defaults, public sharing, and model grants from the existing configuration service.
The selected default must exist in the returned catalog.
Explicit model names remain unchanged. The requesting project and user remain the gateway authorization and billing identity.
The composition root wraps the shared completer used by REST and internal MCP drafting.
No database schema changes are required.

Focused Go tests cover local defaults, shared defaults, missing defaults, unavailable models, dependency failure, and explicit overrides.
The predict, drafts, and Main command suites pass with local test listeners enabled.
Deployed Playwright verification succeeds in chat 545 after Main replacement.
Trace 7329 calls `post_prompt_lib_generate_skill_draft` with skill 5 and version 5.
Trace 7330 calls `post_prompt_lib_generate_project_context_draft` with caller-supplied background.
Neither call supplies `llm_settings`. Both traces record `is_error=false` and actual generated draft content.
Skill version 5 retains its original persisted instructions after the edit draft.
These results prove default-model drafting, not the remaining permission and recovery gates.

Main image: `sha256:5fb19cee2030a4a5cc12a9ddf6b0c44bfbfe5456d62a24b059fcd05f195b7ae1`.

## Current source mapping

Current Core `api/v2/generate_skill_draft.py` and `api/v2/generate_project_context_draft.py` resolve omitted models with `configurations_get_default_model`.
Their source owns the business requirement for optional model settings.
Main `internal/application/configurations/model_service.go` owns the replacement catalog and default policy.
Main `cmd/elitea-main/main.go` composes the default resolver around the shared completion client.
Rust continues to invoke these Main-owned operations through its existing internal MCP adapter.
No Rust-specific model resolver or Python RPC bridge is introduced.

## Failure and permission checks

`TestInternalDraft*` passes through the in-process MCP endpoint.
It covers permission denial, conflicting projects, invalid inputs, unusable output, safe gateway errors, and token-scoped edit refusal.
`TestInternalSkillDraftReadsSelectedVersionWithoutSaving` passes against a disposable migrated PostgreSQL database, with no skipped tests.
It covers selected-version reads, unchanged persistence, foreign versions, missing skills, and folder-hidden skills.
This database test uses a fake model and does not prove deployed authentication.

Deployed chat 545 also requests missing skill 2147483647 with version 5.
Trace 7333 records one draft call with `is_error=true` and no successful draft output.
Live restricted-user permission verification passes on 2026-09-14, as recorded in `point3-audit-20260913.md`.

## Selected-version browser acceptance, 2026-09-14

A fresh headed Chrome session opens internal-MCP chat 545.
Two temporary skills provide distinct base, selected, and foreign version markers.
The first call selects skill 6, version 7, and omits `llm_settings`.
Message group 5920 stores trace 7432 with `is_error=false`.
The returned instructions retain `SELECTED_DRAFT_SOURCE_20260914` and exclude both other markers.

The second call deliberately selects skill 6 with version 8, which belongs to another skill.
Trace 7433 stores those exact arguments with `is_error=true` and no successful draft output.
The browser displays the failed tool row. This is the required validation refusal, not a failed acceptance run.
The generic tool error does not itself identify the validation cause.
The existing PostgreSQL test separately proves rejection before model invocation.

Full skill reads remain identical before and after both calls.
Both temporary skill deletions return HTTP 204.
The script exits successfully, and the screenshot shows the successful draft followed by the deliberate refusal.
Evidence: `elitea-selected-draft-browser.py`, `elitea-selected-draft-browser.log`, and `elitea-selected-draft-browser.png`.
Main image: `sha256:1111a68e414a4331a4b3448af440411f58dec5dd8d7b4cb2ef7889ea4d11a9d8`.
Rust image: `sha256:5d024c10addbb14f58be732f3e48059df1abaf1ad5dc4d46fcca5588c04ce78d`.
Web image: `sha256:6f6767ebb1dacfd2a5dd409aa05648b8e244059f65d1673eed587140ab59e893`.
No schema changes occur.
