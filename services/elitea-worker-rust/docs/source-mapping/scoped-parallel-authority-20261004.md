# Scoped checkpoint authority for fixed Parallel

This packet preserves scoped application receipts across fixed Parallel checkpoint writes.
It also retains exact admitted child paths and inherited event identity.
Shared source remains unchanged until root reviews assembly.

## Source mapping

| Existing contract | Private change | Compatibility |
| --- | --- | --- |
| `state/postgres_checkpointer/application_children.rs`: exact application path activation and thread routing | Derive exact parent-thread and owned-node families from the admitted frozen path catalog. Forward atomic append through the same routed adapter. | Existing ordinary save, load, delete, prune, ancestry, depth, and path limits remain unchanged. |
| `agents/pipeline/scope_receipts.rs`: graph call originals and revision validation | Export the composed adapter in `scope_receipts_parallel.rs`. | Ordinary graph call validation remains the owner of immutable receipt transitions. |
| `agents/pipeline/scope_receipts_parallel.rs`: new capability adapter | One authority Arc supplies ordinary saves, atomic appends, and child creation. | There is no raw PostgreSQL bypass, downcast, caller-defined identity, or append fallback. |
| `agents/pipeline/scoped_applications.rs`: exact registered graph paths | Wrap a previously verified authority with the registered metadata gate. Initialize ordinary call owners without a Parallel sidecar. | Unknown graph paths fail. Ordinary scope admission remains unchanged. |
| `agents/graph/node_events.rs`: bounded invocation event bridge | Retain an invocation-owned Parallel sidecar outside graph business state. | Ordinary event routing and serialized static scope validation remain unchanged. |
| `agents/graph/node_events_parallel.rs`: new sidecar | Validate the factory's exact child catalog and captured parent scope. Preserve ancestor attribution across the hashed edge. | A hashed thread prefix never supplies authority. Local scope chains still require admitted static edges. |
| `agents/pipeline/scoped_runtime.rs`: saved Agent and saved graph call ownership | Clone existing owners with the branch event sender. Use inherited identity before minting original call receipts. | Scope, node identity, application projection, and preparation gate are retained exactly. |

## Authority and receipt rules

The compiler first verifies its incoming checkpointer Arc against the binding's authority Arc.
The registry then wraps that same authority.
The returned object supplies all three checkpoint protocols.
The Parallel owner implements this compiler check and the atomic PostgreSQL writer.

The child factory requires an exact admitted parent thread and owned Agent node.
Its descendant catalog contains only paths already admitted from frozen definitions.
The underlying parent adapter mints the hashed branch root from its authenticated claim scope and exact occurrence inputs.
The branch result carries its exact activated thread set before erasing the child checkpointer.

New atomic receipt writes validate candidate and expected-parent codecs before delegation.
A new graph call revision still requires the existing original, frontier, and graph-state proof.
Receipt-only freeze and decision revisions retain the exact original receipt map.
A supplied changed or erased receipt map fails.
A forged expected parent cannot pass the store's full-parent comparison.
Exact immutable identifier replay preserves latest ordering.

The event sender accepts only the sealed branch execution catalog and its captured parent control value.
The current parent scope must match the activation's exact root thread.
Parent and child thread sets cannot overlap.
The combined event ancestry remains bounded to three scopes.
Unknown local threads, skipped local roots, cycles, and neighboring prefixes fail.
A descendant context without its local saved scope fails before original receipt preparation.
Business mappings never receive the inherited event control value.

Existing routed ordinary leaves retain their nearest receipt edge.
Synthetic graph starts and terminal events acquire only their proved container metadata.
Their identifiers, content, arguments, invocation, author, branch, and timestamps are retained.
Inherited invocation identity is selected before the immutable start is saved.

## Fixtures and integration

`application_children.rs` tests exact parent and node selection, neighboring names, missing ancestors, duplicate paths, depth, and catalog size.
`scope_receipts_parallel_tests.rs` tests original retention, atomic freeze and decision, exact replay, forged parents, malformed receipts, and valid graph call revisions.
`node_events_parallel_tests.rs` tests hashed sidecar admission, unchanged static validation, depth, ancestry, immutable originals, routed leaves, and ordinary root behavior.

These fixtures use a bounded in-memory atomic adapter.
They do not prove PostgreSQL fencing, cross-process recovery, provider effects, or browser acceptance.
The Parallel owner's assembled private suite must test these exact bytes with its compiler and PostgreSQL contracts.
The packet verification file records executed checks separately from authored fixtures.
The final missing-local-scope guard requires root's assembled test run.
Prior default-feature logs do not verify those final two postimages.

## Remaining gates

The production fixed Parallel and scoped continuation gates remain false.
This packet does not enable dispatch or declare deployed durability.

Root must prove authenticated Main start, frozen participant identity, durable effect owner, and signed checkpoint claim scope together.
Root must verify immutable PostgreSQL parent comparison and hashed child lineage under writer fencing.
Root must verify fresh process reconstruction, completed child reuse, continuation receipt ownership, interruption, cancellation, and exact output ordering.
Root must verify inherited event attribution and receipt consistency through Worker, Supervisor, Main, and browser read-back.

Fixed Parallel V1 refuses nested explicit Parallel inside an owned Agent.
Its child checkpointer may remain erased only under that explicit refusal.
Future Map and nested Parallel require separate admitted catalogs and capability retention.
They receive no authority from this packet.
