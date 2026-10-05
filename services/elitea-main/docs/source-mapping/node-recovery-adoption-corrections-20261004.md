# Node recovery adoption corrections

This private amendment corrects two Main62 adoption blockers. It also prevents historical result fallback.
Apply this amendment after the original Main62 packet. Keep the original packet and its evidence unchanged.
The feature gates remain false. This packet changes no migration, dependency, protobuf field, or generated production source.

## Identifier ownership

`internal/runtimecomposition/composition.go:586` supplies `currentRuntimeID` to Agent admission.
`internal/runtimecomposition/index_runtime.go:316` encodes 16 random bytes with `hex.EncodeToString`.
Execution IDs therefore contain exactly 32 lowercase hexadecimal characters.
Zero bytes remain syntactically valid. Stored authorization and current claim checks provide authority.

Main62 incorrectly applies a response UUID grammar to execution IDs.
`internal/domain/noderecovery/contract.go:58` now accepts the actual execution grammar.
`ValidResponseMessageID` retains the existing separate nonzero, lowercase chat UUID grammar.
`internal/application/noderecovery/service.go` uses that response validator for public path selectors.
Request, State, private claim checks, and the HTTP owner consume the corrected execution validator.

All eight execution-bearing v1 JSON schemas use the corrected grammar with exact length bounds.
The execution-free receipt schema and canonical fixture remain byte-identical.
The public request retains its six existing fields. Public State and Accepted fields remain unchanged.
This pre-adoption v1 correction supersedes Main62 public pins. Main62 evidence does not prove production execution admission.

## Current visit ownership

`internal/infra/db/repos/node_recovery_current.go:18` selects the latest visit under the already locked execution job.
It includes every visit status and orders by `created_at DESC,journal_revision DESC`, matching the existing latest-visit convention.
`node_recovery_control.go` checks that identity before any historical ACK action lookup.
Both activation and expected revision must match, including consumed-action replay.

Previously, a delayed A ACK could return A restoration authority after a later B visit resumes.
The global RUNNING state and saved A ACK bytes do not prove A remains current.
The new guard rejects that sequence before historical action lookup and before any write.
Exact current-action replay under a replacement claim remains valid and performs no write.
Newer SUSPENDED, AUTHORIZED, RESUMED, STOPPED, and CANCELLED visits all supersede A.
A newer revision of the same activation also supersedes A.

`node_recovery_result.go:37` uses the same current visit selector.
Its eligible-action query binds exact current activation and revision.
It cannot skip a newer STOPPED visit and redeem an older RESUMED result.
Existing owner proof, immutable result hash, length, actor, current claim, and desired-state checks remain active.

## HTTP test ownership

The HTTP proof provider production source stays byte-identical to the HTTP owner's frozen packet.
The coordinated copied `http_action_recovery_test.go` changes its fixture execution ID to the production grammar.
It keeps the original frozen application and exact request bytes.
The owning HTTP contract derives the effect and committed receipt from that execution ID.
The original HTTP packet, provider tests, and shared identity fixture remain unchanged.

`node_recovery_http_owner_integration_test.go` exercises the actual provider through the Main operator transaction, result read, and ACK.
These checks use fake SQL and the actual frozen HTTP fixtures.
They do not execute an HTTP effect or contact PostgreSQL.

## Evidence and limits

The focused checks run one Go package at a time with locked dependencies, offline resolution, and one build job.
They exercise the actual Main ID generator, strict request decoding, State and operator service admission, API and private control boundaries.
They also exercise exact current-action replay, repeated-visit refusal, result fallback refusal, and the actual committed HTTP proof issuer.
The schema test checks all eight execution grammar definitions and the public production request fixture.
It is not a complete JSON Schema evaluator.

The packet pins tested sources, baseline bytes, logs, external inputs, public contracts, and original freezes.
Focused component tests do not prove PostgreSQL lock ordering, deployed claim replacement, UI recovery, or full runtime restoration.
No live service, database, browser, migration, shared source, or Cargo operation runs for this amendment.
Root owns final composition, migration heads, cross-language acceptance, and live verification.
