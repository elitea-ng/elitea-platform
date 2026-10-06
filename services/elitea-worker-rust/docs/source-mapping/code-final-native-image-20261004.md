# Code: final native Rust image acceptance

## Business contract and owners

The current SDK's sandbox node provides source, selected state, dependencies, and business output.
The replatform preserves these functions through isolated preparation and execution workloads.
The [Cargo mapping](code-cargo-retained-profile-20261002.md) describes dependency and language behavior.
The [combined recovery mapping](code-native-combined-recovery-20261004.md) records the earlier four-language restart proof.

| Contract | Owning implementation |
| --- | --- |
| Fixed source, selected state, and dependency declaration | Worker `src/agents/graph/code_preparation.rs` |
| Image, platform, policy, and preparation fingerprint | Worker `src/sandbox/preparation.rs` |
| Completed preparation and original runtime cleanup | Worker `src/sandbox/docker_preparation.rs` |
| Native metadata and complete object publication | Main `internal/infra/storage/sandbox_native_bundle.go` |
| Physical storage namespace and scoped content | Main `internal/infra/storage/ref.go` and `sandbox_bundle.go` |
| Typed state and durable graph frontier | Worker `src/state/postgres_checkpointer.rs` |
| Persisted chat output and execution settlement | Main execution repository and chat projection |
| Inert release measurement and profile production | `scripts/runtime/rust_compiled_release_manifest.py` |
| Additive AgentState history | Main `cmd/elitea-migrate` and `migrations/agentstate/0011_rust_compiled_snapshots.sql` |

## Actual browser and persisted execution

The isolated rehearsal uses final runner image `sha256:8c403798c060330cf9836a583ea5d803890b91387b0088f3e64f005c22b00024` for Rust preparation and execution.
Compiled caching remains disabled during this acceptance.
Other language profiles retain their prior image selections.

Pipeline 145, version 158, executes the fixed registry dependency fixture in persistent chat 801.
Its Rust program uses structs, traits, async processing, CSV data, and saved typed input.
The result contains three accepted records, a total of 30, and group totals of 19 and 11.
The browser displays one result and retains it after reload.
This approximately nine-second local sample is not a production performance claim.

The original execution is `6e1d8c3112e5c21d324cd271fa99424b`, generation 1.
The persisted response is `07dd6fb1-f879-5a14-b5b1-fb32773b7e2d`.
Independent read-back verifies the exact saved definition, published command, completed jobs, typed checkpoint, settlement, and single answer.
Preparation and execution each have one resolved original dispatch.
Execution exits with code 0, and both original sandbox runtimes are removed.

The first fixture version declares reserved state key `result` and fails compiler admission before preparation.
Version 158 changes that business field to `probe_result`.
Source, dependency declaration, selected input, and expected output remain unchanged.
The retained failed version does not demonstrate a runner failure.

## Published dependency bytes and measured release profile

The completed preparation binds native root `a3c3f10ffd8675db5afcf4b8869edf44474e0a137943c8accd7b692f1fc457a7`.
Root reads exactly three existing scoped objects through the authorized rehearsal store.
Stored metadata exactly matches the completed preparation receipt.
The Cargo record contains 116,535 bytes; the compressed archive contains 2,116,169 bytes.
Object hashes, canonical metadata, retained file hashes, archive bounds, image selections, and preparation fingerprint all verify.
This content read creates no runtime grant or attestation.

An inert, non-root, network-disabled final-image container hydrates those original bytes.
The owning audit verifies isolation, complete file inventory, wrapper, adapter, toolchain, and compiler flags.
The audit container runs no user code and is removed afterward.
The generated compiled profile remains `pending_native_cohort`.

Main's deployed migration binary contains the exact eleven owning AgentState SQL files.
The owning binary applies migration 11 against rehearsal AgentState only.
Recorded migrations 1–10 remain unchanged, and the new snapshot index has zero rows.
No DSN value appears in public receipts or command arguments.

| Public evidence | SHA-256 |
| --- | --- |
| Scoped execution, checkpoint, answer, and cleanup receipt | `5faa219405ee869fe2a1fd4cc564038866145e6c2f80d54256145f91ebd4ae51` |
| Reloaded browser read-back | `102e51ec2277c77fc00c243fe8edbb4bcc46fa8095abc715c5be78cc33dc574f` |
| Original object export receipt | `47671000c1f9b28b8b0ffebc91bab7741cb7cabd8a913ee98391d43e8561b0d4` |
| Actual image audit | `512f902837253f48f44db91e86e15a484e295415572ac77199309ca069e52d7f` |
| Pending compiled profile | `47e728341e7a19c13207882733b78a59c39fc7a5d2446c6308b90561be197d28` |
| Inert audit lifecycle and cleanup | `2e9864090399685b562f14e760718aa26085b3e9e6eb00472d3577f5ce08a022` |
| Owning AgentState migration receipt | `960e5d4a04c7bfceedd0be9cfb75495915ed7ed846a15cabf2e5af1227a378ea` |

## Remaining acceptance

The final image passes ordinary Rust dependency execution and persistent browser acceptance.
The following isolated rollout also verifies actual cold compilation and fresh warm execution.
Compiled publication recovery still requires actual product proof.
The editor Test cache selection passes the acceptance below.
Kubernetes final-image and compiled-cache acceptance remain open.
This fixture does not prove requirement-keyed dependency reuse, workspace authorization, platform-client access, or wider graph acceptance.
Point 5 remains open.

## Isolated compiled rollout and cold/warm acceptance

The isolated rehearsal selects the measured final-image profile through four new material volumes.
Main, Worker, and Supervisor restart with their original images, resources, networks, and unrelated settings.
All original volumes remain available for restoration.
Main receives the existing AgentState connection through a private file and uses a separate four-connection pool.
The existing preparation configuration and other language profiles remain unchanged.
Fourteen offline rollout checks and actual startup read-back pass.
This isolated enablement does not enable the general deployment default.

Persistent chats 803 and 804 execute the same saved pipeline version 158 and selected typed input.
Chat 803 starts with an empty scoped index and completes preparation, compilation, publication, and execution.
The compiler exports the executable without calling the user entrypoint.
Its original runtime cleanup is confirmed before the snapshot becomes ready.
Chat 804 selects that exact descriptor and starts a distinct execution sandbox.
The snapshot retains its original compiler job, request, export epoch, and receipt identity.

The cold execution is `e6fe3ece44de9da69aaba7934886461d`, generation 1.
The warm execution is `a3d50963d964dd15697ee17964470b59`, generation 1.
Both return the exact expected result and preserve the terminal typed checkpoint.
Each chat contains one prompt and one answer before and after reload.
All preparation, compiler, and execution runtime removals are independently verified.
The scoped snapshot index contains only one compilation job for the warm selection.

| Bound content | Identity |
| --- | --- |
| Snapshot key | `275b5c745574cb9f234ae672c06a516af07bc75a08ac6d624a73607e76ce7ed2` |
| Canonical descriptor root | `46ea57df2c9ae52981f957217f2042161acebeb9a8b135c1c24cd230160ebbcc` |
| Original compiler job | `87209286f1175e32520c55b0f153bc3026c83a161ec575282c4c0f68709ecfc5` |
| Executable bytes | 5,947,256 |

The cold job interval lasts approximately 73 seconds in this local sample.
The warm preparation and execution interval lasts approximately five seconds.
The browser reports four seconds for the warm response.
These measurements do not establish production capacity or a latency guarantee.
The cold publication interval requires a separate performance review.

Editor Test chat 805 runs the same version and returns the exact result in four browser-reported seconds.
Its execution is `d8b44575d93f2b641edddf525f8d4b28`, generation 1.
The read-only proof confirms the original compiled descriptor, no new compilation job, and a fresh execution sandbox.
Reload preserves its terminal History row and original trace.
Restore displays one original prompt and answer without starting another execution.
The restored screenshot confirms the result; existing console errors prevent a clean-console claim.

The first new chat selects the retained base version 157 and fails before any sandbox dispatch.
Selecting corrected version 158 resolves that fixture mistake without changing source, dependencies, or expected output.
The retained failure does not count as a cache execution.
The read-only proof initially expects two cold dispatches and correctly refuses the three-role record set.
The corrected proof checks separate Prepare, Compile, and Execute intents; no product assertion is removed.

| Public evidence | SHA-256 |
| --- | --- |
| Prepared isolated rollout | `6ad46f32bc85ba897741da11bb4791762a261a76a8527e39a7064b16d40857f7` |
| Applied configuration and startup read-back | `9a71695770c15469d3ba91bbadd70d7eb2b83ea026bd1e6d31c9cca7135164b0` |
| Actual cold execution and original release proof | `2caffdbd881e911523b91862d3835bc06808e355c9197188dc0a7d4c29412018` |
| Actual warm execution and original descriptor reuse | `640d48c6b7e6fab0dc4098355285e13b500b395ab930ea582e50cb23da04d719` |
| Actual editor Test compiled reuse | `0578ba025dc17adb90d3216a252e1e0079216baa04a6ca8763854e24872b8273` |
| Editor History reload and restore | `1152aab4fd0275c7fb5fe5c9689fbc1501c9703b67a8059b32876c5ad509f818` |

The browser screenshot confirms the selected pipeline, corrected version, and single visible warm result.
Reload retains one existing console error; this acceptance does not claim a clean console.

## Second cold run and refused restart test

Persistent chat 806 executes saved pipeline 145, version 159, through the same isolated compiled configuration.
Version 159 adds one Rust comment to force a distinct compiled snapshot key.
Dependencies, selected input, executable behavior, and expected output remain unchanged.
The original execution is `58c5f922dcd547cf50a959127e336dc8`, generation 1.
The persisted response is `a633bb46-4401-5ebb-b3ca-e0bdf97f17cb`.

Independent read-back verifies the original Prepare, Compile, and Execute records, typed checkpoint, settlement, content identities, and runtime cleanup.
The original compiler job is `a6ded3067c5ae21e85db4b7bba122958f55793a153e9d7886688a9a54b4a0ba2`.
The canonical descriptor root is `55007557891cc6e0f7b453d4777ed8e4f2dd9ee0016ea5458ffbf24468516034`.
The browser contains one prompt and one exact result before and after reload.
Existing console errors prevent a clean-console claim.

The intended publication restart test does not execute.
Its first observation fails before the Main stop attempt.
The second observation checks an incorrect derived compiler-container name and also refuses the stop.
Main continues to run, and the original execution completes normally.
Both failed test receipts remain available; this run provides no publication crash-recovery proof.

The compiler job interval lasts approximately 69 seconds in this second local sample.
This interval includes publication and ownership waits; it is not a compiler CPU measurement.
The publication performance cause remains unresolved.
General compiled admission remains disabled, and current Kubernetes acceptance remains open.

| Public evidence | SHA-256 |
| --- | --- |
| Second cold execution, original content, and cleanup | `ad988480f8f7ac6dd233a1f35be09d455be001fa6222281d190c1975b25a3472` |
| Recorded browser result, reload, and refused fault classification | `792e575d22fafcbec58ff176b353d4f04d29a49130c61199c1b220b12bdf8a84` |
| First refused restart observation | `68358825dc2fdb3f978271bdd290adc1b0a3d0c3cc0efc1f55f8adb969873511` |
| Second refused restart observation | `51bf881caa9c0e9c73c02841cebabad2d609a1ff29fc2895b6a5c992416c496c` |

## Third cold run and independent browser read-back

Persistent chat 807 executes pipeline 145, version 160, with a second distinct Rust comment.
The original execution is `ff9963b6d5404013f9f46b147242aeca`, generation 1.
The persisted response is `f36a594b-ebf0-510a-92c6-a531c0644e1a`.
The resolved source hash is `414aa28532c5b01f8c09d87a28afdc986773fd08e6e33395a7706607069551c8`.
The prepared request hash is `1f2e73b5e8c94a1d5c327dd7e3426e3ea9e877242d10895af53dd254dcdeac6b`.

Independent read-back verifies three original dispatches, the ready compiled descriptor, the typed checkpoint, settlement, and one exact answer.
All three original sandbox runtimes are removed.
The browser shows one prompt and one result before and after reload.
Existing console errors prevent a clean-console claim.

The restart test again refuses the Main stop.
Its admission and sandbox checks pass, but the original compiler-runtime check raises a validation error.
The failed receipt does not identify the first failed predicate.
The original run completes normally; it provides no publication crash-recovery proof.
The compiler job interval lasts approximately 68 seconds and includes publication and ownership waits.

The first terminal verifier retains the previous fixture's prepared request hash and correctly refuses the changed binding.
The corrected verifier freezes the exact hash from the original pre-stop observation.
It retains the previous verifier and all existing ownership, source, result, and cleanup checks.

| Public evidence | SHA-256 |
| --- | --- |
| Third cold execution, original content, and cleanup | `dca6a516a900eceb81b52539e49ab3f59a32033f1a0a70f86cad72fce32b58db` |
| Actual browser result and reload | `3d70d789d8ea5d1f6dbb417a73809c0090ad6b70ade5a4dae9d21181c1aa89b7` |
| Refused compiler-runtime fault check | `8618564914cd9be019a2f39e64d9ffb8757ef25a8ffa2ee4be574b9699c9caa4` |

## Large input fixture and later storage acceptance

The separate four-language fixture uses `scripts/runtime/fixtures/code-data-processing/input.json` with 20,000 rows and seed 42.
Its `python.py` creates deterministic CSV bytes in memory, then passes compressed content through selected pipeline state.
There is no persisted CSV input file in this fixture.
Gate 7 must repeat this processing with authorized artifact input and actual storage.
This acceptance does not prove that later artifact integration.
