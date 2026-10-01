# Main integration and release preparation

Date: 2026-10-01. Gate 5 remains active.

The feature parent is `e13a4e4ad42e3b14ff84b1140ee6146938c375f1`.
The incoming main revision is `8563c2d75`.
This integration prepares PR 883 for release. Unfinished dependency preparation belongs to the next PR.

## Source and ownership mapping

The [Code isolation assessment](code-node-isolation-assessment-20260928.md) maps current SDK behavior to Rust execution.
The [crash continuation mapping](agent-crash-continuation.md) defines checkpoint and recovery ownership.
Current platform code remains a business reference. It does not define the new admission or recovery implementation.

| Incoming main owner | Preserved Rust contract | Integration decision |
| --- | --- | --- |
| Main `internal/infra/db/repos/agent_execution_jobs.go` and `internal/db/queries/runtime_agent_execution.sql` | Claims bind admitted execution identities and immutable inputs. | Retain the short reservation transaction and separate materialization transaction. Regenerate both query sets together. |
| Main `internal/runtimecomposition/agent_admission_reservation_reaper.go` | Rust owns graph checkpoints and model sessions. | Reclaim leaked admission slots without moving checkpoint ownership to Main. |
| Python `transport/reconnect_channel.py` and `execution/delivery.py` | Replacement workers recover through persisted claim and checkpoint authority. | Retain main's reconnect and delivery-cap changes. Audit Rust separately. |
| Web `useChatBoxData.seed.ts` | Live execution output survives transcript refresh. | Retain main's protection against replacing a streaming transcript with an older seed. |
| Docker and Helm object initialization | Sandbox artifacts use the admitted object-storage service. | Retain RustFS readiness and CLI initialization changes. |

Generated `querier.go` contains both toolkit-read operations and admission-reservation operations.
Use sqlc 1.31.1 to generate and vet the merged query sources.
Do not resolve generated conflicts by hand.

Materialization again reads `LoadRuntimeAdmissionTiming` from PostgreSQL.
Publication and expiry use the same database clock.
The incoming host-clock optimization can change deadlines when Main replicas have different clocks.
The timing regression verifies stored timestamps, exact TTL, publication eligibility, and replay with both caller-clock directions.
The fix restores one database round trip. No latency measurement is claimed.

## Migration collision

Main owns shared migration 126 for admission reservations.
Feature migrations 126 through 132 move to 127 through 133.
All seven SQL files retain their exact bytes.
The shared head becomes 133. The tenant head remains 138. The agentstate head is 9.
This merge adds no replacement application schema.

| Previous feature filename | Current filename | Unchanged SHA-256 |
| --- | --- | --- |
| `0126_toolkit_execute_read.sql` | `0127_toolkit_execute_read.sql` | `c2497df59b29fec9230c0164e2e1e44d9c1f5d0d0bf08d7ae1dfd0aea4535e71` |
| `0127_mcp_prebuilt_parameter_schema.sql` | `0128_mcp_prebuilt_parameter_schema.sql` | `e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289` |
| `0128_mcp_oauth_clients.sql` | `0129_mcp_oauth_clients.sql` | `62bf7e4c22d2bba3375b216fd578aa1fdb57b1243dada093126d2076b8445515` |
| `0129_toolkit_available_tools.sql` | `0130_toolkit_available_tools.sql` | `a7250a118531f6a9cb39b15790cb8e7d7f62a485e1cbd324dee83bfc1389a913` |
| `0130_mcp_oauth_tokens.sql` | `0131_mcp_oauth_tokens.sql` | `88ce87eb8b489b33fe2ca72936323fb6a2b99198d403ff85e7bfbf560d7ada37` |
| `0131_agent_model_checkpoint_claim.sql` | `0132_agent_model_checkpoint_claim.sql` | `ba861ea8f7752e5f8fd14ff64065b211a89208676fbca30b1b151fd5e4ca5a86` |
| `0132_agent_full_context_input.sql` | `0133_agent_full_context_input.sql` | `c105f0dc25dde00168bc26f0b073378601e706bbbcd86afa98959b0f2c14c476` |

Historical mappings retain their original filenames. Use this table for the current source paths.
Integration fixtures now read the current filenames.
The ledger regression accepts main's prefix through 126 and rejects unreconciled feature toolkit execution at 126.

No database or ledger changes occur during this merge.
Hold rehearsal deployment until its actual ledger is inspected and explicitly reconciled.
Do not disable checksum validation or rerun applied SQL to bypass a mismatch.

## CI corrections

The published failure logs belong to an older synthetic merge revision.
Their status does not prove the current feature parent passes or fails.
Run fresh checks after pushing the resolved candidate.

The catalogue drift test now reads the implemented `ASK_USER_TOOL_NAME` constant.
The agentstate manifest assertion now includes the committed sandbox history through migration 9.
Both corrections retain strict catalogue and manifest validation.

Dependency scanning now covers the supervisor and compiled Rust adapter manifests.
Patched ADK manifests use the worker lockfile's dependency closure.
Their scanner exemptions explain why upstream source and patches move together.
The data-processing manifest remains a fixed acceptance fixture.

## Preserved Point 5 work

The pre-merge checkpoint contains 81 tracked and untracked files.
A local preservation manifest verifies each file against its saved SHA-256.
Retain the checkpoint until restoration and verification finish on the follow-up branch.

The checkpoint includes Python preparation, indexed content delivery, inert hydration, dispatch fencing, and deployment controls.
Focused verification passes before preservation:

- Strict feature Clippy passes.
- Sandbox tests report 66 passes and 20 ignored infrastructure tests.
- Code tests report 37 passes.
- An isolated PostgreSQL probe reports 14 recovery-test passes.
- The real Docker transfer probe reports one pass and removes its owned containers.

These results belong to the preserved checkpoint, not the release candidate.
On-demand deployment and UI acceptance remain open.
JavaScript, TypeScript, and Rust native preparation have component evidence.
Their shared-storage integration remains open.
Transfer timing and final-dispatch budget separation also remain open.

## Release acceptance

Local candidate checks pass:

- All eleven Go workspace modules pass tests, vet, and pinned strict lint.
- Isolated PostgreSQL 18, pgvector 0.8.1, and Redis tests execute 13,762 cases. Thirty-seven infrastructure cases skip.
- Rust runs 1,453 tests successfully. Nineteen infrastructure tests remain ignored.
- Strict Rust Clippy passes for all targets and features.
- Fifty-nine focused Python reconnect and execution tests pass.
- Web lint, typecheck, budgets, translation, cycle, dead-code, and contract checks pass.
- The full Web suite passes 15,440 tests and exposes two stale Code-node assertions.
- Both assertions are corrected. The twenty focused allow-list tests pass after correction.
- The production Web application builds. SQLC and OpenAPI outputs reproduce through their pinned generators.
- Helm lint, chart rendering, dependency coverage, and sixty-six drain-script assertions pass.

The new Web bundle is deployed into the existing rehearsal Web container.
A fresh browser verifies the ten supported node types and all four Code language controls.
Pipeline 141 fails at Python during its editor Test run. The UI displays the typed failure and support reference.
The browser reports no console errors. Runtime receipt diagnosis remains open.
This Web check uses the existing rehearsal Main and worker images. It does not prove deployment of the merged Main.

Fresh GitHub CI and successful browser runtime acceptance remain required before release.
Do not treat preserved checkpoint tests as merged-candidate evidence.
Do not close Gate 5 when PR 883 merges.
