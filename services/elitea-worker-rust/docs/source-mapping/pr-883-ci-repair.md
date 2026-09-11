# PR 883: CI repair after Main integration

Updated: 2026-09-09. Failing revision: `154dbdba`.

This slice repairs checks on the combined continuation branch.
It does not activate Rust capabilities or close runtime verification gates.

## Failure evidence and ownership

| Check | Cause | Repair |
| --- | --- | --- |
| Go lint | Unchecked response close, unused test field, and two negated Boolean expressions. | Check close errors, remove the unused field, and simplify equivalent conditions. |
| Main database tests | Skill fixtures omit `instructions`. Foreign-project catalogue fixtures omit the joined `applications` table. | Align isolated fixtures with the repository queries and migration schema. Preserve permission and tenant-isolation assertions. |
| Scheduler database tests | Parallel fixtures can generate identical database names within one clock tick. | Add a process-local atomic sequence to fixture database names. |
| Scheduler image scan | The resolved gRPC dependency contains CVE-2026-84445. | Update only the scheduler dependency to `google.golang.org/grpc v1.83.2`. |
| Web lint | Two test files share a basename. Type-aware lint cannot resolve the shadowed TSX test correctly. | Give the logout test a distinct basename. Keep its assertions. |
| Web static checks | An operation count, translations, file budgets, type imports, and one export drift from the merged source. | Update the operation count and translations. Remove redundant comments and the unused export. Move shared authorization types into a leaf module. |
| Web shard 4 | Character-by-character URL entry repeatedly renders the toolkit schema and reaches the test timeout. | Paste the complete URL through user-event. Keep discovery, request-count, and selection assertions. |
| Pipeline versioning journeys | A test expects Main to discard geometry during version creation. Main now persists it. | Assert exact instructions and geometry after creation. Retain the separate unsaved-canvas UI test. |
| Pipeline visual checks | The intended MCP access switch shifts the configuration panel. | Refresh only five inspected pipeline baselines from the pinned Linux CI artifact. Assert the switch before each screenshot. |

The Go failures come from [run 34351217741](https://github.com/elitea-ng/elitea-platform/actions/runs/34351217741).
The web failures come from [run 34351217947](https://github.com/elitea-ng/elitea-platform/actions/runs/34351217947).
The pipeline failures come from [run 34351217780](https://github.com/elitea-ng/elitea-platform/actions/runs/34351217780).
The scheduler scan comes from [run 34351217613](https://github.com/elitea-ng/elitea-platform/actions/runs/34351217613).
The [upstream gRPC release](https://github.com/grpc/grpc-go/releases/tag/v1.83.2) records the security correction.

## Contract boundaries

Main retains ownership of version persistence, catalogue queries, and skill projection.
The database repairs change test fixtures, not product migrations or stored user data.
Skill instructions remain internal to runtime projection. The public skill response still excludes them.

The pipeline editor still creates from stored instructions, then saves the live canvas through a follow-up PUT.
That PUT remains necessary until the creation request carries the admitted live graph.
This slice corrects stale test expectations and comments. It does not remove the live-canvas save.

Authorization type extraction changes dependencies, not decision identity or execution behavior.
Rust sources and protobuf schemas do not change in this slice.

## Verification

- Full workspace Go lint passes with golangci-lint 2.9.0.
- Full workspace race tests pass: 12,457 executed test events and 59 skipped events.
- Database fixtures use the rehearsal PostgreSQL service and clean up their isolated databases.
- The skip ledger names absent Redis, storage, external parity, and separate service fixtures.
- The workspace runner excludes the gateway. These results make no new gateway claim.
- Web type checking and lint pass with warnings denied.
- The focused web selection passes 395 tests across 31 files.
- Shard 4 passes all 2,732 tests using the CI command. Merged coverage remains a separate CI check.
- An earlier concurrent run reaches an unrelated agent-form timeout. The final complete shard rerun passes without changing that test.
- Gate-script tests pass 567 tests across 27 files, with 100 percent decision-logic coverage.
- Budget, endpoint, translation, dependency-cycle, dead-code, visual-coverage, and other static checks pass locally.
- The five Linux screenshot baselines come from CI artifact `10104054553` at the failing revision.
- All screenshot retries match under the existing comparator: threshold `0.05` and maximum difference `3` pixels.
- The screenshot tolerance, coverage thresholds, and permission assertions remain unchanged.

Fresh CI must verify browser journeys, visual baselines, merged coverage, and the rebuilt scheduler image.
No product service is redeployed during this repair.
The [remaining gates](../remaining-gates.md) and [testing gaps](../testing-gaps.md) remain open as recorded.
