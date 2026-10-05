# Code lifecycle trace producer

The private slice adds actual Code lifecycle trace events.
The current Code node sends a failure signal but no successful phase events.
A successful Code pipeline can therefore have no Code tool rows in Run History.
This source finding does not measure the deployed trace count.

| Current source | New source | Behavior |
| --- | --- | --- |
| `agents/graph/code_runtime.rs` | Original node context plus `code_trace.rs` | Preserve admitted node, graph thread, step, and activation |
| `protocol/sandbox_authority.rs` | Read-only `trace_identity` | Expose execution identifier and generation only |
| `agents/graph/code_remote.rs` | Phase observation wrappers | Emit preparation, optional hydration, and execution observation boundaries |
| `agents/graph/node_events.rs` | Existing bounded sender and drain | Preserve graph invocation and descendant scope |
| `agents/events.rs` | `events_code_trace.rs` | Emit existing tool start/end events and partial tool deltas |
| `execution/native_agent_lifecycle.rs` | Existing publisher | Publish the projected events under the original verified claim |
| `execution/output_delivery.rs` | Existing durable signed envelope | Preserve execution and generation fences and output sequence |
| Main `infra/db/repos/agent_trace.go` | `agent_code_trace.go` | Check frame identity and preserve bounded typed metadata |
| Main ordinary trace deduplication | Code-only identity branch | Keep distinct loop activations and phase rows |
| Web `entities/run-history/ui/RunHistoryTrace.tsx` | Existing trace rows | Display phase labels through the current tool-call renderer |

The producer uses `NodeContext.config.thread_id` and `NodeContext.step`.
It does not use the Application activation registry.
The existing reserved event scope carries the original descendant parent call and checkpoint thread.
The existing signed envelope remains authoritative for execution and generation.
Main refuses a conflicting producer identity before trace writes.

Preparation completion describes execution-request preparation and optional dependency acquisition and publication.
Hydration completion describes inert indexed delivery and readiness observation.
Execution completion follows a verified completed supervisor receipt.
A failed observation does not prove sandbox removal.
A dropped observation has no synthetic completion.
No trace contains source, state, dependency declarations, package bytes, credentials, grants, or fence tokens.

The metadata contract is `libs/proto/elitea/runtime/v1/code_lifecycle_v1.md`.
This slice adds no protobuf field, generated binding, migration, trace kind, or store.
Existing signed output fences, Main admission checks, and browser read fences remain necessary.

## Verification

Private Main checks pass with CPU parallelism two.
The new suite has six top-level tests and 51 subtests.
The combined trace regression suite has 27 top-level tests and 66 subtests.
Both suites have zero skips.
The combined suite includes the new suite.

The Worker has four authored producer tests and two authored projector tests.
They have not run in this private dependency layer.
The compiled runtime and family producer are required assembly inputs.
Changed Worker files pass direct `rustfmt` checks with child discovery disabled.
Main files pass `gofmt` checks.
Patch apply checks use private baseline files only.
No Docker, Kubernetes, database, browser, deployment, commit, or push runs occur for this slice.

## Required acceptance

Run the assembled Worker producer and projector tests.
Run assembled Worker checks and Clippy with the owning dependency freezes.
Exercise four-language execution with dependency preparation and without dependency preparation.
Verify each actual phase produces one trace row per activation.
Verify scoped descendants retain the original graph and parent-call identities.
Verify a loop visit produces a distinct activation row.
Verify fresh-owner recovery preserves a completed row and an unfinished phase remains unfinished until observed.
Verify failure and cancellation expose safe status without source or runtime payloads.
Verify history list and detail enforce the original receipt binding.
Use Docker and Kubernetes browser acceptance for conversation and editor Test.
Record original execution, generation, node, activation, output sequence, and trace row count.
