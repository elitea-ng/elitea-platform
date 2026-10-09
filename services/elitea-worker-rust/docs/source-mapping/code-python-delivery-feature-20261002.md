# Python durable dependency delivery

Date: 2026-10-02. Status: partial; infrastructure and browser acceptance remain open.

This feature connects the existing native Python resolver, immutable bundle storage, and offline execution consumer. The worker holds the Code frontier pending until preparation is published and the preparer is confirmed terminated. It then binds that exact root into the execution request, hydrates the original inert execution runtime, verifies the imported content, and dispatches once.

## Current-to-new source mapping

The current SDK behavior reference is revision `a54db410a46bac5e2c3cbc2db46c148a5c3d069c`, `infra/data/sandbox/main.ts::install_imports` and `runPython`. It discovers missing imports and installs dependencies before executing source. The extraction base is `ad4da99ff2dcf63b9c92c76e1fadb837c550153a`; its production code is unchanged from `abadda78853aaaf5f12e7cb3cc39386bed96bcfc`. Its existing Python preparation adapters discover imports and literal requirements, freeze native resolution, and produce verified bundle files. This feature connects those components; it does not port the legacy process lifecycle literally.

| Current behavior or frozen-base contract | Feature owner | Behavior and acceptance boundary |
| --- | --- | --- |
| SDK Python dependency discovery and installation before `runPython` | Existing runner `adapters/{prepare_python_code,python_requirements,python_preparation_job}.mjs`; worker `src/agents/graph/code_preparation.rs` | Native Pyodide/micropip resolution remains image-owned. The worker supplies bounded literal source as data to a separate admitted preparer, without executing that source during resolution. |
| Existing `PreparationJob` and stable preparation activation | `src/sandbox/preparation.rs`, `src/protocol/sandbox_authority.rs`, `src/sandbox/client_preparation.rs` | The preparation journal is registered before authorization. Retries retain the exact activation, request digest, image, and policy. The trusted Deno command uses the image-owned frozen lock with cached-only module loading. |
| Existing Main claim-bound sandbox grants | `libs/proto/elitea/runtime/v1/sandbox.proto`, `src/protocol/sandbox_grant.rs`, `src/sandbox/service.rs` | Three additive RPCs separate Prepare, Publish, and Hydrate. Revision 1 job authority, revision 2 cancellation, and revision 3 content authority cannot substitute for one another. |
| Existing Main immutable shared bundle store and private content listener | Main `internal/infra/storage/sandbox_bundle{,_http}.go`, `internal/transport/runtimegrpc/control/sandbox_grant.go`, `internal/runtimecomposition/composition.go`; worker `src/sandbox/dependency_{bundle,content,content_transfer,content_cache}.rs` | Main remains the authoritative shared storage owner. Package bytes move over private mTLS HTTP, outside RPC control, the command bus, and checkpoints. Metadata publishes last after verifying all files. No Main production source change is required at this base. |
| Existing `0009_sandbox_preparation_bundle.sql` receipt column | `src/sandbox/{ledger,docker_preparation}.rs` | Persist one immutable resolved bundle while Dispatched. Indexed publication and exact shared metadata permit recovery without repeating resolution. Complete only after confirmed preparer termination. |
| Existing exact Docker container and Kubernetes Pod identities | `src/sandbox/runtime.rs`, `libs/rust/vendor/adk-sandbox/src/workspace/{docker,docker_dependency_content}.rs`, `src/sandbox/kubernetes/{client,runtime,dependency_content}.rs` | Stream one bounded file through fixed commands. Preserve container ID or Pod UID/request checks before, during, and after transfer. Replacement runtimes cannot serve the original request. |
| Existing root-bound offline Python execution request | `src/sandbox/{request,client_hydration,docker_hydration,docker_dependency_delivery,docker_supervisor}.rs` | Hydrate the original Reserved runtime under separate current job/content grants. Final index verifies every imported file and imports metadata last. Submission verifies that metadata before durable dispatch; dispatched recovery does not download again. |
| Preparation files in `/workspace/python-dependencies`; execution imports in `/workspace/wheels` | Runner `src/lifecycle.rs`; runtime `export_execution_dependency` | Add the fixed `--execution-dependency-read` command. Final hydration and submission read execution imports from their own directory, with no fallback to preparation files. |
| Existing Code graph state projection and durable checkpoint owner | `src/agents/graph/code_remote.rs`, `src/sandbox/dispatch.rs`, existing graph/checkpointer code | Preparation and hydration do not update graph state. Only the final execution receipt advances the frontier. Stop selects both preparation and execution audiences. A resolved preparation journal prevents downgrade when configuration changes. |
| Existing phase-deadline migration and old supervisor compatibility | Existing Main migration `0010_sandbox_phase_deadlines.sql`; `src/sandbox/{ledger,process,docker_supervisor,docker_deadline_tests}.rs` | Preserve readiness from first runtime binding and execution from durable dispatch. Old writers that already bound a runtime with a NULL timestamp retain `created_at` as fallback. Startup requires all deadline and preparation columns. |
| Reserved execution runtime awaiting hydration or final verification | `src/sandbox/{docker_hydration,docker_supervisor,docker_hydration_deadline_tests}.rs` | Bound hydration retention across lease replacement and stalled transfers. Unconfirmed original-runtime termination keeps the receipt Reserved for cleanup retry. Twelve real PostgreSQL regressions pass; the correction remains undeployed. |
| Existing consolidated supervisor and Main audience configuration | `deploy/docker-compose.sandbox-preparation.yml`, `deploy/runtime/sandbox-preparation{.example.json,-deployment.md}`, `deploy/scripts/gen-sandbox-certs.sh`, `src/{config,bootstrap}.rs`, `src/sandbox/process.rs` | Optional Python-only preparation profile, separate preparation/content TLS leaves, bounded private staging, and operator-selected resolver network. Execution retains offline network policy. Kubernetes resolver namespace egress remains operator-owned. |

Worker paths are relative to `services/elitea-worker-rust`. Runner and Main paths identify their owning services. See the linked focused mappings in the source registry for wire fields, bundle bounds, file framing, publication recovery, and execution export details.

## Implementation history and compatibility

1. Extract only Python delivery and its shared contracts from preserved stash `a0aceb1ce5d2dd19f90b2c29f7e9d962d81844b6` into private copies of the frozen base. Rebase the documentation onto the subsequent phase-deadline acceptance commit, preserving Docker/Kubernetes browser proof, the current Point 5 subsection, and child-variable recovery correction.
2. Retain the current phase SQL, NULL binding fallback, startup guards, migration 0010, and deadline regression suite. Adapt only the suite's new method argument.
3. Exclude Cargo declarations/editor/native preparer, JavaScript demand preparation, unrelated Main catalogue changes, and deployment recovery flags.
4. Correct execution exports in runner, Docker, and Kubernetes routing; add final Hydrate/Submit regression coverage with a role-aware runtime.
5. Regenerate Go and Python bindings with the pinned owning script. Rust remains generated by `build.rs`.
6. Add only `tempfile = "=3.27.0"` to the runner and its minimal lock closure; preserve every existing dependency version. Cache the existing Python preparer in the Deno image.
7. Reconcile seven historical migration receipts after isolated-copy verification, then run the unchanged current Main migrator twice. Deploy the optional Docker profile and record its initial preparation failure.
8. Select `--frozen --lock=/opt/elitea-code/deno.lock` in `PreparationJob::manifest`; prevent the cached-only Pyodide dependency request for `@types/emscripten`. Verify the focused manifest regression and strict Clippy, then deploy the corrected supervisor. Verify automatic imports, explicit transitive packages, and execution after worker restart on Docker.
9. Verify Stop during dispatched execution, the preparation sentinel, and missing-package refusal through pipeline 142. Preserve separate preparation and execution receipts, downstream failure behavior, and the verified cleanup boundaries.
10. Apply the Kubernetes parity resources and approve two exact CDN routes over TCP 443. Verify scoped network probes, native 20,000-record Test chat execution, and a fresh persistent-chat request.
11. Add durable hydration retention and fenced expiry cleanup. Verify strict all-target/all-feature Clippy and twelve real PostgreSQL regressions; retain deployment acceptance as an open gate.

Profiles without preparation keep their existing revision 1 execution bytes. Only a configured Python profile can prepare dependencies. Literal source admission and existing dynamic-source approval requirements remain unchanged. This feature introduces no new table or migration. The rehearsal-only receipt audit is outside the feature schema. It requires the existing receipt migrations through 0010 and the existing Main object-store and content-listener activation.

Preparation resolution, publication, and hydration each use fixed worker observation budgets derived from the preparation timeout plus 90 seconds. Final execution observation begins after readiness, using execution timeout plus 90 seconds. Supervisor readiness and execution phase clocks remain durable across lease changes and replacement. The indexed protocol does not grant unbounded operation time.

## Verification during extraction

Private patch assembly runs source checks only.
These include pinned protocol generation, Buf compatibility, formatting, syntax, lockfile preservation, source invariants, and application to private copies.
The extraction report records their results.
It records no build, database, container, deployment, or browser acceptance.
Stash-era results do not prove this feature.

## Verified checks after application

The following checks run after the Python feature and runner test-helper correction are applied.
The root records successful command exits. Current logs confirm the selected Rust test counts.
These checks do not constitute a complete worker test suite.

Commands below run from the owning service directory, except generated Go bindings.
Log names are relative to `/private/tmp/`.

| Check | Verified result | Evidence |
| --- | --- | --- |
| Worker `cargo fmt --all -- --check` | PASS | Root command result |
| Worker default features: `cargo check --locked --all-targets` | PASS | `elitea-python-delivery-default-check-20261002.log` |
| Worker all features: `cargo check --locked --all-targets --all-features` | PASS | `elitea-python-delivery-worker-check-20261002.log` |
| Worker `cargo clippy --locked --all-targets --all-features -- -D warnings` | PASS | `elitea-python-delivery-worker-clippy-20261002.log` |
| Worker `sandbox::` selection, `--locked --lib --all-features` | 66 passed; 0 failed; 27 ignored; 1331 filtered out | `elitea-python-delivery-sandbox-tests-20261002.log` |
| Worker `agents::graph::code_remote` selection, `--locked --lib --all-features` | 4 passed; 0 failed; 0 ignored; 1420 filtered out | `elitea-python-delivery-code-remote-tests-20261002.log` |
| Worker `protocol::sandbox_grant` selection, `--locked --lib --all-features` | 13 passed; 0 failed; 0 ignored; 1411 filtered out | `elitea-python-delivery-grant-tests-20261002.log` |
| Runner `cargo test --locked lifecycle::tests` | 14 passed; 0 failed; 0 ignored; 3 filtered out | `elitea-python-delivery-runner-tests-20261002.log` |
| Runner `cargo clippy --locked --all-targets -- -D warnings` | PASS | `elitea-python-delivery-runner-clippy-20261002.log` |
| PostgreSQL preparation selection | 10 passed; 0 failed; 0 ignored; 1414 filtered out | `elitea-python-delivery-pg-preparation-20261002.log` |
| PostgreSQL phase-deadline selection | 6 passed; 0 failed; 0 ignored; 1418 filtered out | `elitea-python-delivery-pg-deadlines-20261002.log` |
| PostgreSQL dispatch-journal selection | 1 passed; 0 failed; 0 ignored; 1423 filtered out | `elitea-python-delivery-pg-journal-20261002.log` |
| PostgreSQL hydration-retention selection after cleanup correction | 12 passed; 0 failed; 0 ignored; 1425 filtered out; 24.10 seconds | `elitea-python-delivery-pg-hydration-retention-20261002.log` |
| Generated Go, from `libs/proto/gen/go`: `go test ./elitea/runtime/v1` | Compiles successfully; no test files | Root command result |
| Main `go test ./internal/transport/runtimegrpc/control -run Sandbox` | PASS; 0.190 seconds | Root command result; no test count claimed |
| Main `go test ./internal/infra/storage -run Sandbox` | PASS; 0.321 seconds | Root command result; no test count claimed |

PostgreSQL selections use `cargo test --locked --lib --all-features <filter> -- --ignored --test-threads=1`.
Their filters are `sandbox::docker_supervisor::preparation::tests`, `sandbox::docker_supervisor::deadline_tests`, and `sandbox_dispatch_journal_preserves_exact_pending_identity`.
The private PostgreSQL wrapper supplies disposable TLS material.
Its runs explicitly select ignored component tests. They do not clear the other 27 ignored sandbox tests.
The worker test linker reports its existing compact-unwind size warning.
Check and Clippy success do not remove that warning or prove production performance.

The preparation test `final_hydration_and_submission_verify_execution_files_before_dispatch` invokes public Hydrate and Submit operations with signed grants.
A role-aware fixture rejects corrupted execution files and changed metadata before dispatch.
Successful hydration remains Reserved. Terminal replay retains one dispatch.
This component test performs no HTTP download or Python execution.

## Exact runner image and native Docker evidence

The private runner context contains 33 source, adapter, and manifest files, without build targets or caches.
The context totals 158,960 bytes, including `.dockerignore`.
The snapshot includes the runner test-helper `create_dir_all` correction.
Source hashes still match after verification.
The source manifest SHA-256 is `0331139bab9571124bdbb3b212f484ac82acfaa685c1f5d7d833d8b33cbe5827`.

The Deno runtime image build exits 0.
It uses the current Containerfile's locked release build with two Cargo jobs.
Its Python package list is empty. The standard JS/TS package list remains unchanged and is also empty.
This proves core JavaScript and TypeScript execution only.

The offline baseline process exits 0, with eight passing assertions.
Each container's Docker wait exits 0; child exit codes and receipts provide the language result.

| Baseline case | Receipt | Child exit |
| --- | --- | --- |
| `import yaml` before preparation | Failed; native resolver cannot fetch `yaml` metadata | 1 |
| Python core | Completed | 0 |
| JavaScript core | Completed | 0 |
| TypeScript core | Completed | 0 |
| Python exception | Failed, as expected | 1 |
| Network attempt | Failed, as expected | 1 |
| Subprocess attempt | Failed, as expected | 1 |
| Infinite JavaScript loop | Timeout, as expected | No child exit code |

Observed container policy includes UID/GID 10001, network `none`, a read-only root, 512 MiB memory, one CPU, and 64 PIDs.
Capabilities are dropped. No-new-privileges and a bounded noexec workspace apply.
Each case rereads an unchanged terminal receipt and removes its container.

The real Docker transfer fixture uses a private copy of the root's current all-features worker test binary.
The copied binary and shared source hashes match before and after the run.
Their SHA-256 is `3191a8bc5eb6e83e95ee602044026613c3e5a9b2f6fbe57175475225700ff08c`.
No Cargo process or shared-target write occurs during this run.

```sh
ELITEA_CODE_RUNNER_TEST_IMAGE=sha256:dbca393b112ba550cc11eca48eebb7d9b4d7cd55108f08403b0c75a356cc211a \
TMPDIR=/private/tmp/elitea-python-runner-image-20261002/fixture-temp \
/private/tmp/elitea-python-runner-image-20261002/worker-tests \
  sandbox::dependency_runtime_docker_tests::live_docker_dependency_streams_binary_content_and_fences_replacement \
  --ignored --exact --nocapture
```

The fixture exits 0: 1 passed, 0 failed, 0 ignored, and 1423 filtered out, in 2.52 seconds.
It transfers 2 MiB plus 17 bytes of non-UTF8 content through the fixed preparation and execution directories.
The execution export helper returns imported bytes for verification.
A separate checksum command does not substitute for that helper.

Checks cover directory roles, immutable retries, changed-byte refusal, original-instance fencing, release, resource policy, and confirmed cleanup.
All three owned runtime instances terminate and clean up.
The image-scoped container inventory reports zero remaining containers; the private fixture directory is empty.
An auxiliary Docker history query returns zero events. Its event-count assertion exits 1 and provides no cleanup proof.
Passing fixture cleanup assertions and the empty inventory provide that proof.

Evidence is under `/private/tmp/elitea-python-runner-image-20261002/`:
`source-manifest.json`, `image.json`, `fixture-binary.json`, `verification-report.json`, `build.log`, `baseline-probe.log`, `baseline-results.json`, and `native-transfer.log`.
The fixture uses synthetic binary content and trusted holding code.
It does not prove native package acquisition, Main publication, complete live Hydrate/Submit, or browser behavior.

## Rehearsal packaging and deployment boundary

The root verifies these canonical local image IDs with Docker image inspection.
All four images are arm64.

| Rehearsal image or component | Canonical Docker `.Id` | Packaging boundary |
| --- | --- | --- |
| `elitea-code-runner:python-delivery-20261002` | `sha256:dbca393b112ba550cc11eca48eebb7d9b4d7cd55108f08403b0c75a356cc211a` | Exact runner source; normal locked release build; empty Python package list |
| `elitea-worker-rust:python-delivery-20261002` | `sha256:65d3f00d70156a2811b1657541dd85b6944fe0200c7a107c28ed12565b94c679` | Private rehearsal packaging; release opt-level 0, LTO disabled |
| Sandbox supervisor after preparation-lock correction | `sha256:d0b9bdc8b9b9e33a90cfd4e8a484b0fe3353ea47637b172e12065a5125a047cc` | Corrected image-owned preparation lock; private rehearsal packaging |
| `elitea-main:python-delivery-20261002` | `sha256:4ff83e44ec02012d2c9a4c85623ad1dd45adc4a48fd8d1b7b92540d23b08f3d6` | Rebuilt Main binary over previous runtime/UI image |

Build logs are `elitea-python-delivery-{worker,supervisor}-build-20261002.log` and `elitea-python-delivery-main-image-build-20261002.log` under `/private/tmp/`.
The Main binary build also succeeds. Its empty build log provides no runtime proof.
The worker and supervisor rehearsal builds use one Cargo job and 16 codegen units.
Their opt-level 0 packaging does not prove production shipping or performance.

Startup detects the known old rehearsal shared-ledger collision: identities 126–132 conflict with canonical identities 127–133.
The [Main integration history](main-integration-20261001.md) records the canonical migration identities and checksum checks.
The initial attempt restores the previous healthy Main/UI and service images.
The root then verifies rollback, guarded receipt reconciliation, and normal migration against an isolated rehearsal copy.
That copy reaches shared head 133 and tenant head 138; both normal all-tenant migrator runs exit 0.
The scoped audit retains seven original receipts, and the existing Main admission-reservation table is present.

The live reconciliation follows a verified backup and quiesces Docker and Kubernetes Main and worker processes.
The transaction takes the normal migration advisory lock and fences seven descending receipt updates by their original names, checksums, and timestamps.
Only receipt version numbers change from 126–132 to 127–133; their SQL, checksums, and original application timestamps remain unchanged.
The scoped audit retains those originals. The unchanged current migrator applies Main's missing migration 126 and succeeds twice across all tenants.
The active product reaches shared head 133 with 104 receipts and tenant head 138.

The repair preserves all execution-job, claim, and command-outbox rows through full before/after comparisons.
It permits only the unchanged snapshot of 26 unfinished August jobs, with no live claims.
Extra, changed, or recent unfinished jobs are refused.
Those historical jobs remain unfinished. This receipt repair introduces no runtime cleanup or general migration bypass.

Docker acceptance uses the exact Main, supervisor, and worker images listed above, with the optional preparation profile enabled. Main is healthy.
The Kubernetes parity helper applies seven resources and selected deployment patches.
Main, supervisor, and one worker are Ready on the listed feature images.
The Docker worker is paused during Kubernetes acceptance to avoid competing consumers.

The initial pipeline 142 attempt reaches `sandbox.preparation_failed` before user code executes.
The preparation-lock correction resolves that failure; the Docker acceptance below uses the corrected supervisor and unchanged runner image.
The earlier phase-deadline Docker/Kubernetes browser acceptance belongs to its separate feature.

[Deployment parity](code-python-deployment-parity-20261002.md) records the optional Kubernetes preparation boundary, staging, and aliases.
Its focused Helm lint and render checks pass.
Those fixture checks do not prove live TLS, registry egress, Pod execution, or browser behavior.

## Verified Docker dependency execution

The focused `manifest_uses_the_image_lock_for_cached_only_preparation` regression and strict Clippy pass after the preparation-lock correction.
The supervisor replaces initial image `sha256:549b4c1722409f674870d8dd31e7d25dc1f97800eb139d80c233136314610ad2` with the canonical image listed above.
The runner image remains unchanged, with no prepared Python packages baked into it.

Pipeline 142 Test chat resolves the automatic `yaml` import and returns exactly `{"case":"automatic-alias","count":2,"last":"beta"}`.
Recorded identity prefixes are preparation `cd0d758d`, bundle root `eeb3659a`, and execution `7b09ccd`.

The [Python fixture](../../../../scripts/runtime/fixtures/python-demand-delivery/process.py) requests `python-slugify==8.0.4` and `python-dateutil==2.9.0.post0` explicitly.
Its resolved closure contains seven files. Async batches, dataclasses, sorting, and dependency calls process 20,000 records.
Test chat and persistent chat 782 match the complete [expected result](../../../../scripts/runtime/fixtures/python-demand-delivery/expected-20k.json).
Persistent reload preserves the original result without duplication.
The [fixture instructions](../../../../scripts/runtime/fixtures/python-demand-delivery/README.md) distinguish the normal request from the delayed worker-restart case.

The restart case uses root execution `b13f27b58ece4c9fee0c3a05dd7fc8d0` and sandbox job `78ca091cf422c7e87ae7a67206bef678ec5e5db17688795f83000106c64efe8e`.
Durable dispatch records 11:45:20; the worker restarts at 11:45:21; the receipt completes at 11:46:20.298327.
The runtime remains `d9381280f0466318fced541c24bacf66078a6fc3dced7e648b863e80de50fb73`.

Persistent reload retains two exact results for two requests, without an error or pending Stop control.
The restart request has exactly two resolved durable dispatches: one preparation audience and one execution audience.
Their audiences are `dns:elitea-sandbox-preparation` and `dns:elitea-sandbox-deno`; these are separate phases, with one execution dispatch.
The runner-image container inventory is empty, confirming cleanup.
This dispatched-execution case does not establish recovery during preparation, publication, or hydration, or supervisor replacement.

## Verified Docker cancellation and preparation boundary

Active Stop targets sandbox job `0da57cd9aaa06305b7eb2a0e8bea65a8f3a6609aa87d62395cddf057bb1bd128`, dispatched at 11:55:52.951972.
The original runtime prefix is `3e89b83`; cancellation completes at 11:56:07.764671 with safe result `sandbox.cancelled`.
The runner-image container inventory is empty, and the UI adds no result for the cancelled request.
Earlier Stop attempts before dispatch or after code completion do not establish cancellation during active execution.

The [sentinel pipeline](../../../../scripts/runtime/fixtures/python-demand-delivery/preparation-sentinel.yaml) raises before its literal `micropip.install` call and includes a downstream node.
Pipeline 142 root execution is `68f2ba3c7b7db019b6c510a033149f5e`.
Preparation prefix `de50df764` completes at 12:00:41.916807; execution prefix `5079bb05` fails at 12:00:45.382339 with `sandbox.code_failed`.
Preparation therefore completes without evaluating the raising user source. The exception occurs only in execution.

The sentinel request records one preparation dispatch and one execution dispatch, both resolved.
No downstream receipt or UI value appears. The UI reports the Code failure and explains that later nodes did not run.
The runner-image container inventory is empty, confirming cleanup.

The [missing-package fixture](../../../../scripts/runtime/fixtures/python-demand-delivery/missing-package.yaml) requests an unavailable literal distribution and contains an unreachable result.
Preparation job `004d9add7ac9dea499d787ab33917a798b9ed5ccb77ba87a755a546056bef068` starts at 12:04:24.790666 and fails at 12:04:28.406396 with `sandbox.preparation_failed`.
The observation window beginning at 12:03:20 contains no newly created execution job.
The UI displays one explicit Code failure, explains that later nodes did not run, and shows no unreachable value.
Its current wording does not name the missing package. The valid processing source is restored after the negative tests.

## Verified Kubernetes preparation and execution

The operator approves two exact CDN CIDRs over TCP 443 for preparation after an explicit approval question.
Fresh probes use the listed runner image, UID 10001, and no service-account token. Both probe Pods receive UID-bound cleanup.
Preparation verifies TLS with HEAD status 200 for the required Pyodide `python_stdlib.zip` and PyPI; the Python-hosted root returns 404.
Public, cluster-API, and metadata-route attempts time out. Execution DNS and registry-route attempts also time out.

These are measured connectivity outcomes. Static CDN address routes do not enforce FQDN identity and can become stale after DNS rotation.
HEAD responses establish TLS and HTTP status, without transferring or validating complete artifact bytes.
The following native UI executions provide separate evidence of real dependency download and offline execution.

Pipeline 142 Test chat matches the complete 20,000-record fixture result.
The fresh Kubernetes request in persistent chat 782 adds the same exact result as its third completed request.

| Request | Phase | Job identity or prefix | Completion time | Pod UID prefix |
| --- | --- | --- | --- | --- |
| Test chat 142 | Preparation | `0a7ad3e20ba65fb74b83027b5c639af5483a77856d5cefb87aea79a990ca780b` | 12:13:58.111476 | `4c6ab29a` |
| Test chat 142 | Execution | `6d05cd8ccaba444c7e5c9769c00ec7420d5bff44d4d1a666ab527417151c9c7d` | 12:14:04.329052 | `dcda5f63` |
| Persistent chat 782 | Preparation | Prefix `6de843e4` | 12:17:31.379465 | Not recorded here |
| Persistent chat 782 | Execution | Prefix `6f6848e1` | 12:17:38.691639 | Not recorded here |

Both preparation and execution namespaces contain no Pods after settlement.
Persistent chat 782 reload retains exactly three expected 20,000-record outputs for its three successful requests, with no Stop control.
Both namespaces still contain no Pods after reload.
The earlier blocked-network run, prefix `b496`, reaches its deadline at 12:12:57; no Stop is delivered.
That run provides no active Stop acceptance evidence.

The durable hydration-failure cleanup correction passes source Check, formatting, diff checks, and strict all-target/all-feature Clippy.
Its actual PostgreSQL regression selection passes all twelve tests, with zero failures or ignored tests, in 24.10 seconds.
The component tests use a fixture runtime; they do not prove deployed failure cleanup.
The corrected cleanup code is not yet deployed, and its deployment acceptance remains open.

## Required acceptance before feature completion

1. Exercise live bundle-integrity, transport-failure, and registry-outage cases. Verify user code and dependent nodes remain blocked, and confirm failed-runtime cleanup.
2. Replace worker and supervisor during preparation holding, publication, and inert hydration; also replace the supervisor during dispatched execution.
   Verify one durable root, no repeated resolution, the original runtime identity, one execution dispatch, confirmed cleanup, and unchanged terminal replay.
   Exercise Stop before execution and registry failure. Dependent nodes must not run.
3. Complete Kubernetes replacement, Stop, and negative cases with exact Pod UID fencing.
   Preserve separate resolver and offline execution namespaces.
   Require refusal when an original-identity operation reaches a same-name replacement Pod.
4. Verify safe preparation error details, earlier-phase Stop and replacement, and terminal replay through the browser.

Existing-activation recovery retains the recorded root; later-run lookup from requirements to a previously frozen bundle is outside this extraction and remains unproven.
Language-specific components retain separate source mappings and acceptance boundaries within the complete Point 5 delivery.

Gate 5 stays open until these acceptance cases pass. JavaScript/TypeScript and Cargo preparation need their own language-specific immutable bundle formats, launcher selection, source/platform receipt binding, and delivery acceptance. They may inherit this feature's protobuf methods, typed authority and exact-runtime transfer structure, phase clocks, bounded staging, cancellation/journal behavior, and deployment identity separation. The Python bundle schema and fixed directory names must not be advertised as generic native-language delivery.
