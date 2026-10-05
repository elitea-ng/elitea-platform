# Code: combined source integration

## Business behavior and owners

The current SDK supplies selected state, dependencies, platform operations, and optional debug artifacts to its sandbox node.
The replatform separates these functions across Main, Worker, Supervisor, Runner, and Web.
This record describes combined source checks. It does not replace deployed acceptance records.
The [indexed hydration record](code-indexed-hydration-20261005.md) now verifies the refreshed Docker cohort and four-language chat execution.

| Current behavior reference | New implementation owner |
| --- | --- |
| SDK `elitea_sdk/runtime/tools/function.py::_prepare_pyodide_input` and `_handle_pyodide_output` | Worker `src/agents/graph/code_state.rs`, typed graph checkpoints, and Runner language adapters |
| SDK `infra/data/sandbox/main.ts::install_imports` | Worker dependency preparation, Supervisor native bundle delivery, and Runner offline hydration |
| SDK `elitea_sdk/runtime/clients/sandbox_client.py::SandboxClient` | Main `internal/application/codeplatform` and `internal/infra/storage/code_platform_*.go`; Runner `src/code_platform_*.rs` |
| SDK `elitea_sdk/runtime/tools/function.py::_save_code_to_artifact` | Main `internal/infra/storage/runtime_code_debug.go`, debug artifact repositories, and Worker `src/transport/code_debug.rs` |
| Existing chat output persistence and observation | Main `internal/infra/db/repos/replay_events.go` and Web generation-bound stream observation |
| Operator-controlled repository access | Main `internal/infra/storage/code_workspace_service.go` and Worker Docker/Kubernetes workspace adapters |

Platform calls retain actor, project, resource, and execution authority.
Code execution approval does not grant unrestricted platform access.
Main captures product authority. Worker owns graph checkpoints and execution-local state.

## Integration changes

Combined source includes 39 Runner paths, 12 Web paths, 148 Worker/Supervisor paths, and 148 Main paths.
Each source packet has verified baseline and resulting file hashes.
Unrelated graph work remains preserved in the worktree.

The repository generator produces the Go and Python protocol outputs from their exact protobuf inputs.
All 65 generated output hashes match the verified combined candidate.
The protobuf compatibility check passes against the branch HEAD.
Generated files receive no manual changes.

Main replay stops before an event whose terminal projection is not visible.
It does not advance the observer cursor past that event.
This prevents later visible events from hiding the pending result after reconnect.
This source correction does not establish the cause of the earlier Run C display failure.

The Web observer remains attached until the owning final message, validated pause, or failure.
Referenced output validates its byte count and digest before display.
Missing referenced output permits one bounded persisted-message read without another execution admission.
See the [final-result stream mapping](../../../../docs/source-mapping/code-final-result-live-stream-20261005.md).

## Migration checks

Shared migration history extends through 0141. AgentState history extends through 0012.
Existing applied migration bodies remain unchanged.
The new Code tables depend on preserved recovery, HTTP effect, and saved-child scope contracts.
Their schema presence does not enable HTTP execution admission.

A PostgreSQL probe applies shared migrations 0134 through 0141 in an isolated schema.
It applies AgentState migration 0012 in a separate isolated schema.
The probe copies base table definitions without data.
Rollback removes both isolated schemas, as confirmed through independent read-back.
This probe does not apply migrations to the rehearsal runtime schema.
It does not prove complete clean-database bootstrap.

## Verified checks

| Check | Result and boundary |
| --- | --- |
| Combined private Main suite | 11,892 passed, zero failed, and 1,697 skipped; the receipt retains every skip |
| Shared Main build and vet | Both direct exit statuses are zero |
| Browser application build | Passed with the existing npm lockfile; other Web surfaces are outside this check |
| Protocol generation and compatibility | Generation, hash comparison, and breaking checks pass |
| PostgreSQL migration probe | Isolated schema application and rollback pass; runtime migration deployment remains open |
| Rust Runner image | Built successfully with user `10001:10001`; no new runtime acceptance is claimed |

The Rust image identity is `sha256:355653a4eaced3bdd74a57eb5df9a4655a1e1117158dcdb2168cc5a6a574ecfd`.
The [Worker/Supervisor integration record](code-worker-supervisor-integration-20261005.md) retains its focused test and skip details.
Focused filters overlap. Their counts must not be added as independent test cases.

Additional integration checks pass: six Code deployment methods, four material installer methods, and 22 existing compiled deployment methods.
These checks include negative subcases and use rendered configuration or synthetic material.
All three charts pass direct Helm lint. Affected Worker, Kubernetes, capability, edge, and gateway render suites pass.
The Task CLI is unavailable. Direct checks do not claim a completed `task helm:lint` invocation.

Main's Containerfile now consumes BuildKit's target architecture instead of hardcoding `amd64`.
The original ARM image contains an x86 binary. The rebuilt image contains an independently inspected aarch64 binary.
This correction concerns Main image packaging. It adds no execution authority or graph behavior.
Worker and Web candidate image builds pass. Their exact deployment acceptance remains open.

The public receipt trust helper and packaging pass 24 focused tests with zero skips.
The local builder cannot resolve unpublished Runner repository references for the production wrapper.
Rehearsal builds use unchanged base source stages and the same public trust validator.
Their first non-root read-back fails because `COPY --chmod=0444` also makes the new parent directory non-traversable.
The corrected trust packaging preserves a `0755` parent and a root-owned `0444` asset.
Both rebuilt images pass independent read-back as user `10001` in disposable offline containers with read-only root filesystems.
Read-back confirms exact directory and file modes, owner identity, and the public asset digest.
These checks do not prove a live signed receipt exchange or production registry resolution.

## Remaining Code acceptance

1. Complete measured pure Rust compiled-profile image/catalog refresh and its workspace acceptance.
2. Preserve the passed persistent-chat and editor acceptance during the remaining fault and authorization checks.
3. Verify platform operation isolation, workspace access, debug exports, and failures through the UI.
4. Verify acquisition outages, preparer destruction, and later-run lookup of an existing dependency bundle.
5. Complete current-cohort Kubernetes cache, recovery, negative-case, and Stop checks.
6. Publish verified Code changes with source mappings and explicit CI skip accounting.

Operator-mounted folders, Docker volumes, and Kubernetes PVC paths are a separate extension after the current sandbox PR.
The [post-worker feature backlog](../wanted_feature.md#wf-01--code-workspaces) preserves this deferred scope as WF-01.
Existing workspace source implements immutable repository snapshots.

On 2026-10-05, the user defers all workspace integration until after completion and release of the full Rust worker.
This includes repository snapshots, trusted selector metadata, editor controls, operator folders, Docker volumes, and Kubernetes PVC paths.
Existing workspace source remains preserved for that extension.
The current Code-node delivery retains source, selected state, dependencies, and returned output, without requiring a workspace.

Main startup now supplies the optional workspace, platform, and debug dependencies through strict configuration.
Focused startup checks pass with 284 tests and subtests, zero skips, and passing build and vet.
The separate runtime material package passes sixteen tests and subtests after permitted loopback-listener access.
The debug renderer and Code Debug switch pass 178 focused tests, full TypeScript, and focused lint.
The final component smoke passes 25 tests. These filters overlap and must not be added as independent cases.
See the [debug UI mapping](../../../../apps/elitea-web/docs/source-mapping/code-debug-ui-integration-20261005.md).
The [deployment guide](../../../../deploy/runtime/code-consumers-deployment.md) documents Helm and Docker contracts and material isolation.
Worker workspace activation now relies on Main's signed admission instead of a temporary disabled constant.
The consumers remain default-disabled. Source checks do not prove deployed functionality.
The [broker startup mapping](code-platform-startup-20261005.md) records explicit Worker routing and both Supervisor backend selectors.
Twelve new startup tests pass. Focused regression filters overlap and must not be added as independent cases.
Broker Rust cache selection uses its exact route attestation. It cannot use the pure Rust cache profile.
The corrected Worker and Supervisor build as native ARM images with user `10001:10001`.
Their identities are `sha256:7770ad38a7e54b1367ec30401c6093dd0429fa61b04d49595bf7edc515a13e1f` and `sha256:566e6bd3a5cd1ed684617263e0c935524176b42cd1b39c5c1bd2bf493b9e864f`.
The Supervisor release compiler exceeds an 8-GiB builder limit. The unchanged source builds with a bounded 10-GiB allocation and one compilation CPU.
This build observation is not a runtime resource measurement.
The prepared Rust broker also needs its own content-client TLS identity.
Main `internal/infra/storage/sandbox_bundle_http.go::authorize` binds a content grant's audience to the authenticated certificate peer.
The new listener's audience cannot reuse the pure Rust content-client certificate.
Separate broker client material and private staging pass non-root read-back. The pure Rust profile stays byte-identical.
For eight 256-MiB staging limits, the rehearsal plan bounds Supervisor memory at 2560 MiB, including 512 MiB of process headroom.
These material and budget checks do not prove a content handshake, workload capacity, or deployed recovery.
Both rehearsal database backups restore into disposable, offline PostgreSQL storage.
The owning migration command advances the restored shared schema from 133 to 141 and AgentState from 11 to 12.
The successful probe uses a 1536-MiB data tmpfs, 2 GiB of memory, and half a CPU.
The earlier 512-MiB data limit is insufficient for both restored databases.
The probe removes its disposable container and does not change live data.
Deployment must preserve durable backlog under a verified producer and consumer fence.
An empty durable queue is not a migration prerequisite. Current leases and unfinished admission reservations still block migration.
Earlier Docker and Kubernetes proofs remain valid for their recorded images.
They do not close acceptance for this combined cohort.

## Deployed schema failure: 2026-10-05

The combined Docker cohort starts with shared schema 141 and AgentState schema 12.
Main health, four service image identities, and fenced backlog preservation pass read-back.
Editor Test version 163 fails after 210 seconds during Python preparation.
The original preparation container remains inert. No dispatch marker exists, and its owner row remains `reserved`.
PostgreSQL reports missing `code_recovery_binding_json` during admission and missing `code_recovery_receipt_json` during cleanup discovery.
`DockerSupervisor::provision_inert` calls `JobLedger::code_platform_launch_for_admission` before `mark_dispatched`.
That query requires the missing binding fields even when the preparation job has no platform capability.
The worker retries unavailable responses until the fixed preparation observation deadline expires.
The private storage proposal was not included in the owning Main migration corpus.
AgentState migration `0013_sandbox_whole_code_recovery.sql` adds the five nullable fields, phase constraints, and cleanup discovery index.
The new Main image must embed migration 0013 before the forward upgrade. Its startup validates the exact AgentState migration head.
Retained-row comparison must remove only the five added keys and separately verify that their values remain null.
The worker failure path also loses the safe Code phase and reason before the final UI error.
Error-path correction and fresh editor and persistent-chat acceptance remain open.
The owning migration passes 40 tests and subtests against disposable PostgreSQL, with zero failures and zero skips.
The checks cover clean bootstrap, schema 12 upgrade, unchanged retained fields, null additions, idempotent apply, and storage constraints.
The corrected Main image embeds AgentState head 13 and builds for ARM with user `65532`.
The rehearsal forward upgrade applies AgentState head 13 while shared head 141 stays unchanged.
A fresh full backup passes hash and archive catalog checks before the migration.
Before service restart, complete historical job and dispatch fields match their original hashes. All five added fields remain null.
Main runs the corrected image. Worker, Supervisor, and Web keep their exact previous images and containers.
All four services pass readiness. The repeated missing-schema cleanup errors stop after restart.
Docker mount enumeration order caused a deployment guard mismatch. The corrected guard proves an exact permutation of the complete original mount records.
The audit retains its previous entries and appends the reviewed operator transition.
Fresh editor testing reaches Python hydration and execution. Final four-language and persistent-chat acceptance remain unproved.

The [publication recovery record](code-publication-recovery-20261004.md) retains the successful Main restart and live UI result proof.
The [native recovery record](code-native-combined-recovery-20261004.md) retains the earlier combined service-loss proof.
Code acceptance and the broader Point 5 graph work remain open.

## Platform-call wait cycle and recovery pause

The next editor run reaches Python execution after the schema repair.
Runner waits for a platform reply, but Worker waits for Supervisor completion before it services that call.
The sandbox reaches its execution deadline after about 61 seconds.
Main persists the Python recovery visit and changes the execution desired state to `SUSPENDED`.
Execution state remains `RUNNING` because recovery suspension is nonterminal.
The UI omits the recovery notice and continues to show Stop.
This evidence identifies separate execution and display defects.

The [concurrent observation record](code-platform-concurrent-observation-20261005.md) describes the scoped correction.
The adopted preparation and schema diagnostics pass 34 combined native checks, with zero failures and zero skips.
These checks do not prove the new handoff or deployed recovery display.

## Delivery composition and preservation

The user confirms that delivery must preserve working shared foundations, including pieces of future features.
Workspace acceptance remains deferred in [WF-01](../wanted_feature.md#wf-01--code-workspaces); its shared types and parsers remain with the Code implementation.
No source is stripped or stashed to reduce this delivery.

The selected patch is checked against a private export of tracked HEAD, with explicit overlays.
This avoids treating successful compilation against unrelated untracked files as proof that the committed patch builds.
Required companions include the Runner's registered native/compiled modules, Worker node-attempt recovery and graph checkpoint owners, Main source identity and continuation contracts, migration inputs, generated protocols, and the Web's editor Test lifecycle.
These dependencies retain their owning source mappings. Their inclusion does not claim acceptance for every future graph feature.

The clean Main composition builds all production packages and compiles all 195 package/test targets.
Seventy-one focused top-level tests pass, with 235 tests and subtests and no skips.
The checks use installed Go 1.26.5; the declared Go version and pushed-head CI remain separate verification boundaries.
The clean Runner, Worker, and Supervisor pass locked offline all-target checks with one compiler job, including the separate default Worker configuration.
Those Rust checks compile test targets but do not execute them.

Docker/Helm packaging checks pass for the opt-in Code consumers, private material installer, compiled snapshots, and public Runner trust asset.
Worker and Kubernetes sandbox render guards pass. The editor lifecycle CI gate passes its 13 local tests.
Strict lint, generated-code consistency, the complete Web delivery selection, and the pushed-head CI results are recorded separately as they finish.
The latest [debug UI evidence](../../../../apps/elitea-web/docs/source-mapping/code-debug-ui-integration-20261005.md) covers fresh and restored persistent-chat snapshot controls and explicitly records the download-path inspection limitation.

### Final Main composition checks

The final Main selection contains all required source, fixture, migration, and generated protocol companions.
A complete service-free run of `go test -mod=readonly -p=1 -count=1 -timeout=90s -json ./...` passes: 173 packages with tests, 22 packages without tests, and zero failures.
Its parsed test and subtest outcomes are 12,017 passed and 1,692 skipped.
The exact skip ledger is retained with the validation receipt; these results do not claim PostgreSQL or other service-dependent integration coverage.
The private run uses Go 1.26.5 and `GOMAXPROCS=2`, while remote CI owns the declared toolchain check.

Main vet, pinned SQLC generation and vet, pinned API generation, configuration fixtures, static API contracts, and environment drift checks pass.
SQLC and API generation introduce no drift.
The adopted 33 Go formatting changes preserve formatter-equivalent source; four protobuf inputs receive formatting and import-order corrections.
Regenerated Go/Python protocol descriptors remain equivalent after dependency-order normalization.
Root read-back confirms protocol format, lint, and compatibility checks against the tracked branch baseline.
All required shared migrations 0134 through 0141 and AgentState migrations 0011 through 0013 remain in the selected source corpus.
Previously tracked migration bodies remain unchanged.

### Final Web composition checks

The first full Web node-suite run checks 1,590 files and exposes eight failing suites.
The delivery selection lacked existing renderer and authoring-guard companions for the graph types it already imported.
Those exact companions remain in the patch with the default-false authoring policy unchanged.
Six test-only patches align assertions with deliberate YAML field deletion and the native Node Blob returned by the test server; positive values, bindings, mappings, MIME type, size, and exact response bytes remain asserted.
All eight affected suites and their companion regressions now pass: 19 suites, 295 tests, zero failures.
Full TypeScript and selected lint pass.
The initial full-suite failure and final affected-suite success remain separate receipts; a single all-green full Web invocation is not claimed.
The frozen final 186-path Web selection also passes full lint with warnings denied and the production app build.
The build uses the existing lockfile and installed dependencies, takes 21.58 seconds locally, and retains its nonfatal Vite and chunk-size advisories.
These quality checks do not redeploy the final combined source or replace browser acceptance.

### Final Worker and Supervisor source checks

The frozen delivery passes locked, offline, all-target, all-feature tests: 12 targets, 1,990 Rust-reported passed, zero failures, and 63 ignored tests.
Eighteen of the reported passing tests return early because their database or process-test settings are absent; 1,972 other passing outcomes remain.
The exact ignored and early-return ledgers are retained with the private validation receipt.
Strict Clippy, formatting, and rustdoc with warnings denied also pass.
The checks use one compiler job and two test threads, without supplying database, provider, or secret settings.
PostgreSQL, Docker, Kubernetes, external-service, and release-profile coverage are separate gates.

The first full source run exposed an existing 2-MiB sensitive-HITL test thread stack overflow.
Static debug disassembly identified overlapping large async construction and poll frames, rather than recursive output spooling.
A private, non-inlined phase constructor moves the existing boxed future construction outside its caller's poll frame.
The original async body, authority, ordering, and one-allocation boundary remain unchanged.
The original explicit 2-MiB test passes both its focused invocation and the final full run; its stack limit is not raised.
The local debug measurement is not a release-stack or workload-capacity claim.
Nine earlier loopback-listener failures were caused by the restricted test harness; a direct bind probe confirmed the restriction, and the unchanged tests pass in the authorized loopback-capable run.

### Final Runner source checks and CI

The final Runner source passes locked, offline, all-target, all-feature tests: 136 passed, zero failures, and two explicitly ignored test outcomes.
Formatting and strict all-target, all-feature Clippy with warnings denied pass.
The narrow lint changes retain language-specific mailbox and workspace helpers, exact reply verification, resource bounds, and platform authority checks.
Both ignored outcomes are the same native Cargo acquisition test compiled into separate binaries; it requires crates.io access and a fresh Cargo home.
Cargo acquisition and deployed-container checks remain separate integration gates.
These host checks do not prove Linux container descendant termination or a new deployment.
Eighteen Linux-only test instances are excluded by their target configuration on macOS; the new Linux CI job owns that coverage.

The Runner now carries the same Rust 1.97.1 toolchain pin as Worker and its image build.
The Rust CI workflow watches Runner changes and adds its own locked format, lint, and test job with one compiler job.
Remote Linux checks and release builds must be read back at the pushed commit; results from the previous PR head do not cover this source.

### Current-cohort bounded restart attempt

The first restart helper attempt refuses before any fault because two Main fields are bytea and require UTF-8 decoding before a JSONB cast.
The corrected helper passes 42 offline tests and an exact preserved-run read-only terminal probe: seven resolved dispatches, eight committed platform reads, one final answer, and seven absent runtimes.
Its final binding guards distinguish pre-preparation bytes from the final selected prepared bytes while preserving exact source, input, broker, authority, and runtime identities.
Checkpoint messages retain prior history and append one final response.

A second fresh UI request completes all four languages in 20 seconds and retains exactly one new answer after reload, with no console errors.
The guarded restart helper misses the short active broker cutpoint and refuses before killing Worker.
No current-cohort Worker fault or recovery is proved by either of these attempts.
Earlier service-loss proofs retain their original recorded images and boundaries.

## Update from Main: 2026-10-05

Commit `f8a1499d128cfe226500bc33a0bbef1d465ec4fa` publishes the selected Code integration.
Its binary scan passes across 11,235 tracked files.
The original worktree retains 113 changed files, with an additional private backup.
The source checks above apply to that commit unless this section states otherwise.

Main `06122d5fec5d023d693a945aa0a326f417cbe0df` introduces conflicts in Rust, Main, generated clients, and Web.
An isolated checkout resolves these conflicts before the original worktree changes.
The combined source must retain Code identity, recovery, debug metadata, and Main's updated chat and attachment behavior.

Both branches allocate shared migrations 0134 through 0141.
Main's published migrations keep their versions and exact SQL bytes.
The unmerged Code migrations move to forward slots 0145 through 0152 without SQL changes.
The existing rehearsal already records the former Code versions.
It cannot directly apply this reconciled migration corpus without a controlled transition.
The merge does not rewrite its ledger, change running images, or prove an upgraded deployment.

Normal client generation also exposes missing OpenAPI definitions for existing Code and static-continuation types.
The owning contract must supply these definitions before regeneration.
Generated clients must retain the existing metadata and Main's new contract fields.
Final combined-source checks and pushed-head CI remain separate from the earlier frozen checks.

Regeneration exposes a source-selection omission in the earlier Code delivery.
The Web editor Test caller uses metadata whose server admission and restoration companions remained outside the selected Main source.
Compilation alone did not identify this missing behavior.
The reconciliation includes the required editor lifecycle, trace identity, and empty-Stop companions from preserved source.
It also retains Main's participant, attachment, and conversation changes.
The existing static continuation caller uses Main's single continuation route with typed request alternatives.
This correction does not introduce a duplicate route or claim acceptance for deferred graph features.

Editor Test history also needs its existing mounting companions.
The reconciled Web source connects History selection to the exact admitted run and restores its original conversation without creating another execution.
Ordinary History remains available through its existing path.
The server reads restoration state in one read-only snapshot and binds trace reads to actor, project, execution, generation, and response identity.
Code recovery metadata identifies a paused run even when its desired state is suspended and its execution state remains running.

Main's new checkbox component replaces five obsolete imports in retained Code and graph foundations.
Their values, handlers, disabled states, and default-false authoring policy remain unchanged.
The continuation schema retains the legacy common fields and finite typed alternatives.
The strict additive compatibility check remains enabled, and normal generators produce both server and browser types.

### Reconciled source checks

Worker and Supervisor pass formatting, strict Clippy, rustdoc, and the complete locked, offline, all-target, all-feature test command.
The test reports 2,066 passing outcomes, zero failures, and 63 ignored outcomes.
Eighteen passing outcomes return early without database or process-test settings; 2,048 other outcomes remain.
The original 2-MiB sensitive-HITL test still passes without a larger stack.

Main's full service-free suite passes in 141.107 seconds: 186 packages with tests, 21 packages without tests, and zero failures.
It reports 13,153 passing test and subtest events and 1,914 skipped events.
Vet, strict client compatibility, guarded contract-lock update, and normal server and browser generation pass.
Generation produces no byte drift. All 307 incoming operation identities remain unchanged, with 443 combined schemas and no unresolved local references.
All 150 incoming migration SQL files retain their exact bytes.

The strict local skip checker fails because this service-free run has 1,875 undeclared service-dependent skips; 39 skips match the declared ledger.
The merge does not broaden the ledger to conceal missing infrastructure.
Editor PostgreSQL acceptance remains unrun locally.
Its dedicated CI job supplies an isolated PostgreSQL 18 fixture and makes missing configuration, failures, and skips fatal.
Thirteen offline tests for that CI runner and receipt guard pass; they do not prove database acceptance.

Web passes full TypeScript, full lint with warnings denied, and the production application build.
All 74 affected suites pass: 1,025 tests, zero failures, and zero skips.
The test command uses two workers. The production build takes 20.179 seconds locally.
These are combined-source checks, without new browser or deployed-image acceptance.

Protocol format, lint, build, and compatibility checks against incoming Main pass.
The Worker, sandbox, and Kubernetes sandbox Helm render guards pass.
Thirty-four Code packaging and deployment-material tests pass.
Runner source and pinned dependencies remain unchanged from `f8a1499`; its earlier checks retain their stated host and platform boundaries.
Pushed-head CI must verify the resulting merge commit separately.

### Preserved unfinished work

The original 113 changed files are saved in stash `b7f78424bc61786af1450321071edb412ad406e5`.
The stash label is `preserve unfinished graph work before main reconciliation 2026-10-05`.
Independent read-back verifies every saved file against its original digest and private backup.
The stash remains intact. Its graph changes require selective reconciliation with the updated Main source before further delivery.
