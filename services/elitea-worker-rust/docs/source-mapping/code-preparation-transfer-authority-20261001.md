# Preparation transfer authority contract

Date: 2026-10-01.

This contract adds bounded preparation control and content transfer control.
It defines typed content authority for the existing Main revision 3 grant.
The contract is delivered with the supervisor preparation and content publication lifecycle in this feature.

## Source mapping

The SDK behavior reference uses revision `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` prepares missing Python dependencies before execution. | `libs/proto/elitea/runtime/v1/sandbox.proto::PrepareSandboxDependencies` | The supervisor receives bounded literal source under preparation-specific authority. It receives no execution state or package file bytes. |
| SDK preparation and source execution share one process lifecycle. | `PrepareSandboxDependenciesResponseV1` and `PublishSandboxDependenciesResponseV1` | Preparation metadata remains pending until publication and confirmed preparer termination. |
| Existing native preparation freezes bundle metadata and file identities. | `PublishSandboxDependenciesRequestV1` | One recorded index selects one transfer. The final index publishes metadata before preparer release. |
| Existing Main revision 3 grants bind the content root and active worker scope. | `services/elitea-worker-rust/src/protocol/sandbox_grant.rs::GrantVerifier::verify_content` | The verifier returns typed scope, root, and expiry. It does not accept a caller-selected root. |
| Existing execution, preparation, and cancellation grants use revisions 1 and 2. | `AuthorizedContent` | Content authority cannot authorize those operations. Existing signed claim fields and signature bytes remain unchanged. |
| Existing content storage uses Main's private mTLS HTTP listener. | `PublishSandboxDependencies` | RPC control contains only the content grant and recorded index. Package file bytes remain outside RPC control. |
| Prepared execution binds the dependency content root in its request fingerprint. | `SubmitSandboxJobRequestV1::dependency_content_grant` | A separate revision 3 content grant authorizes the bound execution content. It cannot substitute for execution authority. |
| Indexed hydration verifies bundle metadata before source execution. | `SubmitSandboxJobRequestV1::dependency_bundle_json` | Final submission supplies the same bounded metadata. The supervisor compares it with the recorded inert runtime before dispatch. |
| Existing bundle downloads can exceed one content grant lifetime when the aggregate contains many files. | `HydrateSandboxDependencies` | Each ordered file uses fresh execution and content grants. Hydration preserves the same inert execution runtime across retries. |

## Wire contract

`PrepareSandboxDependenciesRequestV1` contains `grant = 1` and `preparation_job_json = 2`.
The JSON body has a 1 MiB limit and uses strict `PreparationJob` revision 1.
The request admits Python source, immutable preparer image digest, policy revision, and timeout.
It does not admit input state, executable arguments, package lists, or content roots.

The preparation response contains status, bundle metadata, failure code, and cleanup state.
The bundle metadata has a 128 KiB limit and contains no file bytes.
Ready metadata alone does not indicate completion.

`PublishSandboxDependenciesRequestV1` contains `content_grant = 1` and `index = 2`.
The signed content root and recorded preparation determine transfer identity.
The caller cannot select a file path, source, content root, tenant, or project outside the grant.
An index below the recorded file count selects exactly one recorded file.
An index equal to the file count publishes metadata last.
An index above the file count fails validation.
Retries retain the same index and require fresh authority.

Release the trusted preparer after metadata publication.
Confirm preparer termination before returning Completed.
Return Pending while publication or termination remains incomplete.
Failure codes and cleanup state retain the existing sandbox response fields.
The supervisor implementation must enforce these bounds and transitions.

`SubmitSandboxJobRequestV1` adds optional `dependency_content_grant = 16` and bytes `dependency_bundle_json = 17`.
The existing reserved range remains 3 through 15.
PreparedJob revision 2 requires this separate root-bound content grant for its exact execution activation and request.
Revision 2 also supplies the same verified bundle metadata used for indexed hydration.
The metadata has a 128 KiB limit and contains no file bytes.
The supervisor compares it with the immutable runtime record before dispatch.
PreparedJob revision 1 rejects content authority and requires empty bundle metadata.
Absent content authority and empty bundle metadata preserve the previous revision 1 submission wire bytes.
The submission grant remains the execution authority.

`HydrateSandboxDependenciesRequestV1` carries execution grant, content grant, prepared request JSON, bundle metadata JSON, and ordered file index.
The prepared request retains its 1 MiB bound. Verified bundle metadata retains its 128 KiB bound.
Each indexed transfer obtains fresh execution and content authority for the final Code activation and request digest.
The content root must match the verified bundle metadata.
Package bytes remain on Main's content data plane.
Hydration reserves the exact execution runtime before dispatching user source.
Indexed retries preserve that original inert runtime and its immutable imported bytes.
The file count selects final metadata import and verifies the complete content before readiness.

`HydrateSandboxDependenciesResponseV1::ready` has one boolean field.
A successful false response acknowledges that index and requires the next indexed transfer.
A true response indicates readiness or an execution that already crossed dispatch.
Submit reconciles that exact execution after a true response.
A busy lease returns Aborted. It must not return a false file acknowledgement.
The caller retains the same index after Aborted or a transport failure.

## Authority contract

Content verification shares the existing exact Ed25519 signature and strict protobuf validation.
The existing signature domain and 4,096-byte claim limit remain unchanged.
The verifier requires revision 3, non-cancellation purpose, and an exact 32-byte content root.
It checks tenant, project, execution, activation, generation, peer, audience, issued time, and expiry.
The maximum grant lifetime remains 30 seconds.

`AuthorizedContent` exposes only scope, root, and expiry validity.
The content root comes from verified signed claims.
The supervisor must match scope and root against its durable preparation record before transfer.
Signature verification alone does not prove that this preparation produced the signed root.
Worker replacement retains the same runtime scope when execution, activation, and request digest remain unchanged.

Execution, preparation, and cancellation verification reject revision 3 grants.
Content verification rejects revision 1 and revision 2 grants.
Content verification also rejects cancellation flags, malformed roots, changed signatures, and incompatible claim fields.
No new grant revision, signature domain, or cryptographic primitive is introduced.

## Implementation history and limits

The frozen base supervisor schema contains only submission and cancellation.
The schema now adds preparation and publication RPCs without changing existing field numbers or methods.
The follow-up adds submission content authority at field 16 without changing the existing reserved field range.
The field 17 follow-up adds bounded bundle metadata to final submission for verification against the existing inert runtime.
The hydration follow-up avoids one aggregate download under a single 30-second content grant.
It adds ordered transfer control and reuses the existing execution receipt schema.
No new receipt table, field, or migration is introduced by the hydration protocol.
The normal pinned Go generation updates `sandbox.pb.go` and `sandbox_grpc.pb.go`.
The Rust build generates its bindings from the schema.
No generated Rust file is edited.

The Rust supervisor and its test service implement all three new RPC methods.
Unconfigured preparation or content delivery fails closed.
Go and Python bindings are regenerated together with the pinned owning script.
Rust bindings remain generated by the existing build script.

Component authority tests do not prove process cleanup, shared storage publication, replacement recovery, or deployed execution.
Docker, Kubernetes, and UI acceptance remain open until the complete supervisor path passes those checks.

## Verification status for the extracted feature (2026-10-02)

This source is extracted from preserved work and reconciled with the current phase-deadline branch.
No stash-era test count, image identity, browser result, or runtime timing is evidence for this assembled patch.
The extraction runs formatting, source invariants, pinned protocol generation, protocol checks, shell/JSON syntax, and patch applicability only.
Rust builds, focused Rust tests, PostgreSQL tests, Docker and Kubernetes acceptance, and browser acceptance remain pending.
See [complete feature mapping and acceptance](code-python-delivery-feature-20261002.md) and [execution export correction](code-python-execution-export-20261002.md).
