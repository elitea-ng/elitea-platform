# Main sync follow-up: 2026-09-09

## Revision boundary

The continuation branch starts at `08f29021`. The fetched Main head is `1dee0c89`.
This merge preserves the existing worktree and branch history.

## Source and target mapping

| Source behavior | New platform ownership | Resolution |
| --- | --- | --- |
| SDK model invocation and execution correlation | Rust `src/transport/{openai_compatible_facade,anthropic_facade,runtime_context}.rs` | Preserve the provider-neutral facade. Forward the authenticated execution ID for gateway cost attribution. |
| Current UI guard decisions and message history | `apps/elitea-web/src/entities/message/lib/wire.ts` and chat-box components | Retain authorization identities and Main memory metadata. Preserve extracted component types. |
| Core toolkit configuration and internal MCP | Main `internal/api/router.go::newToolkitHandler` | Share the handler across REST and MCP. Add Main worker capability metadata. |
| Core project context and events | Main `internal/api/router.go` | Preserve one shared Core handler with delegated authorization, DCR, and domain events. |
| Current Core schema ownership | Main shared migrations | Keep Main versions 122–124. Move branch additions to versions 125–127 without changing SQL bytes. |

Current SDK and Core code define business behavior. They do not require identical implementation.
Detailed legacy source paths remain in the existing agent-runtime and internal-MCP ledgers.
The [migration ledger](shared-migrations-20260909.md) defines required rehearsal reconciliation before deployment.
No running database changes during this sync.

## Verification

- Main API, runtime composition, and startup package tests pass.
- Rust formatting and locked offline Clippy pass for all targets and features.
- Rust transport tests pass: 122 tests, zero failures, zero ignored tests.
- Focused UI tests pass: 94 tests.
- Migration manifest tests and version checks pass. The three new versions have no collisions.
- The repository-pinned OpenAPI generator rebuilds the Go bindings from the merged schema.

These checks do not close deployed runtime, browser, replacement, load, or production gates.
Production capability registration stays disabled.
