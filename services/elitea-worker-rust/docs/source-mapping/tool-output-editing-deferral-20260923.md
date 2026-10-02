# Optional tool-output clearing: deferred

## Decision — 2026-09-23

The user defers optional clearing of older tool results from model context.
This feature does not block Gate 4 acceptance.
No runtime implementation changes accompany this decision.

This feature is separate from model-assisted error explanation.
It is also separate from replacement messages after sensitive-tool rejection.
Authorization decisions and rejection outcomes remain outside this deferral.
Existing structured compaction remains in scope.

## Current-platform source mapping

SDK revision: `966526e8334354366dd161b606d73fe8e204b850`.

`elitea_sdk/runtime/clients/client.py::_inject_context_editing` installs `ContextEditingMiddleware` with `ClearToolUsesEdit`.
The middleware clears older tool-result payloads from model context when enabled.
The SDK requires a conversation and the master context-management setting.
The feature is independent of summarization and defaults to disabled.

## New-platform source mapping

- `src/agents/context_management.rs::admit_current` rejects enabled `enable_context_editing` as unsupported.
- `src/agents/context_management_tests.rs` covers this capability boundary.
- `apps/elitea-web/src/features/settings/ui/memory/MemoryContextManagement.tsx` already exposes the setting.
- `services/elitea-main/internal/domain/contextsettings` carries the setting.

UI and Main paths are relative to the repository root.
Rust paths are relative to `services/elitea-worker-rust`.
The exposed setting does not prove Rust support.
Enabled requests remain rejected; disabled requests retain current behavior.

## Deferred acceptance

Before enabling this feature, preserve call identities, outcomes, pending approvals, and authoritative instruction references.
Keep original execution history intact and persist the prepared model request before dispatch.
Verify recovery and independent nested-model histories.
Align the UI capability indication with runtime support before general release.

This documentation records a source audit and scope decision.
It does not claim runtime or browser acceptance for tool-output clearing.
