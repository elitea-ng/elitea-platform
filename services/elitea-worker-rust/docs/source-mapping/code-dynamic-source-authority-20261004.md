# Dynamic Code source authority

Date: 2026-10-04. Status: source, offline checks, and the observed Python Docker acceptance pass.
Kubernetes, cache, and higher graph gates remain open.

## Selected execution contract

Saved-pipeline execution authority covers the declared Code source resolver.
The frozen pipeline defines a fixed source, a state variable, or a bounded template.
Selected state remains untrusted, including source text from a model or user.
Resolver provenance supplies no grant and selects no runtime capability.

This contract applies to graph Code nodes in an authorized saved pipeline.
An ordinary agent can call an authorized saved pipeline through its existing application-tool contract.
This decision does not authorize a future ordinary-chat Code tool or internal Code module.
Those tools require their own authority contract.

The pipeline start authorizes its declared behavior.
The worker resolves the source and selected input before it requests an execution grant.
Main signs their actual request fingerprint and the graph activation for each submission.
The supervisor verifies that grant before it admits runtime work.
No separate source-review confirmation is introduced on this path.

## Current source to target ownership

| Current source | Target owner | Observable contract |
| --- | --- | --- |
| SDK `elitea_sdk/runtime/langchain/langraph_agent.py:1413-1438` | `agents/pipeline.rs`, `agents/graph/compiler.rs` | Saved Code bypasses the sensitive-action guard and declares its source mapping. |
| SDK `elitea_sdk/runtime/langchain/utils.py:529-560` | `agents/graph/code.rs` | Fixed, variable, and bounded template resolvers produce exact source bytes. |
| Main `api/v2/agentexecution/route.go:73-93` | Existing authenticated start route | Project permission admission precedes saved-pipeline execution. |
| Main `application/agentexecution/start.go:217-245` | Existing version freezer and admission | Main resolves and freezes the saved version before durable execution admission. |
| Worker `agents/pipeline.rs::PipelineExecutionProfile::validate` | Existing graph admission | The worker compiles frozen YAML before it binds Code runtime authority. |
| Worker `agents/pipeline/composition.rs::CompositionAdmission::saved` | Existing child-pipeline admission | Child pipelines use authorized exact saved versions and definition digests. |
| Existing literal-only remote admission | `agents/graph/code_remote.rs::validate_invocation` | All declared resolver provenances use the same exact request authority. |
| Existing Cargo declaration admission | `agents/graph/code_remote.rs::validate_invocation` | Cargo declarations remain exclusive to Rust Code. |
| Main `transport/runtimegrpc/control/sandbox_grant.go` | Existing grant issuer | Main checks workload, live execution fence, desired state, command scope, capability, and configured supervisor audience. |
| Worker `sandbox/request.rs::PreparedJob::fingerprint` | Existing content identity | The fingerprint binds source, input, language, image, policy, timeout, and dependency metadata. |
| Worker `agents/graph/code_runtime.rs::activation` | Existing graph occurrence identity | Thread, node, step, and saved Code configuration digest distinguish Code visits. |
| Worker `protocol/sandbox_grant.rs::GrantVerifier` | Existing supervisor admission | Signature, request fingerprint, workload, audience, purpose, and expiry must match. |

The SDK reference permits general network access and platform-client operations.
This slice preserves Code mapping outcomes within the existing isolated execution profile.
It does not import those broader SDK authorities.
The separate client, artifact, workspace, and debug contracts retain their existing owners.

## Exact binding and failure behavior

Variable and template resolution validate declared state before runtime entry.
The Code mapping owner validates types, reserved keys, source size, and bounded expansion.
The resolved source travels as an exact borrowed string through `CodeInvocation`.
Graph-private invocation fields keep this construction inside the admitted Code-node boundary.
`PreparedJob` retains those bytes and normalizes only object ordering in selected JSON input.
Literal and dynamic resolvers with equal executable requests have equal content fingerprints.
Their immutable definitions and graph occurrences still determine separate activation identities.
The checkpoint definition digest separately binds the complete admitted graph.

Changed source, input, language, image, policy, timeout, or dependency content changes the signed request fingerprint.
An existing grant cannot authorize changed request bytes.
Changed tenant, project, execution, or activation cannot replace signed claim bytes without a valid new signature.
A valid fresh grant for another activation produces another durable job scope.
Reusing an old grant retains its original activation and never authorizes a new activation.

The worker requests a fresh Main grant during bounded reconciliation.
Replacement claims retain the original execution and graph activation.
Generation and authenticated worker can change after Main validates the replacement claim.
The job scope retains the original activation and request fingerprint.
The supervisor reconciles the recorded runtime and receipt; it does not create a replacement Code execution.

Invalid source or selected state fails before runtime entry.
Invalid, expired, forged, or mismatched grants fail before supervisor runtime admission.
The worker dispatch journal can retain an unresolved intent when Main rejects a submission.
That private intent is recovery metadata; it contains no source, selected input, credentials, or grant.
An authority rejection performs no Code execution and produces no graph result update.

The authority change adds no source, selected input, or grant plaintext to dispatch journals or lifecycle identity metadata.
Saved definitions and authorized graph checkpoints retain their existing configured source and state values.
Sandbox jobs retain request fingerprints, runtime identity, inert bundle metadata, and bounded result receipts.
These stores do not establish a global plaintext-absence claim.

## Isolation boundary

The runtime image, language profile, policy, and timeout come from deployment configuration.
Source and selected state cannot select those controls.
The prepared manifest contains only the fixed request and fixed launcher files.
The request exposes no host mount, general credential, user token, or platform-client handle.

Docker execution uses the existing `network=none` policy, capability removal, resource limits, and private temporary filesystems.
Its operator-selected dependency-preparation network is a separate trusted resolver role.
Kubernetes execution keeps its existing deny-egress deployment policy, private temporary volumes, non-root identity, and disabled service-account token mount.
These controls are unchanged by resolver admission.

## Focused fixtures

`agents/graph/code_remote_authority_tests.rs` supplies these checks:

- Exact source bytes and selected JSON input for all three resolver provenances.
- Cargo-language refusal for all three resolver provenances.
- Actual Code-node refusal before runtime entry for missing source, invalid source types, invalid selected input, and missing thread identity.
- Real Ed25519 grant refusal after source, input, language, image, policy, or timeout changes.
- Forged signature, substituted scope, wrong worker, wrong audience, stop-only purpose, and expired-grant refusal.
- Actual graph occurrence identities for recovery, another step, another thread, and changed saved configuration.
- Replacement-worker authority that retains the original job scope and rejects the original worker grant.

The grant fixtures use synthetic test keys and the production verifier.
The Code-node fixtures use a recording runtime in place of a supervisor.
Their execution counter is a fixture observation, not Docker or Kubernetes evidence.
No test requests a live service, deployment credential, database, or container runtime.

## Implementation history and verification

2026-10-04: The Code closure selects saved-pipeline execution authority for declared variable and template resolvers.
The private remote patch replaces blanket dynamic-source refusal with exhaustive provenance admission.
It retains dependency-language admission and every existing request, grant, journal, runtime, and recovery boundary.
No Main, protocol, generated binding, migration, or approval-UI changes are required.

Standalone Rust formatting passes for the private candidate files.
The combined Code focus passes 65 tests, including six authority tests and the mapping fixtures.
That focus precedes two equivalent mapping style corrections.
Strict Clippy passes on the final source with all targets, all features, and warnings denied.
The full final suite passes 1,671 tests and ignores 55 tests.
The library contributes 1,578 passes; binary and integration tests contribute 93 passes.
All checks use pinned Rust 1.97.1, locked offline dependencies, one build job, and disabled incremental builds.
The signed-grant fixtures run with the existing `sandbox-supervisor` feature.

The private harness uses symlinks to unowned current source and the exact frozen mapping candidate.
It compiles the adopted activation and Redis changes with this authority packet.
The final source manifest records 643 inputs and confirms no source drift during the final checks.
The manifest and logs retain exact candidate hashes and direct process exit codes.
No test modifies the shared source.

The first full run has nine failures because the restricted process cannot bind local fixture listeners.
An isolated OpenAPI test confirms `PermissionDenied` at the local listener.
Automatic approval permits the same bounded fixture suite outside that restriction.
The final suite then passes with unchanged source.
The 55 ignored tests retain their existing live-service or manual-probe requirements.
The Cargo run executes no ignored test or deployed acceptance check.
Separate root-controlled Docker and browser acceptance now verifies saved test versions 151–155.
The positive runs match actual source, selected input, provenance, profile, and bundle request fingerprints.
The negative runs retain initial typed state and produce no Code trace, dispatch, or candidate sandbox job.
The recovery run retains original job and runtime identities after the exact Worker process restarts.
The terminal checkpoint contains the expected typed state under the original execution scope.
Browser reload retains one prompt and one result without another Send.

The root-reproduced recovery receipt has SHA-256 `7f32183d95033a3a666a67001b724387b68d39583b495331dc1f7f43fb424e87`.
The [acceptance mapping](code-source-ui-recovery-acceptance-20261004.md) records deployment identities, source owners, receipt hashes, and evidence limits.
These observations do not declare Code or Point 5 complete.
