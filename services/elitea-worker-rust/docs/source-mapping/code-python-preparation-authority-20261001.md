# Python preparation authority foundation

Date: 2026-10-01.

This component defines a bounded Python preparation request and verifies its Main grant.
It does not connect a preparation runner, supervisor submission, or graph dispatch.
It does not prove deployed dependency installation or UI acceptance.

## Source mapping

The SDK source revision is `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`.
The source revision is verified against the current SDK checkout.

| Current source and behavior | New source | Result |
| --- | --- | --- |
| SDK `infra/data/sandbox/main.ts::install_imports` discovers missing imports and installs packages through micropip. | `services/elitea-worker-rust/src/sandbox/preparation.rs::PreparationJob` | The request admits literal Python source for separate dependency preparation. It does not admit execution state. |
| SDK `main.ts::find_imports_to_install` maps imports through the native Pyodide distribution map. | `services/elitea-code-runner/adapters/python_requirements.mjs` and `prepare_python_code.mjs` | Existing native preparation defines package discovery. This authority component adds no package resolver. |
| SDK environment preparation precedes source execution in the same process. | `PreparationJob::fingerprint` and `GrantVerifier::verify_preparation` | A separate fingerprint domain binds preparation authority. Execution authority cannot substitute for preparation authority. |
| SDK literal source retains Python escapes on the code path. | `PreparationJob::new` and `PreparationJob::from_transport` | JSON transport retains the admitted source string. The constructor does not evaluate source. |
| No SDK preparation activation or durable receipt contract is assumed. | `preparation_activation` | A separate hash domain derives stable preparation identity from the original graph Code activation. |
| Existing Main sandbox revision 1 grants bind the exact request digest and verified worker identity. | `services/elitea-worker-rust/src/protocol/sandbox_grant.rs::GrantVerifier` | Existing signature and claim validation also verify the preparation fingerprint. A separate authority type prevents execution admission. |
| Existing sandbox revision 2 grants authorize cancellation by immutable activation and request digest. | `GrantVerifier::verify_cancellation` | Preparation uses the same cancellation identity path. Worker generation replacement does not change the runtime identity. |
| Existing sandbox revision 3 grants authorize dependency content. | `GrantVerifier::verify_preparation` | Content grants and dependency content roots cannot authorize preparation. |

## Request and authority contract

The JSON request revision is 1. Python is the only language.
The request contains source, preparer image digest, policy revision, and timeout.
The image digest contains `sha256:` and 64 lowercase hexadecimal digits.
The request contains no input state, executable arguments, package list, or dependency content root.
Unknown fields, duplicate fields, incompatible revisions, and other languages fail validation.

The constructor reuses the execution request validation with empty input state.
Source has a 256 KiB limit. Source must contain non-whitespace content and no NUL character.
Policy revision has a 128-byte limit and uses the existing identifier rules.
Timeout is between 1 and 3,600 seconds. Transport has a 1 MiB limit.
These bounds do not configure the future preparation process or its network access.

The fingerprint covers the exact normalized request bytes and their length.
Its hash domain is `elitea.sandbox.preparation-job.v1` with a trailing NUL byte.
The graph activation derivative uses `elitea.graph.code.preparation-activation.v1` with a trailing NUL byte.
It covers the original 32-byte Code activation and its length.
The derivative does not include worker identity, grant expiry, or claim generation.

Main continues to sign its existing sandbox revision 1 claims.
The signature domain, claim schema, peer check, audience check, and 30-second maximum grant lifetime remain the same.
Preparation verification returns `AuthorizedPreparation`. Execution verification returns `AuthorizedJob`.
Each authority accepts only its corresponding typed request.
The preparation authority rechecks the fingerprint and expiry before admission.
No new grant revision, RPC, execution manifest, database migration, or dependency is introduced.

## Implementation history and limits

Existing code execution grants previously accepted only `PreparedJob` fingerprints.
The verifier now shares signature validation through a private verified authority record.
The public authority types keep preparation and execution admission separate.
Preparation authority rejects execution fingerprints, cancellation grants, and content grants.
Execution authority rejects preparation fingerprints, including requests with identical source, image, policy, and timeout.

The existing adapter preparation component freezes native resolved content before offline execution.
This foundation does not supervise that adapter or publish its output.
Current Docker execution containers exit and lose their temporary filesystem content.
A preparation runner must retain verified output through immutable publication before its container exits.
The future supervisor must own process limits, cancellation, publication, and durable preparation receipts.

Component checks do not prove Main-to-supervisor preparation transport or graph retry behavior.
Connected Docker and Kubernetes preparation, replacement recovery, downstream failure behavior, and UI acceptance remain open.
Keep the Code acceptance gate open until those checks pass.

## Verification

Focused tests cover strict request fields, constructor bounds, immutable fingerprints, stable activation, and authority separation.
Grant tests cover signed scope changes, verified peers, audience, expiry, content purpose, and cancellation identity after worker replacement.
Golden vectors record the preparation fingerprint and activation derivative.
Run the focused checks from `services/elitea-worker-rust`:

```bash
CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=line-tables-only CARGO_PROFILE_TEST_DEBUG=line-tables-only cargo test --lib --features sandbox-supervisor -j2 sandbox::preparation::tests
CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=line-tables-only CARGO_PROFILE_TEST_DEBUG=line-tables-only cargo test --lib --features sandbox-supervisor -j2 protocol::sandbox_grant::tests
CARGO_INCREMENTAL=0 CARGO_PROFILE_DEV_DEBUG=line-tables-only CARGO_PROFILE_TEST_DEBUG=line-tables-only cargo clippy --lib --tests --features sandbox-supervisor -j2 -- -D warnings
rustfmt --edition 2024 --check src/sandbox/preparation.rs src/protocol/sandbox_grant.rs
```

The preparation suite has five passing tests. The grant suite has nine passing tests.
The grant suite contains four new preparation tests and five existing execution and cancellation tests.
Clippy and assigned-file formatting checks pass.
No connected runner, Docker, Kubernetes, or browser acceptance check runs for this foundation.
