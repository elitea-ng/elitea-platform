# Main node recovery control source map

## Scope

This private packet implements the Main side of Gate5b explicit operator recovery.
The owning requirement is `services/elitea-worker-rust/docs/remaining-gates.md:546`.
It preserves the exact execution and generation. It does not use ordinary HITL
continuation, which goes through `application/agentexecution/continue.go:595`
and new-turn admission (`application/agentexecution/admission.go:108`).
The original actor, immutable definition/input, node visit, failed attempt and
journal revision remain bound. Worker journals and graph restore are separate
owned inputs; component tests here are not PostgreSQL or runtime acceptance.

## Current-to-new mapping

All paths below are relative to `services/elitea-main/` except explicit libs paths.

| Existing owner | New owner and behavior |
| --- | --- |
| `internal/infra/db/repos/node_events.go:119` accepted frame/sequence/current fence transaction | Same transaction calls `node_recovery_projection.go:15` at node_events.go:358. It admits only typed nonterminal recovery, stores canonical receipt, changes desired state and original response metadata together; no settlement. |
| Existing project-local chat response/question/conversation binding | `node_recovery.go:78` resolves original job actor, project, generation and conversation from the stored response; client selectors grant nothing. |
| `internal/infra/legacyrbac/postgres.go` permission semantics | `legacyrbac/transaction.go:5` reuses those rules on the caller's owned transaction. `node_recovery.go:48` checks original actor permissions inside authorization/ACK transactions. |
| `internal/application/agentexecution/continue.go:595` terminal HITL new-turn continuation | Separate `application/noderecovery/service.go` and `api/v2/agentexecution/node_recovery.go:24`: GET current typed state and six-field explicit operator POST; never calls ordinary turn admission. |
| Ordinary execution command outbox, unique execution/generation | Separate `node_recovery_control_outbox` in shared/0135. Serializable `node_recovery.go:168` atomically stores exact request/action/audit and visit authorization. Replay compares exact typed request bytes. No new command, turn, effect or generation. |
| Original current claim/session lease authority | `node_recovery_control.go:39` locks exact certificate identity/claim/fence/session/job/outbox and repeats DB-clock lookup after locking; checks original actor permissions and immutable input manifest. |
| Existing claim replacement protocol | `claims.go:42` and `node_recovery_claim.go:16` permit SUSPENDED opt-in only with stored receipt; fresh fence/attempt/epoch, same execution/generation. Terminal settlement has precedence. Ordinary Begin/Invoke at claims.go:493/599 exclude NODE_RECOVERY. |
| Existing input manifest/content authorization | `input_bundles.go:70` allows suspended NODE_RECOVERY manifest inspection. `storage/postgres_content.go:61` permits only original agent.execution_request and marks inspection-only. `content_server.go` skips materialization/redemption only for that server-owned inspection marker. |
| Existing private mTLS content listener | `storage/node_recovery_control.go:104/126` strict bounded poll/ACK; `node_recovery_result.go:25` empty-body exact result redemption; no redirects/client receipt/credential input. Existing listener slots bound concurrency. |
| Existing worker progress/terminal distinction | `node_recovery_control.go:121/249` consumes exact journal-applied action: restore, no-effect evidence remaining SUSPENDED, or stopped-journal terminal-only settlement. Canonical stored ACK bytes fence replay conflicts. Cancellation and missing owner proofs fail closed. |
| Existing effect transport ownership | HTTP owner `http_action_recovery.go` external SHA3fe8a141f2ddc7336992a78d7a99fb6074fc6ad07e9dd1a1a0641ef4e66b013c reads immutable frozen request+completed exact v2 receipt under Main tx. Main result reader verifies stored action/proof/hash/length; no HTTP dispatch or artifact redemption. |
| Existing cancellation query/response projection | agent_cancel.sql + regenerated sqlc output accept RUNNING/SUSPENDED and clear recovery metadata. agent_cancel.go invokes node_recovery_cancel.go:6 inside original cancellation tx to audit/invalidate pending controls. |
| Existing Main composition and public route owner | runtimecomposition/composition.go:603 uses actual HTTP proof issuer; :1849 wires mTLS control/result store, :1945 exposes service. Production router :182 and command main.go:1765 mount same auth/project middleware. Main command compiles. |

## Cross-language source contracts

`libs/proto/contracts/node-recovery-v1.md` was authored before consumers.
Versioned strict JSON schemas own receipt, public Request/State/Accepted,
private Control/ACK/Resumption/TerminalSettlement and OwnerProof. Go structs are
closed typed contracts. Duplicates, unknown fields, invalid nulls, missing fields,
unsafe integers, controls/U+2028/U+2029 and wrong action/replay/class relations fail.
Canonical receipt fixture is 392 bytes without newline, SHA256
8f592df47f70a2b2f5c4bf0bd97832856cd194d139a0eeab7dc37a82ca4b1351.

common.proto adds SUSPENDED=4; control.proto adds RECOVER_NODE_VISIT=12,
node_recovery opt-in field17 and receipt bytes field16. protoc35.1 with pinned
protoc-gen-go1.36.11 reproduces both generated Go files byte for byte. No generated
file was hand-edited. agent_cancel SQL output was generated with existing sqlc1.31.1.
No product dependency or lockfile changes are included. go.offline.mod/sum are
verification-only locked copies plus local egresslib replace, outside the patch.

## Authority and effect rules

A fresh NODE_RECOVERY lease is inspection/control-only. Worker must revalidate
its exact stored graph checkpoint and node journal, CAS the approved action,
then seal the authenticated Main ACK. A restore grant binds current claim,
original input bundle/digest, activation, request, receipt digest and applied
revision; it grants one checkpoint restore, never ordinary Begin/Invoke.
Post-ACK current-fence RUNNING material/sandbox reads preserve frozen scope.
A terminal ACK grants only existing FAILED settlement, no node attempt or graph
restore. Terminal claims/output take precedence over any recovery receipt.

`verified_no_effect` only stores evidence/new receipt under SUSPENDED. It must
retain original failed history/attempt/node/thread/step/class/effect and require
another explicit Retry under unchanged policy/budget/backoff. A committed result
must be issued by its real owner, redeemed as exact immutable bytes and validated
through the original node output contract before Worker completion CAS.
Current HTTP issuer never turns absent/failed/dispatching/uncertain into success
or no-effect proof. Journal activation and HTTP effect activation are checked
separately. Browser body cannot provide proof, credentials, selectors or results.
Future Code/no-effect issuers must supply real evidence; missing issuer is denied.
The current HTTP proof issuer admits the original frozen HTTP node in the stored
Agent input. Nested saved-definition proof coverage requires the owning immutable
scope source; no root-definition fallback is inferred by this packet.

## Verification and limits

Final `verification/final-owned-pass-tests.jsonl`: 43 top-level tests and 98
subtests pass, direct exit0. All 10 targeted owning packages compile; 9 pass
package entries, application/noderecovery has no test files. Main command,
service/composition and application/execution have compile-only coverage in this
focused run. Existing cancellation/Begin/Invoke regressions run as well.
New tests cross actual Main route, accepted NodeEventsRepository projection,
operator transaction/outbox/audit, claim allocation and protobuf wire, private
mTLS listener, actual HTTP owner proof/result/ACK composition. SQL is exercised
with in-process transaction doubles; it is not PG integration.

Vet for all 10 packages exits0; changed source gofmt check passes. Proto
regeneration exact-byte proof is verification/proto-reproduction.json. Private
shared0135 head/checksum unit passes. The existing combined migration-head test
records baseline agentstate drift: expects10, existing source0011 yields11.
That failure is retained in final-owning-tests-rerun.jsonl; no unrelated agentstate
fix is made. Root must update final composed shared head after HTTP0140 and any
other migrations, and settle agentstate head separately. Migrations are not run.

No live DB, Docker, Kubernetes, browser, external effect, credential redemption,
Cargo, whole Go suite, race run, deployment, real restart/lease takeover or
independent-service recovery occurred. Root owns fresh hash-fenced adoption,
actual PG/CAS/concurrent authorization/migration/restart and runtime/UI gates.
The UI public contract was frozen separately, with no public POST changes from
private ACK extension. Main8, Main20 and actual Parallel wireproof remain frozen.

## Implementation history

1. Read the accepted frame, current permission, ordinary HITL and claim owners.
2. Establish typed receipt/request and new enum/claim contract; preserve legacy
   omission and ordinary HITL/terminal handling.
3. Implement projection, serializable operator outbox/audit, recovery-only claim,
   raw inspection and cancellation in private copies.
4. Add exact journal-applied restore/evidence/terminal ACK branches and replay
   bytes; keep ambiguous effects suspended and fresh Begin/Invoke blocked.
5. Wire externally owned actual committed HTTP proof issuer and exact receipt
   redemption. Exercise owning Main transaction through actual issuer/result/ACK.
6. Review terminal precedence and current readback digest; repair before final
   owning tests/vet/generated-byte check. Freeze private source and evidence.
