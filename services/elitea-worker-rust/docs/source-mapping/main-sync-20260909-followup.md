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
- Focused UI tests pass: 94 tests. Full UI type checking passes after the locked dependency installation.
- Migration manifest tests and version checks pass. The three new versions have no collisions.
- The repository-pinned OpenAPI generator rebuilds the Go bindings from the merged schema.

These checks do not close deployed runtime, browser, replacement, load, or production gates.
Production capability registration stays disabled.

## Integration checkpoint

Commit `44854f72` preserves configuration ID selection across shared read paths.
Its mapping is [configuration IDs](internal-mcp-configuration-ids.md).
The configuration API and application suites pass 1,093 test cases without skips.
The chat and internal MCP integration suites pass 496 test cases without skips.
A separate PostgreSQL regression verifies settings updates require a mapped participant.

The finalized authorization schema regenerates the UI client successfully.
Full UI TypeScript checking passes after regeneration.
The web image `elitea-web:rust-gate3-20260909` builds successfully.
Its image configuration digest is `sha256:bbcb2b918fb0e6c0686ce1562e64ae9a1f73ae69b629c2496406aabab9d65219`.

Playwright reaches the rehearsal chat and skill pages.
Those pages still use the earlier deployed images.
This observation does not verify the new draft or Toolkit Test contracts.
The new Main, Rust, and web integration deployment remains pending at this historical checkpoint.

## Later deployment and delivery

The [point 3 audit](point3-audit-20260913.md) supersedes this checkpoint's deployment status.
It links subsequent chat, Toolkit Test, credential, and independent external MCP acceptance.
Commits `2f206087` and `82e0ef3a` preserve the remaining verified point 3 changes and their source mappings.
See [delivery reconciliation](point3-delivery-reconciliation.md) for the current delivery boundary.
