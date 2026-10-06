# Code source and recovery acceptance

Date: 2026-10-04. Scope: authorized saved pipeline 144, test versions 151–155, Python, and Docker.
It does not declare Code or Point 5 complete.

## Source and ownership mapping

| Source owner | Contract | Observed evidence |
| --- | --- | --- |
| Main `internal/application/agentexecution/start.go` | Authenticated admission freezes the saved version and selects `agent.execute.application.v1`. | All five scoped executions retain that capability and numeric generation 1. |
| Worker `src/agents/graph/code.rs` | Declared variable and bounded template resolvers produce exact bytes with immutable provenance. | Variable, template, and fixed nodes report `StateVariable`, `StateTemplate`, and `SavedLiteral`. |
| Worker `src/agents/graph/code_runtime.rs` | Thread, node, step, and saved Code configuration digest bind each activation. | Captured lifecycle identities match reconstructed activations for all three nodes. |
| Worker `src/agents/graph/code_remote.rs` | Saved-pipeline authority admits declared resolvers through existing signed request and sandbox fences. | Main-authorized preparation and execution fingerprints match the actual resolved requests. |
| Worker `src/sandbox/request.rs` | Request fingerprints bind source, selected input, language, image, policy, timeout, and dependency identity. | Python preparation uses revision 1; bundle-backed execution uses revision 2. |
| Worker `src/sandbox/ledger.rs` | Original job keys and runtime identities survive replacement ownership. | The original variable and template jobs retain their request, runtime, and bundle identities. |
| Worker `src/state/postgres_checkpointer.rs` | Claim, generation, and lease fences control graph checkpoint writes. | Scoped readback selects the original writer execution, graph definition, capability, and thread. |
| Main `migrations/agentstate/0007_sandbox_dispatch_journal.sql` | Dispatch metadata binds the exact execution, generation, activation, request, and audience. | Recovery has six unique resolved dispatch records and six unique sandbox job keys. |
| Worker `src/agents/graph/code_trace.rs` | Lifecycle metadata identifies each preparation, hydration, and execution phase. | Recovery has nine completed phases and three completed Code executions. |

The [authority mapping](code-dynamic-source-authority-20261004.md) separates trusted definitions from untrusted state.
Provenance supplies no grant, credential, runtime capability, or network authority.
The [source mapping](code-source-mapping-20261004.md) defines template syntax and byte limits.

## Observed saved versions

| Version | Trigger | Observed result |
| --- | --- | --- |
| 151 | Variable source, named template fields, escaped braces, and fixed verification source. | Exact expected result appears once. Reload and history restore retain the same result. |
| 152 | Add the exact template prefix `# Multiline source acceptance.\n\n`. | Save As preserves version 151. Reload retains the exact edit. Persistent chat returns one exact result. |
| 153 | Resolve an empty string from the declared source variable. | Main settles as failed. Initial typed state remains unchanged. No Code trace, dispatch, or job exists under 18 tested keys. |
| 154 | Expand the template `{source_text}` from an empty declared string. | Main settles as failed. Initial typed state remains unchanged. No Code trace, dispatch, or job exists under eight tested keys. |
| 155 | Dispatch the template with an eight-second sleep, then kill and restart the exact Worker. | Original job and runtime identities complete successfully. Reload retains one prompt and one result without another Send. |

Both negatives show the safe Code-node error and retain the initial checkpoint.
Readback checks the actual empty source value, its string type, and unchanged typed inputs.

The positive result has `count: 9`, `label: "beta"`, and `typed_updates: true`.
The variable node receives only `seed`.
The template node receives only `variable_result`.
The fixed node verifies five selected fields and their types.
The terminal checkpoint preserves integer, string, and object state.

The final Web retains the exact two-line draft before the first Send, then returns one prompt and one answer.
The [focus mapping](pipeline-test-focus-draft-20261004.md) owns that UI change and its checks.

## Exact deployed identities

| Component | Container identity | Image identity |
| --- | --- | --- |
| Worker | `568ea22550858dcbbecfd3b03ab6d4116094f7b4df647582e842456fccae8d54` | `sha256:1d5328a53329cb6eb7cb5a779bf534e8009b33f05e8ac3467b72bf091e386c81` |
| Supervisor | `2084ebe595b753379f39a1f0c173c4f7273eda41ad30a4451414587781ba9292` | `sha256:85a91b8c375a9f21f11b645fa8b0fd0c28cb7a9f6e13d976c72df63ab0777389` |
| Final Web | `a67dece3f4b03a43a3b190761b2c2252f197ee76a527e5e24507195d27b2bc55` | `sha256:b2856228b35af651ead95520169e5b10af0b5cae398a8bf4fffa80ba26f0e6f8` |

Recovery retains the Worker container and image but replaces its process and start time.
The Supervisor retains the original sandbox runtime.

The actual dispatch audience hash selects one public Python profile.
Exact request digests verify its image, policy, timeout, and dependency bundle.
The observed runtime image is `sha256:18eb7de8f6181dc0c8b493a460e8f161e61f506a89446975bc2e930a85dac1f6`.
The configured policy revision is `deno-v1`; the configured timeout is 60 seconds.

## Recovery and checkpoint proof

Version 155 uses execution `a9a19ca888efa52e0f526dacb2b69f6c` and response `973754b6-ab68-52c5-a5b0-5ab586c63a65`.
Numeric execution generation remains 1; the browser execution-generation UUID is a separate identity.

Before the kill, the variable job completes and the template job remains dispatched with its original runtime and no result.
Checkpoint step 1 retains the pending template node.
The monitor verifies the exact Worker and excludes competing live ownership.
Its frozen legacy exception permits only exact unchanged rows with expired ownership.

Reconstruction verifies source, input, configuration, activation, request, profile, and bundle identities.
Supervisor admission supplies the existing signed-grant checks.
Terminal proof preserves the original job and runtime identities despite replacement lease ownership.
The result has three successful execution jobs, three preparation receipts, six resolved dispatch records, and four checkpoints.
The final checkpoint has graph step 3, save ordinal 4, and no pending nodes.
Its definition digest is `0c4ce0e9d4c6d013720ddea0cd0eb8b28676a55c6159f79089feaa276c4996e0`.

Bounded readback binds tenant, both projects, capability, thread, graph definition, and writer execution.
It returns comparison hashes without credentials, grants, audience addresses, runtime handles, raw logs, or unrelated user data.
Main owns migrations and saved definitions; the native Worker checkpointer owns fenced AgentState writes.

The change adds no source, selected input, or grant plaintext to dispatch journals or lifecycle identity metadata.
Existing saved definitions and authorized checkpoints retain configured source and state.
Sandbox receipts retain bounded output; this record makes no global plaintext-absence claim.

## Evidence and verification

Hashes identify preserved operator evidence.

| Receipt or capture | SHA-256 |
| --- | --- |
| Version 151 positive receipt | `f5e42a2b7bfb1fc2f99e735611002c38cef440fa7770c1f7285aa21ba1762985` |
| Version 152 positive receipt | `72103cba970e45525ad2adc47d4e5f84bfa9ddc4c080a84a1511e93d4c7cdf05` |
| Version 153 strengthened zero-effects receipt | `88fbc3baca5d8b4677bb457211f0d203e3e0e9607e84724c4ea4d603313d585f` |
| Version 154 zero-effects receipt | `0aad7a72a77f0c34371c3e75c89ddfb4d4a85f35842ff5f4da8fd4035a4a092c` |
| Version 155 root-reproduced recovery receipt | `7f32183d95033a3a666a67001b724387b68d39583b495331dc1f7f43fb424e87` |
| Version 155 final recovery manifest | `e382bc27554fd35e5612592278a7198766c94edd73d2d1832d498524e6f90613` |
| Recovery live DOM: one answer, one prompt, no displayed error | `f34ae4adf5c1e48a175c2182301203b9b2ad262c38b746b3cf83a2dac4a044d9` |
| Recovery reload DOM: one answer and one prompt | `de9cd5b17cbe777b2f2a1113441b3e87ebb435f6704547857d00b0a1e4b74c07` |

Historical `ROOT_BROWSER_ACCEPTANCE.json` has SHA-256 `20e5fdb1c5ea83e06d3d68f79c606e4fb862838eac5e15321d6643778cfa7739`.
Later recovery and first-focus evidence supersedes its version 155 `NOT_STARTED` and earlier draft failure.
The historical file remains unchanged; this record makes no console-clean claim.

The final exact-source suite passes 1,671 tests, fails none, and ignores 55 tests.
Strict Clippy passes with all targets, all features, and warnings denied.
Rust formatting checks pass for the adopted candidate files.
The adopted Worker input manifest matches all 643 tested source hashes.
The unchanged-source retry passes after restricted listener failures; both runs remain recorded.
The 55 ignored live or manual tests do not run in that suite.

Eighteen offline helper checks pass.
Seven changed-baseline checks refuse source, input, activation, job key, runtime, bundle, or thread substitution.
The frozen-helper replay and root repeat produce the same receipt hash with exit 0.
These checks remain separate from the Docker, database, and browser observations.

Kubernetes, native-cache, and higher graph acceptance gates remain open.
This Python Docker record does not prove every language, deployment, resource limit, or ordinary-chat Code authority contract.
