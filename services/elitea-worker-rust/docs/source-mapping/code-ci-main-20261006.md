# Code CI repair and Main reconciliation

## Scope

The previous merge includes Main `06122d5fec5d023d693a945aa0a326f417cbe0df`.
Its published Code integration commit is `94e8b11704d5e569a4cf42217d2edd590ec2772b`.
Main then advances to `7ec27cd07fe0c01f32da6c8630f0ae2f169e959b`.
The new reconciliation preserves the incoming image, HITL, native-client, dependency, and DeepWiki changes.

The unfinished graph work remains in the named preservation stash.
Its identity is `b7f78424bc61786af1450321071edb412ad406e5`.
The reconciliation does not apply or discard that stash.

## Owning paths

| Boundary | Source owner | Required behavior |
| --- | --- | --- |
| Kubernetes render acceptance | `deploy/helm/tests/render-sandbox-kubernetes.sh` | Run positive and refusal assertions without an undeclared search executable. |
| Sandbox image render acceptance | `deploy/helm/tests/render-sandbox-images.sh` | Retain digest and node-selection refusal checks. |
| Editor Test acceptance runner | `scripts/go/editor-lifecycle-postgres.sh` | Keep Go JSON events separate from compiler and download diagnostics. |
| Editor Test acceptance gate | `scripts/go/editor-lifecycle-gate.py` | Reject missing, duplicate, failed, or skipped scenarios. |
| Editor Test runner regression | `scripts/go/tests/test_editor_lifecycle_ci.py` | Preserve diagnostic output without corrupting required event parsing. |
| Public API | `services/elitea-main/api/openapi/v2.yaml` | Preserve every incoming operation and schema during generation. |
| Generated server | `services/elitea-main/internal/api/generated/api.gen.go` | Resolve the merge through the owning generator. |
| Terminal finalization | `services/elitea-worker-rust/src/execution/native_agent_lifecycle.rs` | Preserve the 2 MiB lifecycle regression through an allocation factory. |
| Compiler fixtures | `services/elitea-code-runner/src/compiled_compiler.rs` | Isolate fixtures that own process-wide child reaping. |
| Project removal | `services/elitea-main/internal/application/projectprovisioning/steps.go` | Remove new dependent rows before their owners, preserving other projects. |
| Compilation retention test | `services/elitea-main/internal/infra/db/repos/compiled_snapshots_postgres_integration_test.go` | Require the exact protected foreign key under either valid PostgreSQL violation code. |
| Fresh HITL authoring | `apps/elitea-web/src/features/pipelines/lib/flow-editor/constants/nodeDefaults.constants.ts` | Omit an absent optional edit key before strict serialization. |
| Pipeline journey assertions | `apps/elitea-web/e2e/journeys/pipelines/pipelines.validation.spec.ts` | Verify enabled pause controls, exact stored identities, clearing, and invalid identity refusal. |

Current-platform business behavior remains a reference through the existing Code and pipeline source mappings.
These CI repairs change verification boundaries, not the current-platform behavior contract.

## Verified repairs

The Helm job fails because the runner lacks `rg`.
The two affected scripts now use standard `grep` predicates.
Three affected render scripts pass with `rg` absent from their execution path.
The same three scripts also pass with checksum-verified Helm `3.16.0`, the pinned CI version.
Both runs exclude `rg` from their execution path.
These local results do not replace the required CI result.

Editor Test passes its Go tests but fails when download diagnostics reach the JSON parser.
The runner now retains stderr in a separate artifact.
Its JSON gate reads stdout only and retains its strict acceptance rules.
Fourteen gate and runner tests pass without failures or skips.
The added regression emits a download diagnostic before complete acceptance events.
Actual PostgreSQL acceptance remains a separate gate.

The disposable PostgreSQL 18 fixture now passes all thirteen required Editor Test scenarios without skips.
Ten compiled-snapshot lifecycle and guard assertions pass with race detection and no skips.
The retention assertion accepts only `23001` or `23503` and the exact named foreign key.
PostgreSQL can report either code while retaining the original compilation receipt.
Thirty-five provisioning assertions pass, including every blocking foreign key and the new dependent rows.
The bystander test preserves complete source rows from another project.
Four existing provisioning cases skip because this fixture lacks the vector extension.
The fixture uses no live deployment credentials or data.

Main-wide lint and vet pass.
The broader changed-package check passes 2,275 test events with fifteen existing environment-dependent skips.
Those service-free skips do not replace dedicated PostgreSQL or deployed acceptance.

Web's ten affected suites pass 257 tests without skips.
Type checking, full lint, layer checks, dead-code checks, normal generation, and the production build pass.
Chromium and WebKit each pass five focused cases against the production editor in an isolated component router.
They verify fresh HITL saving, pause identity roundtrips, invalid identity refusal, Code debug fields, and bounded native Blob behavior.
Each browser records five version writes and five separate typed application-metadata writes.
The fixture intercepts application APIs locally and refuses unknown requests.
Both browsers report zero page and console errors.
This browser proof establishes editor behavior and request contracts, rather than backend persistence or execution recovery.
These local Web checks use the existing installed dependency cache.
Thirty-two direct dependency versions differ from the incoming lock, and local Node versions precede the declared Node 26 requirement.
The tests establish the checked source behavior within that cache; they do not establish exact-lock CI equivalence.
Replacement CI must install the incoming lock under its declared runtime and renderer before final acceptance.

The Rust worker's unchanged 2 MiB regression and all 67 output-delivery tests pass with current locked dependencies.
All-target, all-feature strict Clippy also passes.
These macOS results do not replace Linux CI stack verification.
See [the lifecycle mapping](terminal-finalization-stack.md) for frame measurements and their limits.

The Runner's deterministic Linux control proves process-wide cleanup can reap another fixture's child.
Two compiler fixtures now run in isolated child processes; production cleanup remains unchanged.
Linux ARM64 verification passes 155 outer tests with zero failures and two existing ignored instances.
Both ignored instances belong to the existing native-Cargo test requiring network and a fresh cache.
Strict Linux Clippy passes for all targets and features with warnings denied.
See [the Runner mapping](../../../elitea-code-runner/docs/source-mapping/linux-compiler-fixture-ownership-20261006.md) for ownership and verification boundaries.

The auto-merged OpenAPI retains all 307 incoming operations and 430 incoming schemas.
The merged document has 444 schemas and 2,026 resolved local references.
Its YAML has no duplicate keys.
Structural preservation does not prove generated client behavior or browser acceptance.

## Remaining verification

The failed CI results belong to commit `94e8b11704d5e569a4cf42217d2edd590ec2772b`.
The repairs preserve the existing resource limits and refusal assertions.
No new test skips, increased thread stacks, or relaxed visual thresholds are introduced.
The replacement head still requires the complete Linux CI matrix.
The enabled pause-control visual case also requires a fresh pinned CI screenshot capture.
Read the resulting CI state before declaring this repair complete.

The live rehearsal services and databases remain unchanged during these source checks.
The earlier migration-transition proposal has no runtime proof.
Fresh deployment and mandatory browser acceptance remain open.
