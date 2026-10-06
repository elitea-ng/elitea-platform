# Static pipeline pause metadata v1

This contract uses the existing `NodeEventV1.response_metadata` JSON field. It adds no protobuf fields or generated bindings.

A paused root pipeline emits `pipeline_static_v1` on its `full_message`. The envelope retains the original response UUID, execution generation, and thread. The result remains nonterminal. It emits no HITL card and no `pipeline_finish`.

The strict object has these fields:

| Field | Required value |
| --- | --- |
| `revision` | Integer `1` |
| `pause_id` | `pipeline-static:sha256:` followed by 64 lowercase hex characters |
| `checkpoint_id` | Exact original root checkpoint ID |
| `kind` | `before` or `after` |
| `node_name` | Exact admitted leaf node ID |
| `definition_digest`, `node_digest` | Immutable definition and node SHA-256 labels |
| `pending_nodes` | Exact saved leaf frontier; ordered, unique, at most 128 nodes |
| `step` | Exact saved leaf step |
| `descendant_path` | Ordered `{node_name,thread_id,checkpoint_id}` entries, from root child to leaf |

A before frontier contains only the named node. An after frontier contains the saved successor set. Each descendant thread equals its parent thread plus `/` and its admitted node ID.

The worker computes `pause_id` from the original invocation, root checkpoint thread, root checkpoint ID, typed leaf metadata, and descendant checkpoint chain. It uses canonical Rust serialization for this identity. Main treats the ID as opaque. Main also binds the frozen root application and version through the exact original command entry and its content digest.

The browser sends the existing continuation endpoint with `execution_contract=agent.continue.static.v1`. Its body contains the project, conversation, response message, `static_pause_id`, and ordinary `user_input`. An optional thread must match the persisted thread. It cannot send checkpoint state, version bytes, frontier, step, HITL actions, credentials, or a checkpoint selector.

Main rechecks permissions, actor, project, response, thread, generation, original input digest, selected application version, and the full stored proof. It consumes the proof atomically with admission. Retry identity names the response and exact pause occurrence. The input keeps `should_continue=true` and `hitl_resume=false`.

The existing input `meta` carries `pipeline_static_resume_v1: {revision:1,pause_id}`. The worker requires that ID to match its latest persisted pause. It then validates the frozen catalog and all original checkpoint IDs before loading state.

A non-static completion removes old static proof. Unknown revisions, fields, mismatched frontiers, changed versions, stale occurrences, and mixed continuation kinds fail closed.

## Ordinary-parent saved tools

An ordinary agent uses a separate `pipeline_static_tools_v1` full-message metadata object. Revision 1 contains `pauses`, a bounded list of 1 through 16 entries:

| Field | Required value |
| --- | --- |
| `tool_call_id` | Original parent tool-call ID |
| `child_thread_id` | Exact saved tool root thread |
| `original_batch_event_id` | Original parent model-call event ID |
| `original_ordinal` | Original 1-based ordinal, from 1 through 16 |
| `proof` | The static proof described above, rooted at this child thread |

Each entry retains its original model batch. Pause identities and child/call pairs are unique. Each batch and ordinal pair is unique. Different batches may reuse ordinals or tool call IDs with distinct child threads. The inventory is at most 128 KiB. Descendant paths within each proof remain exact. Replayed container invocation IDs do not replace original pause or batch identity.

The same static continuation contract accepts `static_decisions`. Each strict entry contains `pause_id`, `child_thread_id`, `tool_call_id`, `action` equal to `continue`, and text `value`. There must be 1 through 16 entries. Each text value is at most 8 KiB. All values together are at most 64 KiB. This shape cannot include root `static_pause_id`, root `user_input`, or dynamic HITL decisions.

Main requires an exact stored inventory and original frozen ordinary-agent input. It matches the selected subset by pause, child, and tool call. It preserves the root application/version and command content digest. Admission consumes only the selected inventory entries, preserving untouched entries and their order.

The input `meta` carries `pipeline_static_tool_resume_v1: {revision:1,decisions:[...]}`. The worker resolver must compare each decision with its original durable event, frozen definition catalog, exact root and descendant checkpoints, frontier, and step. Its validated result carries the original model batch and ordinal into the owning coordinator.

The private consumer implements this validation and Main transport/storage admission. The family view adapter, original-event inventory producer, validated resume constructor, and partial-child coordinator remain required integrations. No ordinary-parent adoption is permitted until these paths pass their assembly and recovery tests. No protobuf fields or bindings change.
