# Toolkit discovery result artifacts

This slice carries `toolkit.available_tools.v1` results through the private data plane.
The command and output event contain references. They do not contain discovery JSON.

## Current business sources

Read these sources under the umbrella workspace `projects/` directory.
The platform Python worker provides protocol evidence only.

| Current source | Verified responsibility | Native owner |
| --- | --- | --- |
| `projects/centry/pylon_main/plugins/elitea_core/api/v2/toolkit_available_tools.py:PromptLibAPI.get` | Authorize project and folder access. Load settings. Dispatch discovery. Return the full JSON result. | Main `application/toolkitdiscovery`, its repository, and toolkit HTTP handler |
| `projects/centry/pylon_indexer/plugins/indexer_worker/methods/indexer_toolkit_available_tools.py:Method.indexer_toolkit_available_tools` | Invoke SDK discovery once. Preserve the returned result shape. | Rust `src/execution/toolkit_delivery_processor.rs` |
| `projects/elitea-sdk/elitea_sdk/tools/__init__.py:get_toolkit_available_tools` | Select the toolkit enumerator. Return tools and argument schemas. Preserve result errors. | Rust toolkit registry and family implementations |

The inspected Core revision is `b701a00aeff0af1a416916c4a537bfdd4b7d8337`.
The inspected Indexer revision is `1f0fbcf5c3429c5ef08a3a3e7a4e81ee94b49460`.
The inspected SDK revision is `ecf49dfac73cd096da4c2297f3d91d13e526395a`.

## Contract and ownership

`libs/proto/elitea/runtime/v1/toolkit.proto` owns the artifact reference and result input binding.
The artifact media type is `application/vnd.elitea.toolkit-available-tools.v1+json`.
Its classification is `tenant-confidential`.

Rust publishes the exact JSON bytes through `InputContentClient::publish_toolkit_discovery`.
The client uses the existing private CA, worker certificate, HTTP/2 channel, and request deadline.
The request uses `PUT` at the admitted input URL with `/toolkit-discovery-result` appended.
The request carries the existing claim and fence headers, plus content length, media type, and SHA-256.
Main returns `ToolkitAvailableToolsArtifactReferenceV1` as `application/protobuf`.
Both directions remain outside Redis.

Main limits each upload to 1 MiB and uses the content listener concurrency limit.
Rust limits each returned reference to 4 KiB.
Main authorizes before reading the request body.
The storage statement repeats authorization while locking the durable claim, execution, and workload session.
The statement checks the lease and session expiry again after locking.
It derives the input bundle from the execution and requires `toolkit.available_tools.settings`.
The caller cannot select a tenant, project, bundle, or artifact identity.

`infra/db/repos/toolkit_discovery_artifacts.go` reuses the existing
`elitea_runtime.index_result_artifacts` ledger and `elitea_storage.transfer_grants`
upload inventory. Migration 0128 and its proposed table were removed. No new
artifact table or migration is required.

The ledger already owns artifact identity/version, execution/generation,
resource project, media type, length, digest, classification, and storage identity.
Its foreign key targets `execution_jobs`; no constraint restricts it to index
capabilities. The settings binding derives from the same immutable admitted
execution/input entries verified under the claim lock and again by output
acceptance. It is not copied into a second authority table.

## Upload, retry, and object lifecycle

The uploader authorizes before creating inventory. It returns an existing exact
attestation without uploading again and refuses different bytes. Otherwise it
records a server-owned transfer inventory row before writing to `ObjectStore`.
The row is already consumed: no presigned URL is issued, public upload-grant
redemption cannot use it, and the generic unconsumed-grant sweeper skips it.
The per-project reserved `index-artifacts` bucket is the existing result owner.

Object keys contain `toolkit-discovery/`, the execution/generation identity hash,
the content SHA-256, and a unique upload-attempt UUID. `ObjectStore.Put` does not
support conditional writes or immutable backend versions. The digest preserves
content addressing, while the attempt suffix prevents cleanup of a losing upload
from deleting another attempt's object. A successful retry returns the original
ledger reference rather than exposing its staging object. The ledger's
`storage_record_id` remains the existing transfer-grant UUID shape.

Main reads back and hashes the bounded stored bytes before attestation. A fresh
transaction locks the claim, execution, workload session, and upload inventory.
It checks lease/session expiry after acquiring authority locks and refuses an
expired inventory row. A subsequent statement sees a preceding writer's commit.
The execution lock serializes the first accepted artifact across versions; the
ledger primary key alone would not. Attestation and its immutable metadata commit
atomically. An uncertain commit is resolved by an exact retry.

A 30-second request deadline bounds upload and reads. Upload inventory expires
in 15 minutes. The existing discovery runtime loop also runs bounded orphan
collection (32 records per iteration). It locks expired inventory and requires
that no accepted ledger row references it. Object deletion happens before
inventory deletion in the same transaction. If byte deletion fails or the
process stops, the retained inventory permits another attempt. Idempotent object
deletion handles an uncertain cleanup commit. Accepted artifacts survive the
inventory TTL. Execution deletion removes the attestation through its existing
foreign key; the now-unreferenced object is reclaimed by the same collector.
No process-local object map owns production state.

Discovery composition requires an `ObjectStore` when enabled and passes the same
repository to the private content endpoint, settled result reader, and cleanup
loop. No object-store or database mutation was performed against the application
database during this replacement; verification creates isolated test databases.

## Reuse boundaries

The prior index artifact writer remains unwired. Its externally transferable
grant format lacks full claim/input binding and its consume/attest operations
are not atomic. This implementation reuses its storage owners, bucket, and UUID
locator convention, without exposing that writer to untrusted workers.

The generic objects table owns mutable user objects and their retention, not
immutable execution evidence. Input entries are sealed admission snapshots.
Output inbox rows are typed output frames with a 256 KiB database limit and
tighter transport limits, not the reference-only discovery result bytes. These
restrictions do not require a new table: existing ledger and object-store
infrastructure provide the artifact capability.

## Delivery and cancellation

`src/transport/toolkit_discovery_artifact.rs` owns the native upload and reference validation.
`src/execution/toolkit_delivery_processor.rs` keeps the upload under the existing lease monitor.
Dropping the upload future cancels its local wait. Main also checks the durable claim before writing.
A timeout can follow a successful commit. Retrying the same bytes preserves the immutable result.
Different discovery bytes after uncertain success produce a conflict and cannot replace the committed result.

Main output acceptance verifies the artifact and admitted input binding before terminal settlement.
The public result reader checks the project and execution before reading stored bytes.
These owners are `infra/db/repos/toolkit_available_tools_results.go` and `application/toolkitdiscovery/service.go`.

## Verification

| Test | Boundary and evidence |
| --- | --- |
| Rust `transport::input_content::toolkit_discovery_artifact::tests` | Component tests cover claim headers, encoded input paths, exact bytes, integrity, limits, timeout, and refusal statuses. |
| Go `TestToolkitDiscoveryUploadEnforcesAuthorityBoundsAndDigest` | HTTP component tests cover TLS, claim rejection, upload limits, digest, media type, conflict, and cancellation responses. |
| Go `TestToolkitDiscoveryArtifactAtomicReplayAndFencing` | The complete migration corpus runs in an isolated PostgreSQL database. Concurrent first uploads and exact retries preserve one result. |
| Same PostgreSQL test | Foreign execution, workload, generation, fence, claim, input, and version fail. Cancellation preserves committed bytes. Losing uploads and post-upload claim loss leave durable inventory; deletion failure retries; accepted artifacts survive expiry. |

The PostgreSQL test uses the local rehearsal database service and a deterministic object adapter.
The test creates and removes only its isolated database.
These tests do not prove browser behavior, workload replacement under load, or a deployed worker round trip.
Production transport remains subject to the coordinated discovery delivery verification.
