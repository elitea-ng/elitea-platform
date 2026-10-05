# Assembled Code execution acceptance

Point 5 remains open. This record separates assembled checks, explicit integrations, shipping images, and browser acceptance.

## Source mapping

| Owner | Verified boundary |
| --- | --- |
| `src/sandbox/docker_deadline_tests.rs` | PostgreSQL preserves phase deadlines, cancellation, and replacement ownership. |
| `src/sandbox/docker_hydration_deadline_tests.rs` | PostgreSQL preserves original hydration identity, retention, cleanup, and heartbeat expiry. |
| `src/sandbox/docker_preparation_tests.rs` | PostgreSQL preserves preparation publication, invalid-marker cleanup, and cancellation fences. |
| `src/state/postgres_checkpointer_tests.rs` | PostgreSQL preserves sandbox receipts and exact dispatch identity. |
| `src/state/postgres_checkpointer_tests/graph_receipts.rs` | Required PostgreSQL mode verifies immutable graph receipts and unchanged parent frontiers. |
| `../elitea-main/Containerfile` and `../../apps/elitea-web/Containerfile` | Shipping builds include the assembled Main and Web source. |
| Main editor lifecycle and Web editor Test controls | History and Restore retain the original successful Test conversation. |

## Assembled checks

The assembled Rust library run passes 1,535 tests, with zero failures and 55 ignored integrations.
The loopback fixture run requires local listener permission. Strict Clippy passes across all targets and features after two test-fixture corrections.
These results include startup and warm hydration, before the later outer static-scope packet.

The later outer static-scope assembly passes 1,539 library tests, with zero failures and the same 55 ignored integrations.
The optional scoped continuation admission remains disabled.

The reviewed materialization amendment passes 1,548 library tests on 2026-10-04, with zero failures and the same 55 ignored integrations.
Formatting, strict Clippy across all targets and features, and documentation with warnings denied also pass.
The [materialization mapping](scoped-materialization-entry-20261004.md) records actual descendant catalogs and validated transient receipts.
These checks do not enable scoped admission or prove deployed nested recovery.

The ignored tests retain their existing names and reasons. They require explicit fixtures or manual timing.
Their default-suite ignore count does not decrease when separate fixture runs pass.

Fresh disposable PostgreSQL 18 runs pass 35 PostgreSQL integrations and five PostgreSQL-plus-TLS integrations.
The TLS selection includes the actual heartbeat-expiry test. Both required graph-receipt tests also pass.
Each selection checks its exact expected count and zero failures. Generated credentials stay in process memory and environment variables.
All owned database containers and disposable TLS material are removed.

Six explicit Docker integrations pass. They cover process limits, workspace exhaustion, binary transfer, original-dispatch recovery, and authorized Rust submission.
The submission selection includes mTLS. Tests use immutable runner images and remove their owned fixtures.

The native mTLS transfer integration and Main shared-store conformance test also pass.
They verify scoped download, publication, authority rejection, and staging cleanup. The shared-store probe uses only the approved rehearsal credentials.
Credentials remain in memory and are removed from captured output. Test-scoped stored objects and the owned forwarding container are removed.

These separate selections run 47 of the 55 ignored tests successfully. Seven Kubernetes integrations and one manual timing measurement remain.
The two required graph-receipt tests and Main shared-store test are additional checks.

Main application, repository, storage, runtime composition, and command packages pass.
All three Helm charts pass direct lint. The Task wrapper is unavailable locally.
The compiled deployment selection passes 22 focused checks. The connection-budget render checks also pass.

## Shipping deployment and browser

Main and Web build from their shipping Containerfiles for Linux arm64.
Their immutable rehearsal images are `sha256:446faeaa726c2114cd4956997d8cd8c17f8e81fe4def11a537b6491fc76cab74` and `sha256:d49908329e9b82977aea03b432d01ad52d3f359059c49eed9a682e30c9510def`.
Main uses image user `65532`. Worker, Supervisor, database volumes, and private configuration remain unchanged during this rollout.
Configuration roundtrip, original service identities, and inactive claims are verified before replacement.

A new Playwright tab opens pipeline 143, saved version 150.
History shows the original successful execution `8c82a1ed8b1dfe4a2c4b50adae0ded92` and its terminal trace.
Restore shows one result with the expected four-language data digest.
The digest is `6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.
The result has 18,947 accepted rows, 1,053 rejected rows, and 824 refunds.

This check restores existing history. It does not run new code or repeat the earlier restart injections.

The browser still reports the existing `prompt_lib` index-type catalog HTTP 404.
This record does not claim a clean console or full UI parity.

The authorized Docker Desktop restart preserves the existing Main and Web images.
Main health and normal browser login pass after recovery.
Both runtime dispatch streams are absent because local Redis disables persistence.
The existing bootstrap recreates only the two fixed groups from `0-0` and exits successfully.
The Worker restart count then remains stable. This restoration does not prove recovery of deleted pending deliveries.

## Remaining acceptance

Optional compiled-cache startup remains off. The later Worker and Supervisor assembly still requires shipping deployment and browser checks.
Native cache publication, grants, cold/warm execution, and restart recovery need separate product acceptance.
Synthetic runner and kernel checks do not establish those product boundaries.
Native Kubernetes acceptance remains open. Full Kubernetes deployment acceptance follows the agreed later deployment work.
