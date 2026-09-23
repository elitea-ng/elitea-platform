# Long-term memory: separate gate 7b

Status: scoped for later implementation, 2026-09-17. This is not compaction acceptance.

Long-term memory retains selected facts and preferences across a user's conversations.
Compaction retains enough execution-local information to continue one task within its model window.
A compaction record must not automatically become a personal memory, a new instruction, or authority to execute a tool.
Suggested next steps in a compaction record are continuation hints, not new user requests.

## Functional source map and existing work

| Source | Evidence and future integration owner |
| --- | --- |
| Current SDK `elitea_sdk/tools/memory/__init__.py` | Existing `manage_memory`, `search_memory`, `get_memory`, and `delete_memory` behavior uses a namespaced LangGraph store. Use the behavior as a reference, not its persistence or exception implementation as a required port. |
| Current EliteaUI `src/[fsd]/features/settings/ui/memory/MemoryLongTermMemory.jsx` | The inspected component is a “Coming soon” placeholder. It is not live acceptance evidence. |
| Main `internal/infra/db/repos/memories.go`, `internal/api/v2/memories`, and `internal/application/agentexecution/memories.go` | Existing personal-memory storage, CRUD, and recall integration must be audited and reused where suitable. Do not introduce a second memory store or replace the current schema merely to adopt an SDK. |
| New UI `features/settings/ui/memory/LongTermMemoryManagement.tsx` and related hooks | Existing management controls need complete runtime, persistence, user-scope, and browser verification. Their presence does not close gate 7b. |
| Installed ADK-Rust 2.2.0 feature manifest | Declares `memory`, `memory-tools`, `database-memory`, `neo4j-memory`, `redis-memory`, and other memory adapters. The current worker does not enable these features. Audit actual APIs and backends before selecting an integration; the graph feature alone is not graph-memory support. |

## Closure requirements

- Define what is explicitly saved versus automatically proposed, with the user's control over read, update, disable, and deletion.
- Preserve user/project ownership and stable memory identities and revisions across retrieval, compaction, and worker replacement.
- Reuse appropriate ADK memory primitives and evaluate graph-backed retrieval against the existing store and product needs. A new database is not a default requirement.
- Bound retrieval and provenance; separate remembered facts from trusted project/skill instructions and the current user's request.
- Verify cross-conversation recall, corrections, conflicting or obsolete facts, removal, isolation, restart behavior, and the management UI.
- Document the final source mapping, any justified schema change, and deployed browser/runtime evidence.

Scheduling: after built-in runtime modules and before indexing, preserving indexing as the final gate.
No memory dependency, schema change, or runtime behavior is activated by this planning entry.
