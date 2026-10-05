# Code lifecycle trace metadata, revision 1

This contract uses existing `NodeEventV1` tool events and `partial_message` deltas.
It adds no protobuf field, trace kind, or store.

The worker emits each phase from its actual Code lifecycle operation.
The phases are `preparation`, `hydration`, and `execution`.
Preparation covers admitted execution-request preparation and optional dependency acquisition and publication.
Hydration covers inert indexed delivery and readiness observation.
Execution covers supervisor submission and terminal-receipt observation.
Submission alone does not prove execution completion or runtime removal.
A failed observation does not prove runtime removal.
An interrupted producer leaves an unfinished trace.

Each phase uses one `tool_call` row.
Its `tool_name` is `<node> / <phase>`.
Its inputs are an empty object.
Its output is null.
Its metadata contains `langgraph_node`, `original_name`, `node_type: code`, `language`, and `code_lifecycle_v1`.

`code_lifecycle_v1` is a closed object, bounded to 2048 encoded bytes.
It has these fields:

| Field | Required value |
| --- | --- |
| `revision` | Integer `1` |
| `execution_id` | Original claim-bound execution identifier; 1–256 UTF-8 bytes; no control characters |
| `generation` | Canonical positive decimal string for the signed output generation |
| `activation_id` | Lowercase SHA-256 hexadecimal Code activation identifier |
| `node_id` | Admitted graph node identifier; at most 128 bytes |
| `graph_thread_id` | Original durable graph thread; 1–512 bytes; no control characters |
| `graph_step` | Canonical unsigned decimal string |
| `language` | `python`, `javascript`, `typescript`, or `rust` |
| `phase` | `preparation`, `hydration`, or `execution` |
| `status` | `started`, `completed`, or `failed` |

`failed` means the worker operation returns an error.
It does not declare a sandbox terminal state or cleanup result.
Only `completed` after a verified terminal execution receipt declares execution completion.
Preparation and hydration completion each describe their own operation.

The run identifier is SHA-256 over length-prefixed identity fields.
The domain is `elitea.graph.code.trace.v1\0`.
The fields are execution identifier, generation, activation identifier, and phase, in that order.
Each length is an unsigned 64-bit big-endian integer.
The identifier is `code-` plus the lowercase hexadecimal digest.
Retries and takeover reuse this identity.
A new graph visit has a different activation identifier.

Existing scoped node-event wrappers carry graph invocation and parent-call ancestry.
Existing signed output envelopes carry the authoritative execution and generation.
Main rejects trace identity that differs from this envelope.
Main rejects unknown fields, malformed values, conflicting replay identity, and backwards lifecycle changes.
An exact terminal replay is idempotent.
A repeated start cannot erase a completed or failed trace.
No source, selected state, dependency declaration, package content, credentials, grants, or fence tokens enter this metadata.
