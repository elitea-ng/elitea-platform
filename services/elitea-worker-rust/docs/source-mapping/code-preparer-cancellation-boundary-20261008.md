# Code preparation cancellation boundary

Date: 2026-10-08.

## Product behavior and evidence

The current application's Stop behavior remains the product reference.
Normal UI Stop cancels the whole execution, including its pending dependency preparation.
[Chat850](code-pending-stop-owner-recovery-20261008.md) verifies this behavior after Worker loss.
The replacement stops the original preparation, resolves its dispatch, and commits one canonical CANCELLED settlement.
No user Code or downstream node starts. Runtime removal, History, and Restore Test pass.

The separate preparation-cancelled diagnostic describes an internal outcome while the execution remains RUNNING.
The shipped UI and public application API provide no preparation-only cancellation action.
Canonical UI Stop cannot prove this distinct diagnostic because it first changes the execution's desired state to CANCELLED.
The existing component checks retain their stated scope. This review adds no runtime or component-test evidence.

## Source ownership

| Owner | Source | Boundary |
| --- | --- | --- |
| Main | `internal/db/queries/agent_cancel.sql`, lines49–74 | Normal Stop changes the original execution's desired state to CANCELLED. |
| Main | `internal/api/v2/agentexecution/cancel.go`, lines14–17,46–62,77–102 | The authenticated public DELETE route requests whole-execution Stop. |
| Main | `internal/transport/runtimegrpc/control/sandbox_grant.go`, lines68–113 | The current fenced Worker can request a revision2 cancellation-only sandbox grant. |
| Main | `internal/infra/db/repos/output_inbox.go`, lines423–430 and483 | CANCELLED authority accepts a cancellation failure and settlement, and rejects an ordinary FAILED settlement. |
| Rust Worker | `src/protocol/sandbox_authority.rs`, lines63–79 | Replacement output recovery requires the original cancelled execution binding. |
| Rust Worker | `src/execution/agent_delivery_processor.rs`, lines396–427 | Cancellation recovery confirms original sandbox Stop before terminal output. |
| Rust Worker | `src/execution/agent_preparation.rs`, lines1286–1294 | Terminal cancellation confirms sandbox Stop before publication. |
| Rust Worker | `src/agents/graph/code_remote.rs`, lines464–508 | Stop resolves the original terminal preparation or execution dispatch. |
| Rust Worker | `src/sandbox/client_preparation.rs`, lines245–246 | A cancelled preparation receipt with no bundle becomes `PreparationOutcome::Cancelled`. |
| Rust Worker | `src/agents/graph/code_preparation.rs`, lines328–333 | The internal cancelled outcome resolves the original dispatch and selects the finite preparation-cancelled diagnostic. |
| Web | `apps/elitea-web/src/pages/pipelines/lib/usePipelineChatAdapter.ts`, lines122–132 | Editor Stop uses whole-execution cancellation. |
| Web | `apps/elitea-web/src/entities/conversation/api/conversationApi.ts`, lines304–317 | The conversation client submits the normal DELETE request. |

Main paths are relative to `services/elitea-main/`.
Worker paths are relative to `services/elitea-worker-rust/`.
Web paths are relative to the repository root.
[Preparation diagnostics](code-preparation-diagnostics-20261005.md) records the diagnostic mapping and its earlier component evidence.
This review changes no application source, cancellation contract, grant, or output admission rule.

## Exact deployed source review

Worker source is `c53ab7d5496a571c1844e9906a83c261ff0da06e`.
Main source is `efa7213e803d6f314ee8b6c482b78f45544140b4`.
All nine reviewed files match their deployed source blobs and the current worktree.
The source verification receipt is `18096d2d3b4fa193b030e889f5a5b668d70194d8b4ce013cec18bd694ceb509b`.
The review reads source only. It issues no grant, cancellation request, database mutation, or runtime action.

Deployed Web source is `42a0c390bc19bd46f0a3eae2f244714024595d71`.
Its pipeline adapter and conversation API call the same whole-execution DELETE route.
Both caller files and Main's public cancellation handler match their deployed source blobs and the current worktree.
The entrypoint receipt is `c747f4406362f473ef59d0ae1e5ed8879875fb26daa62c304501a7934ad8ba18`.
The deployed Web, public Main API, and Main application searches find no preparation-only sandbox cancellation caller.

## Proposed caveat and completion scope

The proposed Code acceptance scope retains the verified whole-execution UI Stop behavior.
The preparation-only cancelled message retains component evidence and an explicit missing deployed proof.
No private grant extraction, synthetic cancellation caller, or new product action supplies substitute evidence.
Owner acceptance is pending. This proposal does not close Code, graph point5, or the full worker goal.

Preparation failure, unconfirmed completion, applicable debug reconciliation, NATS recovery, Kubernetes, and live canvas verification remain separate requirements.

## Implementation history

The diagnostics change preserves finite safe messages without changing cancellation decisions.
Chat850 later verifies the normal UI cancellation path across Worker loss.
This source review explains why that accepted path cannot also prove the preparation-only FAILED message.
It replaces an unreachable normal-UI acceptance scenario with a concrete proposed scope boundary.
