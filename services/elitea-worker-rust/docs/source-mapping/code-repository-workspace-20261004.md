# Code repository workspace: private source candidate

Status: source authored. Tests are not run. The activation gate remains false.
This document does not prove compilation, deployment, or product acceptance.
Readwrite repository storage remains unavailable. This is an explicit product limit.

## Current business reference

The SDK GitHub toolkit reads files from a configured repository.
Its saved configuration supplies the API origin and credential reference.
Its client normalizes the saved repository name.
The SDK sources are `elitea_sdk/tools/github/api_wrapper.py` and `elitea_sdk/tools/github/github_client.py`.
These sources define business behavior. They do not authorize a sandbox Git clone.
The current Main toolkit catalog declares `github_configuration`, `repository`, and `selected_tools`.

## Source ownership

| Boundary | Owning source | Candidate behavior |
| --- | --- | --- |
| Saved declaration | `src/agents/graph/code.rs`, `code_workspace.rs`, `code_runtime.rs` | Preserve the owning YAML and exact configuration. Select workspace without requiring debug. |
| Original visit | Main original Code intent owner | Register the visit before acquisition. Revalidate the current claim and registered saved family. |
| Prepared request | `src/sandbox/request.rs`; Main `internal/infra/storage/code_prepared_request.go` | Append optional workspace. Preserve exact absent bytes and the broker capability. |
| Repository acquisition | Main `internal/infra/storage/code_workspace_service.go`, `code_workspace_github.go` | Resolve current saved toolkit authority. Read the exact repository and commit with bounded projections. |
| Immutable snapshot | Main `code_workspace.go`, `postgres_code_workspace.go` | Store immutable files. Bind one selected preparation base to one original visit. |
| Worker acquisition | `src/transport/code_workspace_content.rs`, `src/agents/graph/code_workspace_remote.rs` | Send the original visit and exact preparation base. Accept only the bound immutable manifest. |
| Supervisor content | Main `code_workspace_read_http.go`; Worker `workspace_content_transfer.rs` | Verify the signed role and current claim. Recheck authority after bounded object reads. |
| Indexed hydration | `service_workspace.rs`, `client_workspace.rs`, `protocol/sandbox_workspace_authority.rs`, `docker_workspace.rs` | Import bounded batches into the original job. Do not dispatch Code. |
| Docker mount | `vendor/adk-sandbox/src/workspace/docker_repository.rs`; `src/sandbox/runtime.rs` | Hydrate a named volume as UID10001. Mount that exact volume read-only for Code. |
| Kubernetes mount | `src/sandbox/kubernetes/workspace.rs`, `repository_content.rs`, `runtime.rs` | Hydrate the original Pod init volume. Mount it read-only for Code. |
| Image entry | Runner `src/workspace_content.rs`, `workspace_lifecycle.rs` | Verify exact content, file modes, completion, and a kernel read-only mount. |
| Durable cleanup | `src/sandbox/ledger_workspace.rs`, `docker_supervisor.rs` | Save provenance before container removal. Compare actual volume labels before cleanup. |

## Exact identities

The saved selection contains toolkit ID, toolkit reference digest, repository ID, commit, mode, and include paths.
No caller URL or host path enters the selection.
The toolkit digest binds saved metadata and reference-mode settings before credential redemption.
The manifest binds that selection, operator policy, ordered files, content digests, sizes, and executable flags.
Policy defaults are operator defaults. Hard limits still bound every consumer.
Typed bounds errors name the failed bound and its configured limit.

Prepared revision4 contains workspace without the broker.
Prepared revision5 contains the broker, with or without workspace.
Absent workspace and broker retain exact revisions1,2,3.
Old revisions reject smuggled optional fields.
`pre_workspace` removes only workspace and preserves broker, dependency, source, input, image, timeout, and policy.

The prepared fingerprint hashes the exact transport bytes with the existing domain framing.
The raw prepared SHA256 is a different identity.
The compiled binding retains its existing19 fields and hash domains.
Its `base_prepared_request_sha256` remains the framed prepared fingerprint.
Workspace changes therefore change the final compiled key without another hash domain.
Compile-time repository reads require the exact workspace manifest and read-only mount.
No workspace-bearing compiled request becomes eligible from a revision number alone.

## Admission and object reads

Main reserves metadata under a short original-visit transaction.
Main performs repository and object I/O outside that transaction.
Main commits Ready only after a fresh current-claim check.
The repository adapter supports the existing GitHub read capability.
It verifies numeric repository identity, immutable commit, tree identity, and each Git blob.
It refuses selected symlinks, submodules, unsafe paths, truncated trees, unsupported credentials, and unapproved API origins.
Credentials remain inside Main. Code keeps deny-network execution.

Supervisor content reads use actual verified TLS state.
Plain and cached Execute require the same signed original Code execution intent.
Compile requires its signed original-visit access. Compile cannot substitute an Execute intent.
The callback validates the immutable Ready receipt before object I/O.
A fresh callback validates the current fence before response bytes leave Main.
Nested access uses the sole registered saved-child scope and its `code_workspace` purpose.
It does not derive authority from a thread name or Worker checkpoint payload.

## Hydration mode matrix

| Mode | Job grant | Additional proof | Refused fields |
| --- | --- | --- | --- |
| Plain | Revision1 Execute | Exact signed original Code intent | Control, descriptor, Read grant |
| Compile | Revision4 Compile | Exact Control and signed original-visit access42 | Execute intent, descriptor, Read grant |
| Cached Execute | Revision4 Execute | Exact Control, selected descriptor, separate pinned Read grant, original Code intent | Compile access |

Hydration checks the current signed role before import.
Submission remains the owner of Code dispatch and final binding registration.
The new RPC does not execute user code.
It does not release or extend unrelated job authority.

## Cursor and original runtime

A batch contains at most32 files and4MiB.
The operator default per-file limit is1MiB. The hard per-file limit is4MiB.
The response cursor contains revision, manifest SHA32, next index, file count, and ready.
It contains no runtime selector or authority selector.
A future index or changed manifest fails.
An old index returns only confirmed progress from the same original helper.
IndexN with ready=false requires an explicit finalization request atN.
Ready=true requires full verification, helper exit, and Code launch preparation.
Empty manifests fail explicitly.

Stop, grant expiry, and the whole preparation deadline remain effective during transfer.
Uncertain loss does not create another original runtime.
The runner checks all confirmed files again before it acknowledges progress.
The runner rejects changed file modes, content, metadata, and unexpected files.
Docker retains the original named volume across process replacement.
Kubernetes binds its original Pod UID and refuses a restarted init helper as completion proof.

## Cleanup and writable limits

The existing sandbox job stores the workspace runtime receipt.
The receipt binds project, job, request, signed dispatch activation, original runtime, volume, owner token, creation token, and manifest.
Cleanup compares the receipt and actual runtime or volume provenance.
Missing provenance does not authorize deletion by name.
A crash after container removal leaves the immutable volume receipt available for the next cleanup owner.
The database fixture covers that metadata cutpoint. It does not prove a real Docker crash.
No mutable repository data enters shared source or compiled cache.
Readwrite needs durable quota-backed storage. This candidate refuses it.
Scratch outside the repository retains the existing bounded per-job behavior.

## Required integration and acceptance

1. Compose the exact original-visit, registered-family, broker, and debug owner inputs.
2. Allocate the two storage proposals through the owning Main migration workflow.
3. Generate the root-approved protocol source with pinned tooling.
4. Construct Main workspace services with the real toolkit reader, settings resolver, object store, receipts, and operator policy.
5. Wire the original-visit and sealed-read callbacks into Main runtime composition.
6. Compose the image entry verifier and measured compiled extension profile.
7. Compile the final source and run the authored focused fixtures.
8. Run required-mode PostgreSQL fixtures on the isolated guarded PostgreSQL18 instance.
9. Run actual Docker and Kubernetes read-only, Stop, takeover, helper-loss, and cleanup cutpoints.
10. Prove omitted legacy bytes, cold compilation, cached execution, and broker coexistence on final images.
11. Enable the gate only after root accepts the composed authority and runtime evidence.

The source gate is `CODE_REPOSITORY_WORKSPACE_READY=false`.
Constructor wiring, generation, compilation, and runtime acceptance are still open.
No row above proves those open requirements.
