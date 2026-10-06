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
| Private Code material mounts | `scripts/runtime/test_code_consumers_deployment.py` | Require explicit host-path refusal in authored Compose, across normalized JSON representations. |
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
| Selected model request | `apps/elitea-web/src/widgets/chat-box/ui/hooks/useChatBoxSend.ts` | Refresh model identity and settings together for creation, start, and regeneration. |
| Artifact fixture contracts | `apps/elitea-web/src/test/msw/fixtures/artifacts/` | Preserve synthetic response bodies after source verification and update their verification metadata. |
| Saved graph viewport | `apps/elitea-web/src/features/pipelines/ui/useFlowEditorLifecycle.ts` | Finish reset persistence and fitting before acknowledgement; keep authored graphs visible after refresh. |
| Sandbox test schema | `services/elitea-worker-rust/src/sandbox/docker_*tests.rs` and `src/state/postgres_checkpointer_tests.rs` | Include Main's recovery migration in fresh test databases. |
| Project Context save cache | `apps/elitea-web/src/pages/settings/ProjectContext.tsx` | Install the committed response before a pending background read can restore old content. |
| Fixed graph control clearance | `apps/elitea-web/src/features/pipelines/ui/flowEditorFitView.ts` | Measure the control column and reserve horizontal fit padding before ordinary pointer actions. |
| Signed-in shell readiness | `apps/elitea-web/e2e/journeys/shell/shell.redirect.spec.ts` | Wait for the signed-in shell before the existing Profile deep-link and Logout assertions. |

Current-platform business behavior remains a reference through the existing Code and pipeline source mappings.
These CI repairs change verification boundaries, not the current-platform behavior contract.

## Verified repairs

The Helm job fails because the runner lacks `rg`.
The two affected scripts now use standard `grep` predicates.
Three affected render scripts pass with `rg` absent from their execution path.
The same three scripts also pass with checksum-verified Helm `3.16.0`, the pinned CI version.
Both runs exclude `rg` from their execution path.
These local results do not replace the required CI result.

The replacement Helm job passes chart rendering but fails its Code consumer packaging test.
Its normalized Compose JSON omits `create_host_path: false`.
The assertion now verifies the explicit safeguard in the authored overlay before it accepts an omitted false field.
It also verifies all three exact targets, bind types, and read-only mounts.
The regression rejects an absent authored safeguard and either authored or normalized `true`.
Seven packaging tests, four material installation tests, and twenty-four public-trust tests pass without skips.
All three charts pass local Helm `3.16.0` lint.
These checks do not start services or read deployment credentials.

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

Replacement CI installs the exact lock under Node 26 and passes Web quality, Storybook, and production build checks.
Its pinned full-application visual suite passes 67 cases.
The enabled pause-control case passes its behavior assertions, then fails because its new baseline does not exist.
The reviewed 1,602 by 848 pixel CI image becomes that baseline without any transformation.
The remaining visual cases include all four updated editor baselines.
No visual tolerance changes.

One exact-lock Web shard times out while the idle editor Test fixture enters a multiline draft.
The unchanged case passes locally; the timeout is not reproduced as a product deadlock.
The fixture now preserves its first focused keystroke and Shift+Enter, then pastes the same bulk text.
Pending preparation, attachment refusal, input identity, exact admission, and duplicate-send assertions remain.
Twenty-two focused tests pass without skips; type checking and owning lint pass.
No product code, dependency, timeout, or retry changes.
The exact-lock replacement CI result remains required.

The next static gate finds nine dependencies in the regeneration callback, above the eight-dependency budget.
Model identity and settings now share one memoized request value with their original input dependencies.
Creation, start, and regeneration retain their existing request contracts.
The transport regression also changes the picked model and project before regeneration.
It verifies that regeneration sends the new identity and settings together.
Three focused suites pass all 73 tests without skips.
Full complexity budgets, type checking, and owning lint pass.
These local Web checks use the existing dependency cache described above.

Four synthetic artifact fixtures exceed their thirty-day verification window.
Their bodies match the current Main bucket, object-list, upload, and download handlers.
Six isolated handler checks pass without skips.
The fixture metadata records this source verification and retains the synthetic marker.
No response body changes or live backend recording occurs.
The freshness gate passes all 22 fixtures.

Project Context Save also exposes a cache race while its refreshed GET remains pending.
The synchronization effect copies the previous query body after the successful Save clears its dirty guard.
The correction installs the successful PUT response through the generated GET query key before invalidation.
Its controlled regression fails before correction and passes afterward.
All eleven paired tests, type checking, and owning lint pass.

Chromium and WebKit preserve the saved buffer during a held refetch and two oversized Markdown import refusals.
Both isolated probes use full generated response schemas and exact request checks.
They report zero unknown requests, page errors, or console errors; root reviews both final screenshots.
See [the save cache mapping](../../../../docs/source-mapping/project-context-save-cache-20261006.md) for the unchanged import and permission contracts.
Full AppShell project switching and backend persistence remain separate verification boundaries.

The saved-version reset has two defects.
It excludes authored two-node graphs from fitting and acknowledges before the delayed persistence callback.
Acknowledgement clears the reset flag, which cancels that callback through effect cleanup.
The correction acknowledges after persistence and fitting, and fits authored two-node graphs.
The existing delay, external reset cancellation, unmount cleanup, and placeholder behavior remain.
Five focused files pass all 75 tests without skips; type checking and owning lint pass.

Chromium and WebKit each pass the saved-and-cleared interrupt roundtrip with ordinary pointer actions.
Each browser records two exact version writes and two separate application metadata writes.
No manual Fit View, forced clicks, unknown requests, page errors, or console errors occur.
This isolated production-editor proof does not establish backend persistence or deployment.

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

The initial CI failures belong to commit `94e8b11704d5e569a4cf42217d2edd590ec2772b`.
Replacement commit `d14074ee8725253ff0ddb4d8b07498d8aa43ac1c` exposes the Compose normalization assertion failure.
Its Code runner checks, required Editor Test lifecycle, compilation retention checks, and Helm install smoke test pass in CI.
Its Worker PostgreSQL job also exposes two graph receipt fixture failures.
The test step omits required-mode and disposable-database admission flags.
Both fixtures pass locally with those flags against a fresh owned PostgreSQL 18 server, without skips or stack overrides.
The fixture is removed after verification.
See [the graph receipt mapping](graph-receipt-postgres-ci.md) for the unchanged checkpoint and test boundaries.
The repairs preserve the existing resource limits and refusal assertions.
No new test skips, increased thread stacks, or relaxed visual thresholds are introduced.
The replacement head still requires the complete Linux CI matrix.
The added enabled pause-control baseline still requires comparison in replacement CI.
Commit `9cca2bb23ce2e1c1e40f253b3eb4709c769f233c` passes all three Helm jobs and the committed-binary gate.
Its Rust PostgreSQL job progresses past graph receipts, then fails three sandbox deadline cases.
Fresh test databases omit Main's recovery migration `0013`.
The repaired fixtures apply that migration without changing production code or migration ownership.
All 35 required deadline, preparation, and hydration cases pass against an isolated PostgreSQL 18 server.
The run uses default test concurrency and no stack or wait-bound override.
The fixture is removed after verification.
Three real-Docker recovery/submission cases share two updated state setup locations but remain unrun locally.
The native macOS debug link emits the existing compact-unwind size warning; full Linux CI remains required.
See [the fixture mapping](code-ci-deadline-fixtures-20261006.md) for exact schema owners and proof boundaries.
The WebKit journey reports a pointer obstruction while clearing a saved pause control.
The isolated WebKit fixture reproduces the obstruction; the normal Fit View control restores pointer access.
The saved-version reset replaces node positions but excludes an authored two-node graph from automatic fitting.
The focused reset correction and browser proof above address that obstruction.
Full browser acceptance remains required.

The same commit's Web unit shard then reports two Code debug live-case timeouts and a restoration read-count failure.
The local baseline passes; it does not reproduce the CI timeout.
Repeated character delivery updates the entire mounted chat tree for each input event.
The test keeps real focus and a first keyboard character, then pastes the remaining exact prompt.
Its handlers verify the full prompt, conversation identity, execution stream URL, feedback identity, and trace group.
Opening still requires zero artifact reads; downloading requires exactly one verified Blob read.

Four focused files pass 41 tests without skips; type checking and owning lint pass.
The subsequent read-count mismatch remains consistent with timed-out test overlap, rather than a reproduced production defect.
Exact-lock unit acceptance remains required.
Read the resulting CI state before declaring this repair complete.

The complete `9cca2bb23` matrix finishes with 63 successful checks, five failures, and four skips.
All Helm jobs, image scans, Main checks, and native Rust chat journeys pass.
The five failed jobs belong to sandbox fixture schemas, Web static gates, one debug unit shard, and two WebKit shards.
The source corrections above address their reported failures; replacement CI remains required.

Coverage merge skips because a unit shard fails.
The other skipped jobs are conditional contract parity, live toolkit/image credentials, and documentation screenshot capture.
They do not establish runtime acceptance.
Two WebKit cases also pass only after retry: configuration-form mounting and toolkit catalogue retry.
Their transient failures are retained without source changes or relaxed assertions.

The live rehearsal services and databases remain unchanged during these source checks.
The earlier migration-transition proposal has no runtime proof.
Fresh deployment and mandatory browser acceptance remain open.

## Completed replacement matrix

Commit `33880427e24e1beb3ae8abb5e041dd9d6c773858` finishes with 67 successful checks, two failures, and three skips.
Replacement CI clears the previous sandbox fixture, Web unit, static, and Project Context Save failures.
Rust release, PostgreSQL, Runner, image scans, Helm, Main, Web quality, coverage, and visual checks pass.
The complete Web unit suite passes 16,538 tests and retains seven existing expected failures.
The two failures belong to separate WebKit journey shards.
Conditional contract parity, live toolkit credentials, and documentation screenshot capture remain skipped.
Those skips provide no acceptance result.

The pause-control case validates its saved identities, then cannot clear a checkbox with an ordinary pointer action.
Its trace identifies the fixed `Toggle Interactivity` control as the interceptor on all three attempts.
The earlier reset correction remains necessary, but default fit padding still permits this narrow-canvas overlap.
Automatic fitting now measures the fixed control column and reserves its right edge plus a twelve-pixel gap.
Wide-canvas fitting, vertical padding, missing measurements, reset timing, and persisted node identities retain their existing behavior.
The journey retains its assertions, pointer actions, timeout, and retry policy.

Four geometry regressions use React Flow's viewport calculation, including the 161-pixel canvas from CI.
The six root verification suites pass 55 tests without skips.
The independent candidate check passes 67 tests across its six selected suites.
Full type checking, full lint, complexity budgets, layer checks, and dead-code checks pass.
Chromium and WebKit each complete eight geometry checks, two version writes, and two separate metadata writes.
Both isolated browsers save, reload, clear, save again, and reload with ordinary pointer actions.
Both report zero unknown requests, page errors, and console errors; the central command returns exit zero.
The screenshot review confirms that node controls clear the fixed control column.
This production-editor fixture does not establish full AppShell or backend persistence acceptance.

The Logout case navigates to Profile after cookie authentication but before the signed-in shell mounts.
The failing trace shows an empty application root and cancelled module loads during the document change.
WebKit then reports an entry-module failure in the Profile document.
The test now waits for the existing signed-in sidebar control before its unchanged Profile deep link.
The test retains actual Logout, both storage sweeps, surviving control keys, and unauthenticated-session assertions.
Its sixty-two journey-shape checks pass without skips.
Chromium and WebKit each pass a cookie-only control and the guarded full-shell case; the central command returns exit zero.
The guarded cases hold session, author, and lazy-shell responses to prove the readiness boundary before Profile navigation.
Both use the compiled shipping AppShell and Profile, then execute the actual Logout control and storage sweep.
Both retain zero unknown requests, external requests, and page errors.
The unguarded WebKit control reproduces the exact cancelled lazy-module import and the router's recorded recovery reload.
Its narrow expected interruption applies only to that control; the guarded assertions remain unchanged.
Seven owning response schemas validate the explicit synthetic backend fixtures.
Missing author and budget fixtures fail earlier attempts; those receipts remain preserved without weakening the request fence.
This proof does not establish real OIDC authentication or server-side session revocation.
Replacement full-application CI remains required for both corrections.

## Shipping image preparation

The four Linux ARM64 shipping images build from a frozen source snapshot of `33880427e`.
Main uses user `65532`; Worker and Supervisor use `10001:10001`.
The Supervisor image is 29,626,836 bytes, and the Worker image is 48,291,961 bytes.
The first Supervisor build exceeds the isolated compiler's eight-GiB limit.
The unchanged release recipe succeeds with twelve GiB and one CPU within the approved sixteen-GiB Docker budget.
This failure belongs to compilation; it does not establish a runtime resource failure.
The source manifest and image receipts remain separate from deployment proof.
The later Web correction requires a new Web build before deployment.
Running services and database history remain unchanged.
