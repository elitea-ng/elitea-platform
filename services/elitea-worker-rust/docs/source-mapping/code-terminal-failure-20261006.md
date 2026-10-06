# Typed Code terminal failures

The Code node preserves a finite failure category through the graph wrapper and Main output admission.
This correction replaces generic public failures for supported terminal Code cases.
It preserves settled history and the existing recovery policy.

## Source ownership

| Boundary | Source | Behavior |
| --- | --- | --- |
| Code category | `src/agents/graph/code_runtime.rs::terminal_failure_code` | Map validated classes to fixed internal codes. Preserve authorization, cancellation, and preparation distinctions. |
| Fresh failure | `src/agents/graph/node_recovery_runtime.rs::RecoverableNode::execute_inner` | Commit the failure and validate the current writer before publication. Return after one report. |
| Restored failure | `NodeAttemptBody::report_restored_terminal_failure` and the Code override | Project the validated persisted class without executing, reconciling, claiming, or rewriting the journal. |
| Protocol | `src/protocol/output.rs::model_failure` | Map Code authorization and cancellation aliases to existing public enums. |
| Main admission | `services/elitea-main/internal/transport/runtimegrpc/output/server.go::runtimeFailurePolicyForError` | Accept the three exact existing preparation messages under `PIPELINE_CODE_FAILED`. |

Fresh generic Code failure uses `PIPELINE_CODE_FAILED`.
Authorization and sensitive rejection use `AUTHORIZATION_FAILED`.
Whole-Code cancellation uses `CANCELLED`; preparation cancellation retains its existing preparation message.
All public messages remain fixed and nonretryable.
Raw sandbox logs, source, SQL, and private errors do not become public authority.

The replay hook defaults to no operation for other node types.
Lease loss, operator approval, reconciliation, retries, and error routes do not produce a Code terminal signal.
Parent cancellation retains its existing control path.
Historical journals cannot restore preparation details that were never stored.
They restore the persisted class only.
Existing settled `INTERNAL` messages remain immutable.

No public enum, protobuf, database schema, ADK, dependency, retry, or deadline changes.

## Focused verification

The source baseline is `1f53522936adab8f72f1b77521e620718a09c6b9`.
The reviewed six-file patch SHA256 is `559b71c048ded8d284c3deb21b9a086cf053f421afe795b6987e459b6f8c1147`.
The source manifest SHA256 is `c8482edfb577a681c8431739131be1c9012ca33e07700d034fdea9d60bf933c0`.

Locked offline Cargo checks use one build job and two test threads:

- Recovery group: 33 pass, zero failures or ignored tests.
- Code runtime group: 11 pass, zero failures or ignored tests.
- Protocol output group: 6 pass, zero failures or ignored tests.
- Strict all-feature library and test Clippy: exit zero.

The actual graph wrapper and native runner preserve the finite category.
Tests cover failure before dispatch, post-dispatch authorization, cancellation, rejected append, and writer revocation after commit.
Restored journal tests retain the exact revision, attempt history, and checkpoint identity.
They make zero additional runtime calls.

Main output checks pass 31 top-level tests and 109 cases, with no skips.
Admission tests reject altered messages, wrong categories, wrong digests, appended private details, and `retryable=true`.
Focused vet, Rust and Go formatting, and patch whitespace checks pass.
Cargo manifests, lockfile, and shared preparation fixtures remain unchanged.

These checks use in-process doubles on macOS ARM64.
The existing native compact-unwind linker warning remains recorded.
They do not prove Linux, live PostgreSQL, mTLS, sandbox processes, or deployed UI acceptance.

## Remaining acceptance

Deploy the corrected Worker and Main after the separate migration-preservation gate passes.
Verify an isolated valid pipeline whose source variable is empty at runtime.
Require the exact Code error, zero sandbox dispatches, no later node execution, and the same persisted error after reload.
Browser reload alone does not prove journal replay.
Actual replay requires an authorized current-claim continuation before public settlement.

The [v7 restart proof](code-supervisor-task-ownership-20261006.md) remains the separate positive Worker-loss boundary.
Workspaces remain in [WF-01](../wanted_feature.md#wf-01--code-workspaces), after full Worker completion and release.
