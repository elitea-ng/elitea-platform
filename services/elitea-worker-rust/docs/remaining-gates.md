# Rust worker remaining gates

Updated: 2026-09-09. Main baseline: `f28189b5`.

This register orders implementation work. The [testing register](testing-gaps.md) owns detailed proof requirements.
Source mappings remain the behavioral evidence for each capability.
Current-platform code defines business behavior, not a requirement to copy its implementation or security defects.

## Progression status

- Gate 1 has pipeline turn, regeneration, and nested-resume fixes with regression coverage. Mixed-guard and collaborative proofs remain open.
- Gate 2 is accepted for progression, with explicit verification debt. OAuth/DCR support is not a claim of complete provider coverage.
- Gate 3, internal and external MCP, remains the active implementation slice.
- Production Rust capability registration remains disabled.

## Implementation order

| Order | Remaining capability | Closure boundary |
| --- | --- | --- |
| 3a | Internal MCP builder completion | Publish skill and project-context drafting through shared Main services after permission, prompt, edit, and failure-contract checks. |
| 3b | Internal MCP discovery and entity parity | Complete tag relation metadata, Search Options, actor-safe chat operations, and the remaining configuration, skill, application, and secret contracts. |
| 3c | Toolkit discovery and Test | Implement shared `toolkit.available_tools.v1` and `toolkit.call_tool.v1` in Rust. Preserve exact selection, authority, result shape, cancellation, and recovery. |
| 3d | External MCP | Complete dynamic instance schemas and saved-agent/pipeline terminal, failure, mixed-guard, resume, and replay proofs. |
| 4 | Current-platform runtime drift | Audit context management, summary models, continuation, tool-output editing, skills, project context, provider errors, and tool naming. |
| 5 | Remaining graph capabilities | Complete deeper pipeline composition, child variables, static pauses, and isolated Code nodes. |
| 5a | New parallel and map nodes | Implement the separate fixed-branch and data-driven designs with durable child state, bounded concurrency, reducers, and recovery. |
| 6 | Effectful toolkit operations | Require durable intent, effect receipts, idempotency, approval, fencing, and crash reconciliation before writes. |
| 7 | Artifact-backed capabilities | Complete attachment authority, object grants, storage behavior, and affected toolkit operations. |
| 8 | Indexing | Implement indexing after the agent and artifact gates. Indexing remains last. |

Read-only runtime tool binding already works for supported native families.
It does not close standalone editor discovery or toolkit Test.
OpenAPI supports delegated OAuth and client credentials. DCR remains an MCP or other explicitly supported toolkit flow.

## Immediate next slice

Verify the repaired PR checks first. The [CI repair ledger](source-mapping/pr-883-ci-repair.md) records this prerequisite.
CI repair does not close any runtime gate below.

Audit skill and project-context draft generation against the current platform.
Reuse Main's composed draft service instead of adding a second model execution path.
Verify source prompt ownership, permissions, edit-by-ID behavior, validation, and safe failures before publishing either internal MCP operation.
Keep application drafting unpublished: its legacy operation has `mcp_tool=False`.
Update source mappings, component tests, protocol tests, and deployed-proof requirements in the same slice.

## New main changes already incorporated

- Entity-filtered tag listing now exists. Relation metadata and internal MCP publication still require review.
- Version metadata updates merge supplied keys instead of replacing unrelated runtime settings.
- Application timestamps now advance with version changes through tenant migration `0134_application_updated_at.sql`.
- Vault writers use shared locking. Internal MCP retains the shared Main secret handlers.
- Chat participant, canvas, credential, model-grant, and E2E changes are merged.

The [sync ledger](source-mapping/main-sync-20260909.md) records the combined contract and verification boundary.
Merged UI code is not deployed-browser proof.

## Verification debt and production activation

Keep TG-01 through TG-16 open until their completion requirements have evidence.
Priority proofs include sensitive decisions, independent mixed parallel guards, sibling reuse, active-run reauthorization, and competing-tab behavior.
External saved-agent and pipeline terminal completion requires separate proof from a settled authorization pause.
Typed configuration tools and copied skills still require deployed chat/editor-to-runtime checks.
Investigate `agent_session_terminal_completion_unavailable` and preserve complete history across replacement.
Keep detailed diagnostics under `OBS-RUST-01`.

Production activation requires replacement, reclaim, lost-acknowledgement, Redis TLS/ACL, load, soak, and Kubernetes proofs.
Another worker must continue unfinished work without the original process or its local spool.
Do not count skipped tests, mocks, or a matching final answer as system proof.

## Source ledgers

- [Internal MCP](source-mapping/internal-elitea-mcp.md)
- [External MCP](source-mapping/external-elitea-mcp.md)
- [Toolkit discovery](source-mapping/toolkit-discovery.md) and [toolkit Test](source-mapping/toolkit-test.md)
- [Delegated OAuth/DCR](source-mapping/delegated-oauth-dcr.md) and [active-run expiry](source-mapping/delegated-auth-expiry.md)
- [Agent runtime](source-mapping/agent-runtime.md) and [pipeline nodes](source-mapping/pipeline-nodes.md)
- [Toolkit families](source-mapping/configuration-toolsets.md) and [indexing](source-mapping/indexing.md)
