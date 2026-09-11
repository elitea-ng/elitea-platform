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
