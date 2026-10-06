# Native JavaScript and TypeScript dependency delivery

## Source and ownership

This change extends the Python indexed preparation flow. It does not enable a
native profile in a deployment. It requires the Python delivery feature at
`ad4da99ff2dcf63b9c92c76e1fadb837c550153a`. The review artifact records the exact
worktree bytes used as its baseline.

The business reference is `projects/elitea-sdk/infra/data/sandbox/main.ts`
(`a54db410a46bac5e2c3cbc2db46c148a5c3d069c`, `install_imports`, lines 87–131;
Python installation before evaluation, lines 278–312). The new implementation
retains preparation before evaluation. Deno owns JavaScript parsing and package
resolution. It does not port the Python package parser to JavaScript.

The native acquisition component came from preserved work
`a0aceb1ce5d2dd19f90b2c29f7e9d962d81844b6`:
`services/elitea-code-runner/adapters/prepare_javascript_code.mjs` and its test.
The delivery patch adds a workspace-local verification scratch path to that
component. Its standalone format remains revision 1.

## Current source to new source

| Current behavior | New owner | Contract |
| --- | --- | --- |
| SDK import discovery before evaluation | `prepare_javascript_code.mjs` | Native Deno AST inspection. Reject dynamic, URL, local, and bare package imports. Never evaluate the supplied source during acquisition. |
| Python holding preparer and durable marker | `javascript_preparation_job.mjs`, `preparation.rs`, `docker_preparation.rs` | Preparation revision 2 binds source, language, platform, preparer image, execution image, policies, and timeout. Reuse verifies the original record. |
| Python bundle identity | `native_bundle.rs`, `native_bundle.mjs`, `sandbox_native_bundle.go` | Native revision 2 is additive. Python revision 1 bytes and hashes remain unchanged. |
| Python scoped content publication | `sandbox_native_bundle_http.go`, `dependency_content_transfer.rs` | The native route retains grant revision 3. Writes must match the preparation request digest. Reads require the signed scope and root. Metadata is published last. |
| Python indexed download and inert import | `docker_dependency_delivery.rs`, `runtime.rs`, Docker and Kubernetes content helpers | Transfer one flat digest alias at a time. The header carries kind and root. It has no destination path. Preserve original runtime identity and Pod UID. |
| Python imported metadata proof | `javascript_hydration_job.mjs`, `lifecycle.rs` | Hydrate into a private cache. Verify every byte and the frozen graph. Write a separate ready proof only after success. Metadata alone cannot permit dispatch. |
| Image lock and image cache execution | `execute.rs`, `javascript.mjs` | Native revision 3 selects the recorded workspace lock and cache. Use cached-only, frozen, no configuration, no node_modules, denied network, subprocess, and FFI. Verify readiness and bytes before evaluating source. |
| Existing stop and recovery | Graph journal, `docker_preparation.rs`, `docker_hydration.rs`, existing supervisor ledger | Retain activation, request digest, root, indexed transfer leases, persisted phase deadlines, and stop intent. Never reacquire packages after unknown delivery. |

## Native contract

`DependencyBundle` distinguishes `PythonV1` and `NativeV2`. `NativeKind`
distinguishes Deno and Cargo. `NativePlatform` accepts Linux, amd64 or arm64,
and GNU ABI. A native envelope binds these fields:

- revision, kind, language, and platform;
- preparation request SHA-256 and acquisition source SHA-256;
- execution image digest and execution policy revision;
- a language-owned payload and the envelope digest.

The native root hashes UTF-8 JSON with every object key sorted. It excludes
only the outer digest field. Native records must use those canonical bytes.
Python keeps its original ordered JSON hash. Preparation revision 2 keeps the
existing preparation fingerprint domain and typed field order. Execution
revision 3 includes `native_dependencies` in its existing request fingerprint.
No protobuf fields, signing domains, or grant revisions change.
The source proto comments describe the new JSON revisions.
Pinned generation completes on 2026-10-02.

Deno retains its ordered inner record and its path inventory. The transfer
index derives `<file-sha256>.blob` from each validated inventory entry. It
retains inventory order. The native store uses `native-v2/` below the signed
scope. The HTTP route is `/sandbox-native-bundles/{root}`. Its metadata name is
`elitea-native-bundle-v2.json`. The legacy route and keys remain unchanged.

The native envelope is bounded to 128 KiB. The Deno wrapper bounds its inner
record to 126 KiB. Deno permits 512 recorded files, 32 MiB per file, 128 MiB
in total, and fixed lock and import-module limits. It accepts the native npm
registry cache and the fixed JSR cache path pattern. It rejects unsafe path
segments. Nested inventory paths never enter the transfer header.

Cargo payload validation comes from the coordinated Cargo slice. It accepts
exactly two ordered flat objects: an 8 MiB record and a 128 MiB archive. Its
adapter owns nested archive validation, extraction, and offline compilation.
The coordinated declaration and graph bridge patch now enables Cargo acquisition in the shared source.
Synthetic Cargo conformance fixtures remain contract evidence.
The real registry proof below provides separate archive and compilation evidence.

## Docker and Kubernetes

Preparation uses `/workspace/native-dependencies/objects`. Inert execution
uses `/workspace/native-bundle/objects`. Hydration creates the private cache
under that execution directory. A fixed image helper creates
`elitea-native-ready-v2.json` with the exact envelope bytes after verification.
The supervisor compares that proof before durable dispatch.

The fixed native header contains revision, kind, root, name, and byte length.
Kubernetes also binds Pod UID and request digest. Both backends retain bounded
binary streaming, original instance checks, and exact byte counts. The image
helper checks its saved request or preparation marker before file access. It
refuses imports and finalization after dispatch. Native release creates only
the fixed native release marker. Completion still requires confirmed original
runtime termination.

A new native profile requires explicit `native_platform` and
`native_workspace_bytes`. Workspace capacity is 512 MiB through 4 GiB. Memory
must cover that workspace plus 128 MiB. Docker and Kubernetes use the same
capacity. Existing profiles retain a 256 MiB workspace. Deno acquisition is
bounded to 120 seconds. Cargo acquisition is bounded to 600 seconds and a
64 KiB declaration. Native execution admission requires the selected platform.
No existing deployment selects the new fields.

## Verification and remaining gates

The private offline Deno suite passed 16 tests. Two registry tests stayed
ignored. The tests cover AST inspection, source non-execution, immutable reuse,
request and envelope bindings, aliases, inert hydration, replay, traversal,
tampering, and a native fixture shared with Main and Rust.

Main's native parser, store, and direct authenticated handlers pass targeted Go
unit tests. The Python parser and store tests also pass. These tests use local
files, synthetic grants, and direct handlers. They do not contact services.
The runner passes 21 tests on macOS and non-root Linux. These include native
process-group timeout, owner drop, overlap rejection, and fixed helper admission.
Worker Rust tests cover native fixtures and typed request fingerprints. They
have not run. Worker and Cargo language builds remain outside this check.

Before a feature PR is enabled, run worker Rust type checks and its focused
suites. Run PostgreSQL indexed publication, stop, lease takeover, and unknown
acknowledgement tests. Run non-root Linux Docker and Kubernetes acquisition,
hydration, frozen offline execution, cancellation, replacement-worker recovery,
and original workload termination tests for JavaScript and TypeScript. Test
packages with optional platform dependencies and lifecycle scripts. Measure
maximum graph memory and time. The fixed 27-second finalization deadline and
30-second transfer deadline are unmeasured graph capacity gates. No deployed proof,
maximum-size support claim, or Point 5 closure follows from these local tests.

## Hydration deadline and descendant correction

The original native finalizer killed only the hydration parent after 27 seconds.
Each verification pass started a separate 25-second budget.
A late verifier could survive while the original container remained inert.

`native_finalization.rs` now owns a fresh process group and an exclusive retry lock.
The runner passes one phase deadline through staging and both Deno verification passes.
Timeout, SIGTERM, SIGINT, and owner drop kill the entire owned group.
Linux uses child-subreaper ownership and reaps adopted descendants before releasing the lock.
Hydration and verification scratch stay below the exclusive lock directory.
Cleanup removes that owned staging after descendant termination.
The group leader stays unreaped until cleanup, preventing PID reuse during group signalling.
Cleanup has a separate two-second termination bound.
Failed cleanup keeps the lock and blocks overlapping retries.
The safe syscall API uses the existing pinned rustix 1.1.5 package.
No package versions or Deno subprocess permissions change.

A disposable Linux probe uses the corrected finalizer and an injected trusted Deno helper.
The helper starts a real long-lived Deno descendant.
After the 27-second timeout and SIGTERM cancellation, all three process IDs are absent.
No ready proof exists, and the original inert PID 1 remains alive.
A subsequent retry succeeds on the same container and bundle root.
Linux unit tests also prove owner-drop cleanup before join and retry.
These checks prove finalizer ownership, not native package acquisition or full Kubernetes acceptance.
SIGKILL cannot run a cleanup handler; hard owner termination still requires original runtime termination.
The stale retry lock fails closed after such termination.
Pinned protobuf generation completes during shared integration on 2026-10-02.
The owning generator checks all toolchain versions before regeneration.
No protobuf field or signing domain changes in the native delta.

## Inherited runtime allocation limit

The native delivery paths inherit the Python hydration retention correction.
`ledger.rs::hydration_age_seconds` uses the immutable runtime binding time.
The legacy fallback uses creation time.
`docker_supervisor.rs` expires only Reserved jobs after 3690 seconds.
The indexed path checks that limit before, during, and after delivery.
The first dispatch checks it again.
The bounded owner sweep also finds abandoned bound jobs after lease expiry.
Cancellation takes precedence, and the authoritative phase check protects dispatched jobs.
The existing readiness and execution clocks remain separate.
The inherited 12 PostgreSQL fixtures select `DependencyBundle::PythonV1`.
Their assertions are unchanged.
See `code-python-hydration-retention-20261002.md` for the Python owner’s source map.
This inheritance adds no protocol fields or migrations.

## Shared integration verification, 2026-10-02

The combined native patch applies after the final Python correction.
The Cargo prerequisite, retained runtime, and graph bridge apply after that patch.
Existing equivalent duration formatting remains unchanged.

The worker all-target, all-feature locked check passes.
The first check finds an incorrect Kubernetes module reference.
The correction imports the sandbox digest validator through its explicit crate path.

Main runs 16 focused native and Python storage tests successfully.
These include verified local TLS transfer and native handler fixtures.
Main storage vet also passes.
The native upload regression checks that incorrect preparation authority returns HTTP 403 before storage writes.
A valid grant then succeeds with HTTP 204.
This correction removes misleading retry guidance without changing write authority.

The shared Deno suites pass 16 tests with network access denied.
The two real registry tests remain ignored in this check.
These results do not prove registry acquisition or deployed offline execution.

The Deno runtime, Rust runtime, and Main images build from recorded source snapshots.
Docker confirms their immutable local image identities and non-root users.
The supervisor and worker images also build from the final combined production source snapshot.
All five images use immutable local image identities and non-root users.
No native language profile is deployed by these image builds.

The deployment review finds two compatibility blockers.
Seven distinct profiles are needed to preserve existing execution and add all language preparers.
The existing four-profile limit cannot admit that configuration.
A native-capable Rust profile must also accept an ordinary request without native dependencies.
Any supplied native platform must still match the configured platform exactly.
The bounded profile and admission corrections now pass 15 focused tests.
The new limits permit eight profiles and 64 material files.
Per-file and total material byte limits remain unchanged.
Helm renders and chart lint pass.
See `code-supervisor-profile-admission-20261002.md` for ownership and resource boundaries.

The combined all-target, all-feature locked offline Clippy check passes.
Actual PostgreSQL checks pass 10 preparation, six phase-deadline, and 12 hydration-retention tests.
Each suite checks its expected test count.
The conservative legacy-clock fixture now checks allocation expiry before readiness.
The production clocks do not change during that fixture correction.

## Real registry runner verification, 2026-10-02

The immutable Deno runner passes real npm and JSR acquisition.
Async JavaScript uses `npm:is-odd@3.0.1`, including its transitive dependency.
Typed TypeScript uses `npm:is-number@7.0.0` and `jsr:@std/bytes@1.0.4/concat`.
Both return exact expected state and results.
Distinct non-root, network-disabled containers import and hydrate the retained content before dispatch.
The production launcher then executes the source with unchanged permission denial.

The lifecycle-script fixture retains `esbuild@0.25.10` without running its installation script.
Execution refuses environment access before any subprocess launch.
A missing npm version fails preparation without publishing a bundle.
Actual network, subprocess, and FFI attempts all fail with `NotCapable`.
Read-back confirms removal of all 10 owned containers.

The tested inventories contain seven through 39 files and at most 10.7 MiB.
Local preparation takes 0.400 through 1.695 seconds.
Local finalization takes 0.088 through 0.260 seconds.
Local execution takes 0.112 through 0.135 seconds.
These samples do not establish maximum graph capacity or universal latency.

This runner proof excludes live Main grants, object storage, worker journals, cancellation, takeover, Kubernetes scheduling, and browser acceptance.
Those deployment checks remain required.
These integration checks do not close Point 5.

## Native TLS deployment packaging, 2026-10-02

`deploy/scripts/gen-sandbox-certs.sh --native` extends the existing optional Python issuance path.
It adds four native server leaves and five separate content-client leaves under the existing runtime CA.
Docker and Kubernetes use the same exact DNS identities and separate certificate purposes.
The deployment guide records all nine additional leaf names.
The helper does not enable profiles or rotate existing identities.

A disposable CA probe verifies all 15 resulting certificate and key pairs.
Reruns preserve every existing file byte.
Wrong-purpose and wrong-DNS existing leaves fail without replacement.
An unknown flag also fails without changing material.
Shell syntax and repository whitespace checks pass.
The probe removes its test CA and keys.
This probe does not prove live listener identity or deployed content authority.


## Deployed marker boundary correction, 2026-10-02

The first native Docker browser request runs pipeline 143, version 150, in persistent chat 785.
Python preparation and execution complete. JavaScript preparation then returns `DataLoss` before content publication.
The original preparation container remains bound and reaches its original 120-second deadline.
The worker stops the pipeline. TypeScript and Rust do not run.

`javascript_preparation_job.mjs` writes the marker with `JSON.stringify(marker)`.
The nested bundle therefore keeps insertion order, while `NativeDependencyBundle::parse` requires sorted canonical bytes.
The earlier registry probe sorts the nested record before transfer. That probe masks this production receipt boundary.
All four retained probe records have matching hashes and bindings, but their raw embedded records fail canonical byte validation.
Cargo already canonicalizes its embedded bundle through `NativeMarker::serialize_bundle`.

The JavaScript writer now uses `canonical(marker)`.
The Supervisor parser, digest checks, duplicate-field rejection, publication order, and Python format remain unchanged.
A regression reads the emitted marker file and validates its exact embedded record bytes.
All eight focused native adapter tests pass on macOS and in a disposable non-root offline Linux container.
The Linux test uses one CPU, 512 MiB memory, and 128 processes.
A separate Linux adapter run emits exact request and marker fixtures for the Rust consumer regression.
The Rust regression passes those original bytes directly to `parse_marker`, without reserialization.
The focused Rust consumer regression passes with one test and zero ignored cases.
Duplicate revision, platform, payload, and file fields remain rejected.
Noncanonical embedded records remain rejected. The subsequent positive product
acceptance is recorded below; failure and recovery acceptance remains required.

The corrected rehearsal image changes only `/opt/elitea-code/javascript_preparation_job.mjs` over the verified existing Deno image.
Its immutable identity is `sha256:6a54c8b5b9a9d94a008f34f551d718ecde5e44c9033e860dc52d9bfa608b1bdd`.
The source Containerfile uses the corrected adapter on future full builds.
The image update does not prove registry acquisition, shared publication, cancellation, or recovery.
The failed original activation must retain its original runtime identity and terminal semantics.

## Subsequent browser acceptance

The corrected runner passes the four-language on-demand fixture in persistent
chat 785 and in pipeline 143's editor Test chat. Both produce the exact expected
20,000-record result. Persistent reload retains one successful output.
The two requests record 16 resolved preparation/execution dispatches and remove
all 16 original runtimes. The prior failure remains unchanged.
See [the browser acceptance record](code-native-browser-acceptance-20261002.md)
for execution identities, timings, and remaining backend and recovery gates.
The separate invalid-marker cleanup correction passes 17 actual PostgreSQL
preparation tests; its deployment and real backend termination proofs remain open.
