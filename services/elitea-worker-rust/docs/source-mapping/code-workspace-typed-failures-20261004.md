# Code repository failure propagation

This amendment preserves typed workspace failures through the Code attempt owner.
It changes no request, grant, storage, runtime, or workspace mode.
The repository workspace activation gate remains false.

## Owning sources

| Source | Existing behavior | Amendment |
| --- | --- | --- |
| `src/agents/graph/code_workspace_remote.rs` | Acquisition and hydration erase causes into `GraphError`. | Return a bounded typed category at each existing failure boundary. |
| `src/agents/graph/code_workspace_failure.rs` | No workspace-specific typed error exists. | Match actual input and sandbox variants without parsing messages. |
| `src/agents/graph/code_attempt_remote.rs` | Acquisition becomes invalid input. Hydration becomes unknown. | Preserve the category, original dispatch activation, phase, and Started status. |
| `src/agents/graph/code_compiled.rs` | Cold workspace hydration becomes a general compilation error. | Retain workspace errors through snapshot selection. Keep the legacy `GraphError` interface. |
| `src/agents/graph/code_workspace_failure_tests.rs` | No workspace seam fixtures exist. | Exercise denial, Stop, redaction, and original-dispatch reconciliation through the existing planner. |

The existing `InputContentError`, `SandboxCallError`, and `ControlGrpcError` enums supply the categories.
The error stores no provider message, transport cause, grant, or user data.
The legacy workspace error uses one fixed safe message.
Existing compilation errors retain their previous value and unknown classification.

## Typed categories

| Actual error | Node category |
| --- | --- |
| Input content authorization failure; rejected sandbox grant; permission denied | Authorization denied |
| Unauthenticated submission | Authentication denied |
| Cancelled submission | Cancelled |
| Deadline or input content timeout | Attempt timeout |
| Invalid local input; bounded input content exhaustion | Invalid input |
| Invalid configuration | Invalid configuration |
| Invalid receipt; data loss | Invalid result |
| Content dependency or transport failure; unavailable or aborted submission | Dependency unavailable |
| Submission resource exhaustion | Rate limited |
| Other submission status | Unknown |

`ControlGrpcError` exposes configuration, exhaustion, and unavailability only.
This amendment does not infer hidden denial or cancellation from its message.
An upstream erased status remains outside this typed seam.

## Original effect and policy

A fresh attempt retains `NoExternalEffect` for the whole Code effect before user execution.
This does not prove that no acquisition record or inert preparation runtime exists.
A previously Started attempt retains `UnknownExternalEffect` with the original whole-dispatch identity.
Hydration does not create a replacement identity or authorize replay.

The existing recovery planner remains unchanged.
Its default fresh denial result is `Stop(RetryDisabled)`.
Its Started denial result is `Stop(NotRetryable)`.
Cancellation returns `Stop(ControlDecision)` for fresh and Started attempts.
A Started timeout or unknown result requires reconciliation of the same dispatch.
Broker call detail never replaces that whole-dispatch identity.

Compile workspace errors use the hydration phase.
Other compilation errors keep the preparation phase and unknown category.
The transient hydration retry set, deadline, cursor, and original-runtime behavior remain unchanged.
The false gate, request digests, grants, claims, and immutable manifests remain unchanged.

## Fixtures and verification

Eight pure fixtures are authored in the typed error module and compiled adapter module.
They use synthetic causes and the actual attempt classifier and recovery planner.
They do not create a claim, runtime, catalog, repository, or database.
All eight fixtures remain unrun.
Private formatting and captured-baseline patch checks do not prove compilation or runtime acceptance.

Two separate fixture corrections precede this amendment.
The broker correction fixes timeout reconciliation and denial Stop expectations.
The recovery owner correction fixes the guard Stop matrix.
Both corrections preserve the current planner and production source.
The packet manifest records their exact patches and composed baseline.

Run the focused typed error and compiled adapter fixtures after root composes the owning sources.
Then run adjacent Code attempt fixtures and strict checks on the exact assembled bytes.
Keep the workspace gate false until the existing authority, persistence, hydration, and recovery gates pass.
