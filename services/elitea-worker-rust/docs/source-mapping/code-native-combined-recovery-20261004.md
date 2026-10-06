# Native Code: combined Docker recovery

## Business reference and source owners

The current SDK supplies sandbox source, selected state, and dependency controls through `elitea_sdk/runtime/tools/sandbox.py`.
The replatform retains those business functions through isolated preparation and execution workloads.
It adds durable recovery rather than copying the current sandbox transport.
The [JavaScript mapping](code-javascript-native-delivery-20261002.md) and [Cargo mapping](code-cargo-retained-profile-20261002.md) retain the language contracts.

| Contract | Replatform owner |
| --- | --- |
| Original activation, selected input, and prepared submission | `src/agents/graph/code_preparation.rs` |
| Exact source, platform, image, policy, and dependency fingerprint | `src/sandbox/preparation.rs` |
| Original preparer observation, lease renewal, and completion handoff | `src/sandbox/docker_preparation.rs::observe_preparer` |
| Durable graph frontier and current writer fencing | `src/state/postgres_checkpointer.rs` |
| Immutable prepared command and published digest | Main `internal/infra/db/repos/command_outbox.go` |
| Command redelivery through existing transport | Main `internal/application/execution/outbox_publisher.go` |

The Worker owns graph checkpoints. Main owns product execution claims, settlement, and persisted chat output.
The Supervisor observes the bound sandbox runtime. A process restart does not authorize a replacement activation.

## Current positive controls

Playwright submits `{"rows":20000,"seed":42}` to pipeline 143, version 150, in persistent chat 799.
Two positive controls complete before the fault test.
Both execute Python, JavaScript, TypeScript, and Rust with real dependency preparation and selected typed state.
Both retain one result per prompt after browser reload.
Their sixteen dispatches resolve, and their sixteen sandbox runtimes are absent after settlement.
These controls establish current deployment behavior; they do not establish recovery.

| Control execution | Receipt |
| --- | --- |
| `0b57a061b7b208a7f4f342728a3ae7b4` | Exact fixture output and cleanup |
| `3cbe281a8fde298faeba882f07e06c86` | Exact fixture output and cleanup |

The positive-control receipt SHA-256 is `d87ea8ab7eff3a9160d8b9c249f3fa3b80708c4ca66be03d3d1dd717abe4dbd0`.
Its `recovery_tested` field is false.

## Fault and recovery acceptance

The fault execution is `67cf7dc068deb74053f4b616ab20c966`, generation 1.
Its response is `95032c46-cb2f-5db7-8dcb-c15defef4cce`.
The controller captures the original JavaScript preparer before its bundle commits.
The original Python result and graph frontier already exist.
The original JavaScript runtime is bound and running; its bundle and terminal result are absent.

Root stops the Supervisor, Worker, and Main processes in that order, then restores all three.
The controller rechecks the original preparer after stopping the Supervisor.
The original services retain their container and image identities, with new process start times.
The platform renews its execution claim and resumes without another browser submission.

The terminal evidence proves all these conditions:

- Main settles the original execution successfully under claim attempt 2 and lease epoch 2.
- All eight original preparation and execution activations resolve, without extra dispatches.
- The original JavaScript preparer retains its job key, request digest, and runtime identity.
- Previously completed Python job identities and selected state remain unchanged.
- Every preparation stores durable bundle metadata and its matching completion record.
- Every execution digest matches the exact source, selected input, profile, image, platform, policy, and bundle root.
- Five checkpoints retain the original thread, definition, contiguous ordinals, and exact pending frontiers.
- The final checkpoint binds the recovered claim that commits settlement.
- The response contains one completed text item with the exact fixture result.
- All eight owned sandbox runtimes are absent; the stopped historical fixture remains unchanged.

The result contains 18,947 accepted records, 1,053 rejected records, and 824 refunds.
The total is 866,440,800 cents; the weighted total is 8,662,764,360,486.
The exact business result digest is `6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.
The open browser receives the result after recovery.
Reload retains three prompts and three distinct results, including the recovered result exactly once.
An independent repeat read verifies immutable job, request, bundle, checkpoint, prepared-command, and answer identities.

## Evidence and deployed scope

| Evidence | SHA-256 |
| --- | --- |
| Original inflight baseline | `72290cdde6b646e5b1227c9ef4548d6c1d9c05282b606f3b67c57e0e2eb33fdc` |
| Three-service control receipt | `3c5801bf68d593985b88d54c3806a0cbee70b601347d7331bb90f04f8a515095` |
| Root terminal recovery receipt | `5a541e8a473157edfc50200b2da0240265020de9dbbf9287e459a1840ceec6f4` |
| Root immutable repeat receipt | `c2719ee284fb4d65093010285b7d0bfad707e9ae137705c93bfd3aaf2733fdda` |
| Exact sandbox cleanup receipt | `a734e1f2b03831dac3696d7f02b06f734915ab7cba2bbe211b172fd9f3b9e142` |
| Live browser capture | `9b8e7e717e33f500b7827a4b811705efef430d05e04db4323927da3f49fad087` |
| Reloaded browser capture | `c36dae9c074269c7b1d8ab87cfdb9cccd7c8181bf3946df5c3e6cc5d939c8804` |

The deployed Main image is `sha256:446faeaa726c2114cd4956997d8cd8c17f8e81fe4def11a537b6491fc76cab74`.
The deployed Worker image is `sha256:1d5328a53329cb6eb7cb5a779bf534e8009b33f05e8ac3467b72bf091e386c81`.
The deployed Supervisor image is `sha256:85a91b8c375a9f21f11b645fa8b0fd0c28cb7a9f6e13d976c72df63ab0777389`.
The exact deployed language profiles have receipt digest `eb8ba06b271cf7f3378196e2e82ffb1e8b3b3cb3744115e03cc20c5f6b6aef61`.
Replacement runner images require their own acceptance.

## Verification limits and retained failures

Publication follows from completed preparation and hydration, matching bundle metadata, and signed execution requests bound to those roots.
This test does not independently inventory object-store bytes.
The prepared command predates Main restart by its stored timestamp and retains equal prepared and published digests.
The controller did not capture its digest before the fault; direct comparison starts with the terminal read.

Earlier controls refuse before stopping services when their checkpoint or deployed-profile assumptions do not match.
The terminal cleanup verifier initially rejects Docker's exact missing-container response format.
Its correction accepts only the owner-bound missing response; daemon failures and present runtimes still fail verification.
Twenty-seven offline checks pass before root executes the real read-only evidence queries.
These verifier refusals do not establish product failures or successful recovery.

This test proves combined service loss before native bundle commit, with the original preparer retained.
It does not prove preparer-container destruction, a dependency-provider outage, or every acquisition boundary.
It does not close final-image compiled-cache, Kubernetes, workspace, platform-client, debug-artifact, or wider graph gates.
Local processing metrics are samples, not production latency or capacity claims.
