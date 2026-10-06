# Node recovery pre-frontier claim inspection

This separate private amendment follows corrected Main62. It preserves the corrected public ID and JSON schema pins.
No source is adopted yet. Focused tests run only in the coordinated offline compiler window.
The existing feature gates remain false. This amendment changes no dependency, migration, protobuf field, or generated source.

## Main selection

`internal/infra/db/repos/claims.go` calls node recovery selection for the same live owner and fresh replacement claim.
`node_recovery_claim.go` loads the exact latest stored visit across every status.
It never finds an older eligible receipt by excluding a newer stored status.

Latest SUSPENDED or AUTHORIZED with desired SUSPENDED selects exact NODE_RECOVERY.
Latest STOPPED with desired RUNNING selects node terminal recovery.
Cancelled, reconciled, and inconsistent status/state combinations fail closed.
A missing node visit differs from a stored visit that cannot resume.

No node visit or latest RESUMED permits existing model checkpoint inspection only
for Agent RUNNING, desired RUNNING, and original invocation MAY_HAVE_STARTED.
The selected claim receives RECOVER_AGENT_MODEL_CHECKPOINT and the original immutable input manifest.
It carries no historical node receipt. It receives no fresh Begin or invocation authorization.
This selection covers the first pre-projection crash and the later A-resumed/B-pending cutpoint.
Main cannot distinguish those root frontiers from its prior A receipt.
Worker must inspect the exact root checkpoint and durable journal to distinguish them.

## Mode and lease fences

Mode changes bind claim ID, execution, generation, command, workload identity/session,
producer, claim attempt, lease epoch, fence token, live lease, deadline, and no terminal output.
The SQL rechecks these facts with the DB clock under the original job lock.
Existing actor authorization, frozen input binding, and checkpoint digest checks remain active.

Live CANCELLED and DRAINING claims retain their existing observation path without node mode changes.

The same live inspection claim may promote to NODE_RECOVERY after the exact latest
SUSPENDED or AUTHORIZED receipt is durably stored with desired SUSPENDED.
Private control and result access now require NODE_RECOVERY mode.
An inspection claim cannot replay A ACK or result into a later root frontier.
The corrected Main62 all-status ACK and result guards remain unchanged.

`internal/application/execution/claims.go` admits only an opted-in Agent inspection decision.
It accepts a positive remaining inspection lease bounded by the selected TTL.
Fresh ACCEPTED retains its exact selected TTL check.
No transition extends a lease, manufactures a timestamp, or resets the stored model checkpoint digest.

## Worker contract and limit

Worker sends node_recovery=true without agent_model_checkpoint_recovery.
The existing checkpoint inspection proves root execution, generation, immutable definition, and current frontier.
It then opens the same durable node journal with the new current writer.
Pending A after applied ACK uses the saved result or approved retry timestamp.
Pending B before Main projection reconciles its Started visit or republishes its durable failure receipt.

One model checkpoint digest remains bound to one claim.
A different digest on the same live claim fails existing authorization.
Worker closes the renewal actor without a command-bus ack or business dispatch.
Replacement waits for existing lease expiry and the command-bus redelivery.
This amendment does not reset the digest or invent immediate replacement authority.
The actual A/B fixture passes with MemoryCheckpointer and current-writer journal CAS.
It does not provide PostgreSQL persistence proof.
Its canonical A receipt has SHA256 `d8842fdc746183d1ac18e7aa798e968c93f08361cb61a8afe2c9f29760a81440`.
The full actual frontier evidence has SHA256 `5ffa8b5c228bf2cc04eb5ca1ff37e0eca27e8b1abbb48a9fbc7ab7f1206f6986`.

The fixture captures pending A at step 1 before ACK after journal revision 3.
A's stored receipt remains revision 2. The root checkpoint remains unchanged at that cutpoint.
The later root waits at B, step 2, before Main receives B's receipt.
It preserves a_result 41 and b_result -2, with dispatch counts A2/B1.
The fixture rejects A's receipt against B's exact journal. Explicit B approval completes B2 with result 43.
Main tests consume the exact emitted A receipt bytes and their hash.
Main never reads the Worker journal as authorization.
The packet separately pins the actual frontier evidence and its declared MemoryCheckpointer limitation.

## Component verification

Authored repository tests use actual ClaimService and ClaimsRepository with fake SQL.
They cover original and replacement claims, no visit, resumed A, current stored precedence,
newer noneligible receipt, denial, no fresh Begin/Invoke, and one-use checkpoint digest.
The transport test uses the real server and actual ClaimService with a node-only signed request.
It verifies frozen input, DB observation time, exact fence, receipt presence, and mixed-flag refusal.

These checks do not prove PostgreSQL syntax, concurrent lock behavior, or deployed checkpoint restoration.
No live database, service, browser, migration, shared source, or Cargo operation runs for this amendment.
Root owns the compiler window, final composition, and live acceptance.
