# Point 5 consolidation after PR 1014

The integration baseline is `e79c277bdcd12fd08fc5d487d80438c5a059c74d`.
PR 1014 is merged. The same worktree continues on `feat/rust-graph-point5-consolidation`.

## Preservation and scope

The saved graph snapshot remains intact. No stash is applied.
All 118 audited Worker graph paths remain present; four contain newer corrections.
The consolidation retains those corrections and connects missing Main and Web consumers.
It restores the versioned recovery and static-pause contract documents.
It also restores the complete [data shaping catalog](../data-shaping-node-catalog.md).

Workspaces remain in [WF-01](../wanted_feature.md#wf-01--code-workspaces) after full Worker completion and release.
They do not block Point 5 or the current Code node.
Existing shared workspace foundation code remains intact.

## Source and check mapping

| Boundary | Source mapping | Verified result | Remaining proof |
| --- | --- | --- | --- |
| Main static pause persistence and continuation | [Main consumers](../../../../docs/source-mapping/graph-main-static-consumers-20261006.md) | Generated SQL, repository selectors, content inspection, and focused tests pass. | Real PostgreSQL locking, duplicate consumption, and deployed continuation remain open. |
| Web recovery and ordered YAML callers | [Web consumers](../../../../apps/elitea-web/docs/source-mapping/point5-web-consumer-composition-20261006.md) | Focused tests, typechecking, strict lint, and the editor checks below pass. | Deployed recovery controls and nested static execution remain open. |
| Same-consumer restart delivery | [Pending recovery](code-same-consumer-pending-recovery-20261006.md) | 45 focused tests and strict Worker Clippy pass. | Deployed recovery remains open until the exact restart probe passes. |
| Retained hydration recovery | [Shared Supervisor correction](code-retained-hydration-recovery-20261006.md) | Twelve selected tests and strict Clippy pass. The existing CI harness selects three new PostgreSQL regressions. | Deployed restart and the new PostgreSQL fixtures remain open. |
| Fixed Parallel and data-driven Map | Existing distinct contracts and preserved Worker source | Both designs remain present. | Admission stays disabled until assembled acceptance passes. |
| SplitOut and Aggregate | Data shaping catalog | Planned contracts and operation families remain documented. | Rust execution, editor integration, and acceptance remain open. |
| HTTP and database actions | Remaining gates 5d and 5e | Scope remains explicit. | Executable consumers and acceptance remain open. |

Main's focused run records 73 top-level tests and 168 subtests, with no failures or skips.
These are in-process checks. They do not execute PostgreSQL locks or prove durable compare-and-swap behavior.
Web's focused run passes 321 tests across 24 files.
Separate compatibility and guidance selections pass; their counts overlap and must not be added as unique coverage.
Local Web checks use Node 24.19.0. The package requires Node 26 or later.
Exact CI runtime proof remains separate.
The [Web static CI correction](../../../../apps/elitea-web/docs/source-mapping/point5-web-static-ci-closure-20261006.md) preserves existing gates and loading behavior.

## Real-backend editor acceptance

The source Web uses its ordinary Vite proxy against the running rehearsal backend.
No browser response mocks or replacement runtime configuration are installed.
The operator creates only isolated versions of pipeline 144.
Its base version remains unchanged.

Version 167, `gate5-web-roundtrip-20261006`, retains the exact authored 430-character YAML after layout and view changes, save, and reload.
The retained source includes its comment, declaration order, and `x_future: null` descriptor field.
Attachment synchronization retains an unsaved value change from 7 to 9 and appends the required list descriptor.
It retains declaration order and unknown null metadata.
Disabling attachments while the raw source is malformed leaves `state: [` intact and blocks saving.
The operator then restores valid source.

Version 168, `gate5-web-metadata-20261006`, verifies an unreferenced descriptor rename through the State drawer.
After save and reload, `metadata_renamed` retains its type, value, unknown null field, and position after `second`.
The Code inputs remain `first` and `second`.
These editor checks do not execute the Code node.

An immediate YAML-to-Flow switch also retains an unsaved value change from 7 to 11 and its comment.
The operator returns to YAML and observes the exact authored source, including unknown null metadata.
The same immediate switch retains malformed `state: [` and blocks saving.
The operator restores the saved source without executing either draft.
Two real CodeMirror regressions reproduce the former debounce loss and pass after the caller correction.
The correction passes 79 focused tests, full typechecking, and strict lint.
These counts overlap the broader Web selection.

Semantic edits use the existing canonical serializer and remove comments.
State rename changes the declaration only; automatic node-reference rewriting is an existing open editor gap.
Neither behavior is introduced by this increment.
The acceptance does not claim comment retention after semantic edits or automatic reference rewriting.
The local evidence packet contains saved YAML, screenshots, and browser observations.

## Worker restart failure

The deployed Worker-only probe uses pipeline 143, version 166, and persistent chat 825.
Its JavaScript node delays 30 seconds between two platform reads.
The original Code job deadline remains 60 seconds.
The exact Worker process restarts during that node; Main, Supervisor, and Web remain unchanged.

Execution `683d4368779b6924938e1f247968091c`, generation 1, reaches a failed terminal state.
Python completes both reads. JavaScript commits its first read only.
Three calls commit in total. TypeScript and Rust are not admitted.
The original JavaScript runtime remains bound; no successful replacement execution is claimed.

Worker restart occurs at 13:47:55 UTC.
Main's replacement claim arrives about 60.7 seconds later and about 30.9 seconds after the old claim expires.
The Supervisor preserves the original deadline and reports `sandbox.deadline_exceeded`.
The replacement claim then observes that terminal failure without running another 60-second job.

The deployed baseline reads new entries and uses cross-consumer reclaim with a minimum 60-second idle guard.
It has no startup drain for pending entries owned by the same consumer.
The [source correction](code-same-consumer-pending-recovery-20261006.md) periodically scans those entries through existing claims.
It preserves cross-consumer guards and Code deadlines.
Source and deployed recovery acceptance remain separate gates.

The source correction is deployed as Worker image `sha256:aadd0bd9e643cad032c47536197aec75d80b686ca5bf671c660c9bd3873956af`.
Its source revision is `1598367cee55f1ff7d3b8092caebd17becd95627`.
The exact repeated probe is execution `ceb018d0f35756ef44397b88832abacb`, generation 1.
Worker restart occurs at 14:30:05.658 UTC.
The old claim expires at 14:30:35.764 UTC; its replacement arrives about 254 milliseconds later.
Both claims retain checkpoint attestation. This proves the delivery correction reaches Main without the prior extra reclaim delay.

The original JavaScript job still reaches `sandbox.deadline_exceeded` at 14:31:05.748 UTC.
Only three platform reads commit. No TypeScript or Rust runtime is admitted.
The selected native execution Supervisor has measured concurrency of one.
The running submission retains that slot until the original runtime completes.
Restored indexed hydration requests another slot before recognizing its already-dispatched job.
It therefore delays the concurrent broker pump until the original deadline releases capacity.
The [exact-ledger hydration correction](code-retained-hydration-recovery-20261006.md) now passes its selected source checks.
The dedicated Supervisor image build and repeated deployed restart probe remain required.

The graph recovery failure also loses its specific public category through the ADK legacy error wrapper.
The UI therefore displays `INTERNAL` and the generic runtime error.
That diagnostic loss remains recorded; this probe is not an operator rejection or a successful recovery.

Point 5 remains open. Production capability registration remains disabled.
