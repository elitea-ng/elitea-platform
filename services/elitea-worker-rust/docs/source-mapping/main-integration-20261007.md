# Main merge continuation, 2026-10-07

The preserved branch starts at `4c146ade5ab2204a9972dc3b6ca50c9ceeeccfc1`.
The incoming Main revision is `781bf6ece5e7ffee600f088f5101a9a4bbe4814b`.
The continuation verifies the existing merge before any source or deployment change.
All 501 staged blobs, five stash identities, and 148 frozen packet hashes match the final handoff.
The three current-platform revisions and four mapped source examples also match.
No saved graph snapshot is applied.

## Source and implementation history

Current application behavior remains the business reference.
The [NATS integration](main-nats-integration-20261006.md) records the earlier transport replacement and preserved Code behavior.
The [Point 5 mapping](point5-consolidation-20261006.md) records preserved graph ownership and pending acceptance.

| Boundary | Merge result | Owner |
| --- | --- | --- |
| Local NATS storage | Keep explicit config loading, the read-only config mount, named `/data` volumes, and `sync_interval: always`. | `deploy/docker-compose.yml`, `deploy/docker-compose.standalone-full.yml`, `deploy/runtime/nats-local.conf` |
| Incoming NATS image | Keep `nats:2.12.0`, its Python health sidecar, and healthy-sidecar dependencies. | The two resolved Compose files |
| RustFS readiness | Escape four shell references as `$$i`. Preserve the 30-attempt limit. | Standalone and E2E Compose files |
| Incoming Main changes | Keep generated API changes, Trixie images, native-client contracts, and DeepWiki changes. | Incoming Main source |
| Protected runtime work | Preserve Code, Supervisor receipts, graph checkpoints, Main authority, and Web controls. | Existing capability owners |
| NATS mapping drift | Record imported consumer access and typed poison disposition. Change no runtime policy. | [Command-delivery mapping](nats-command-delivery.md) |

Main and the admitted consumers use NATS; the removed Redis transport does not return.
PostgreSQL remains authoritative. Worker owns graph checkpoints. Supervisor owns jobs, leases, and durable receipts.
The command bus retains bounded references and generation, claim, lease, effect, and retirement fencing.

## Verification

The frozen deployment packet is `elitea-deploy-merge-checks-20261007`.
Helm lint, Worker and Supervisor renders, 274 NATS assertions, seven strict schema variants, and 16 positive Compose configurations pass.
Both corrected readiness probes stop after exactly 30 failures and permit a later successful readiness response.
These synthetic probes do not contact RustFS.

The frozen generation packet is `elitea-main-codegen-merge-checks-20261007`.
Pinned OpenAPI v2.7.2 and SQLC 1.31.1 reproduce all 39 generated outputs.
SQLC vet passes. No tracked generated output changes during verification.

The original Main packet is `elitea-main-merge-checks-20261007`.
Its local sandbox refuses six listener tests across three selections.
The final checkpoint summary names only two failures; the continuation retains and resolves all six.
The rerun packet is `elitea-merge-listeners-resume-20261007`.

| Rerun | Result | Explicit skips |
| --- | --- | --- |
| Full Main `internal/api` package | 1,082 passing test events; direct exit zero | 18 PostgreSQL tests |
| Retained Main Code/NATS startup selector | 30 passing test events; direct exit zero | One live-NATS test and one PostgreSQL test |
| Full native-client helper module | 28 passing test events; direct exit zero | None |

The reruns recover tests interrupted by the original panics.
Their counts include subtests and overlap earlier selections. Do not add them as unique coverage.
Live native-client conformance remains unexecuted; its tagged package only compiles.
The two retained focused Web checks pass under local Node 22.17.0; CI requires Node 26 or later.
Incoming prompt and fixture whitespace remains byte-identical to Main.
The full staged whitespace check reports those upstream paths; owned Compose and documentation changes pass separately.

## Runtime and release limits

Readback verifies eight candidate services and thirteen original services against their frozen identities, images, and restart counts.
Worker and Supervisor are absent from the isolated candidate at this checkpoint.
The historical application images use source `4c146ade5`; they do not prove merged Trixie image behavior.
The retained Supervisor image uses `45b152a92`; matching sandbox source does not prove matching Cargo dependencies.
Current image builds and scans remain required.

The candidate reaches ordinary OIDC identity selection through `/app/`.
Chrome blocks its callback with `ERR_BLOCKED_BY_CLIENT`; candidate authentication and Code execution remain unproved.
This observation does not establish an application defect.

Commit and push this reviewed merge through PR 1084.
Inspect the new CI results and each skip before claiming integrated CI acceptance.
Complete isolated NATS/Code execution, failure replay, preparation display, Stop, reload, and replacement acceptance next.
Keep graph gates 5a–5e and later worker gates open until their completion requirements pass.
Keep Workspaces in the post-worker backlog.
The user owns PR merges. Keep GitHub issues open for the testing workflow.
