# Main static pause and original recovery input consumers

## User outcome

Static pauses retain their public proof or original child inventory in the durable response.
Continuation uses that persisted identity and the original input digest.
NODE_RECOVERY claims can inspect the original agent input without redeeming credentials.
Inspection stays restricted after ACK changes desired state to RUNNING.

## Source mapping

| Owning source | Previous behavior | Composed behavior |
| --- | --- | --- |
| `internal/infra/db/repos/agent_execution_results.go` | Full-message decoding drops static proof and inventory. | Validates existing static contracts and retains their exact JSON bytes. |
| `internal/db/queries/agent_chat.sql`, `FinalizeCurrentAgentFullMessage` | Finalization neither stores nor clears static metadata. | Stores validated object metadata and removes stale proof and inventory on ordinary completion. |
| `internal/db/sqlcgen/agent_chat.sql.go` | Finalizer has no static JSON parameters. | Repository SQLC 1.31.1 generates the two parameters. No other generated file changes. |
| `internal/infra/db/repos/agent_execution_jobs.go` | Static continuation reaches the default HITL branch. | Uses existing guarded static root or tool continuation queries in the admission transaction. |
| `internal/infra/db/repos/runtime_sql.go` | The PostgreSQL adapter lacks static resolution methods. | Delegates both methods to existing generated queries. |
| `internal/infra/db/repos/input_bundles.go` | Suspended claims cannot resolve their original manifest. | Exact NODE_RECOVERY agent claims can resolve it while SUSPENDED or RUNNING. |
| `internal/infra/storage/postgres_content.go` | Suspended claims cannot read their original request entry. | Restricts NODE_RECOVERY reads to that entry and marks both states inspection-only. |

Paths in this table are relative to `services/elitea-main`.
The current source baseline is merged Main `e79c277bdcd12fd08fc5d487d80438c5a059c74d`.
The preserved delta reference is `b7f78424bc61786af1450321071edb412ad406e5`.
Only owning hunks were composed. The stash was not applied or restored as a whole.
Current Code fixes, claims, routes and migrations 145–152 remain unchanged.

## Contract boundaries

Existing application parsers validate root kind, frontier, descendant identity and bounded child inventories.
Malformed proof, contradictory root kind and mixed static authorities fail closed.
Nonempty HITL or authorization metadata also refuses a static terminal.
Null and empty other-pause fields remain compatible. Ordinary result decoding remains compatible.
Worker static events omit those other-pause keys; its event adapter removes a null root proof.
These producer paths are `events.rs` static inventory emission, final full-message emission and `event()`.

Continuation preserves actor, project, application/version, conversation, question, response, generation and thread.
It supplies the original input digest, exact proof or inventory, and selected original pause IDs.
Existing SQL owns response locking and compares the original binding before mutation.
Missing rows and unexpected response bindings refuse admission.
No query creates an alternative checkpoint owner or changes the original graph frontier.

Manifest and content reads retain the current lease and full authenticated claim selectors.
NODE_RECOVERY permits only application or adhoc agent inspection.
Content additionally requires the original `agent.execution_request` role and existing read audience.
The content server verifies stored length and digest before returning raw bytes.
Its existing `InspectionOnly` guard bypasses credential materialization in both desired states.
Ordinary non-node RUNNING content retains its existing materialization path.
Existing BeginExecution and AuthorizeInvocation gates remain unchanged.

## Verification

Repository SQLC generation and vet pass with the CI-pinned version 1.31.1.
Generation changes only `agent_chat.sql.go`; all generated files were hash-compared.
Focused Go tests pass: 73 top-level tests and 168 subtests, with no failures or skips.
Focused Go vet, owned-file formatting and patch whitespace checks pass.
The first run preserves one fixture failure: its certificate lacked a workload SAN.
The repository correctly refused that fixture. The corrected authenticated fixture passes.

The checks use the existing Go cache, offline module resolution, GOMAXPROCS=4 and package parallelism two.
The focused selector is `Static|NodeRecovery|PostgresContent|PersistCurrentAgentTerminal|LoadAndPersistCurrentAgent|DecodeCurrentAgent`.
It covers `internal/infra/db/repos`, `internal/infra/storage` and `internal/application/agentexecution`.
The test command is `go test -mod=readonly -p 2 -count=1 -json -run '<selector>' <packages>`.
Vet uses `go vet -mod=readonly -p 2 <packages>`.

New tests exercise finalizer arguments, exact continuation selectors, refusal, and authenticated inspection without materialization.
They also verify root compatibility and original child batch/ordinal preservation.
Query fixtures verify SQL selectors and adapter behavior; they do not execute PostgreSQL predicates or locks.
Real database continuation replay, stale-claim denial and after-ACK inspection require the owning integration fixture.
No database, Docker, deployment or browser check was run for this increment.
Source checks do not prove deployed pause or continuation behavior.
