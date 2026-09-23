# Nested pipeline checkpoint scope

Date: 2026-09-23.
Status: component checks pass; deployed acceptance remains open.

## Failure evidence

The deployed parent pipeline uses an Agent node to call an LLM-only saved pipeline.
Conversation 665 fails before the child model starts.
Execution `c6b72ef9ca9e1d3b3b161c474b92200b` has only the root session and root graph frontier.
The browser receives `INTERNAL`, not the expected continuation failure.

The compiler passes the root checkpointer to the ADK subgraph.
ADK derives the child thread as `<parent-thread>/<node-id>`.
The PostgreSQL adapter permits only its exact bound thread.
The child load therefore fails before model execution.
Memory-backed tests do not enforce this boundary and miss the mismatch.

## Source mapping

| Behavior | Source | New ownership |
| --- | --- | --- |
| Saved application graph composition | Current SDK pipeline behavior; see [pipeline mapping](pipeline-nodes.md). | `agents/pipeline.rs::NativePipelineApplicationResolver` and `agents/graph/application.rs::execute_pipeline` |
| Native child thread identity | ADK 2.2.0 `adk-graph/src/subgraph.rs::SubgraphNode` | Preserve ADK thread derivation. |
| Exact durable checkpoint ownership | `state/postgres_checkpointer.rs::CheckpointScope::require_thread` | Preserve exact scope checks on every underlying adapter. |
| Admitted child identities | `agents/graph/compiler.rs::PipelineDefinition::application_node_ids` | Derive child nodes from the validated pipeline definition. |
| Child adapter activation | `state/postgres_checkpointer/application_children.rs::with_application_children` | Activate one independently fenced adapter per admitted child. |
| Runtime composition | `agents/session.rs::activate_pipeline_postgres` | Supply the admitted checkpoint family before graph compilation. |

## Implementation

The family contains the root thread and at most 128 admitted application-node threads.
Each adapter retains tenant, project, definition, execution, claim, lease, and workload authority.
Child activation reuses the existing authority derivation and PostgreSQL writer fencing.
Unknown sibling and deeper descendant threads fail closed.
Checkpoint-ID lookup searches only the bounded admitted family.
The existing turn wrapper still separates fresh turns from checkpoint recovery.

No application schema or checkpoint migration is required.
This change does not admit deeper pipeline definitions that the current compiler rejects.
Ordinary-agent pipeline tools remain a separate checkpoint ownership path.

## Verification contract

The PostgreSQL regression runs an ADK subgraph through the admitted family.
It checks child persistence, checkpoint-ID lookup, unknown-thread refusal, replacement claim recovery, and stale-writer refusal.
Browser acceptance must rerun the supported nested pipeline after deployment.
Successful component checks alone do not close that acceptance gate.

## Component results

All 1,214 worker library tests pass with PostgreSQL available.
Clippy passes for the library and tests with warnings denied.
The new PostgreSQL regression executes the child graph and verifies claim takeover.
