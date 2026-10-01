# Main integration and release preparation

Date: 2026-10-01. Gate 5 remains active.

The feature parent is `e13a4e4ad42e3b14ff84b1140ee6146938c375f1`.
The incoming main revision is `8563c2d75`.
This integration prepares PR 883 for release. Unfinished dependency preparation belongs to the next PR.

## Source and ownership mapping

The [Code isolation assessment](code-node-isolation-assessment-20260928.md) maps current SDK behavior to Rust execution.
The [crash continuation mapping](agent-crash-continuation.md) defines checkpoint and recovery ownership.
Current platform code remains a business reference. It does not define the new admission or recovery implementation.

| Incoming main owner | Preserved Rust contract | Integration decision |
| --- | --- | --- |
| Main `internal/infra/db/repos/agent_execution_jobs.go` and `internal/db/queries/runtime_agent_execution.sql` | Claims bind admitted execution identities and immutable inputs. | Retain the short reservation transaction and separate materialization transaction. Regenerate both query sets together. |
| Main `internal/runtimecomposition/agent_admission_reservation_reaper.go` | Rust owns graph checkpoints and model sessions. | Reclaim leaked admission slots without moving checkpoint ownership to Main. |
| Python `transport/reconnect_channel.py` and `execution/delivery.py` | Replacement workers recover through persisted claim and checkpoint authority. | Retain main's reconnect and delivery-cap changes. Audit Rust separately. |
| Web `useChatBoxData.seed.ts` | Live execution output survives transcript refresh. | Retain main's protection against replacing a streaming transcript with an older seed. |
| Docker and Helm object initialization | Sandbox artifacts use the admitted object-storage service. | Retain RustFS readiness and CLI initialization changes. |

Generated `querier.go` contains both toolkit-read operations and admission-reservation operations.
Use sqlc 1.31.1 to generate and vet the merged query sources.
Do not resolve generated conflicts by hand.

Materialization again reads `LoadRuntimeAdmissionTiming` from PostgreSQL.
Publication and expiry use the same database clock.
The incoming host-clock optimization can change deadlines when Main replicas have different clocks.
The timing regression verifies stored timestamps, exact TTL, publication eligibility, and replay with both caller-clock directions.
The fix restores one database round trip. No latency measurement is claimed.

## Migration collision

Main owns shared migration 126 for admission reservations.
Feature migrations 126 through 132 move to 127 through 133.
All seven SQL files retain their exact bytes.
The shared head becomes 133. The tenant head remains 138. The agentstate head is 9.
This merge adds no replacement application schema.

| Previous feature filename | Current filename | Unchanged SHA-256 |
| --- | --- | --- |
| `0126_toolkit_execute_read.sql` | `0127_toolkit_execute_read.sql` | `c2497df59b29fec9230c0164e2e1e44d9c1f5d0d0bf08d7ae1dfd0aea4535e71` |
| `0127_mcp_prebuilt_parameter_schema.sql` | `0128_mcp_prebuilt_parameter_schema.sql` | `e0d3ebd4b3c7d03810a93d3e824d9a391ff2c870cbd79832c6cdca1028e86289` |
| `0128_mcp_oauth_clients.sql` | `0129_mcp_oauth_clients.sql` | `62bf7e4c22d2bba3375b216fd578aa1fdb57b1243dada093126d2076b8445515` |
| `0129_toolkit_available_tools.sql` | `0130_toolkit_available_tools.sql` | `a7250a118531f6a9cb39b15790cb8e7d7f62a485e1cbd324dee83bfc1389a913` |
| `0130_mcp_oauth_tokens.sql` | `0131_mcp_oauth_tokens.sql` | `88ce87eb8b489b33fe2ca72936323fb6a2b99198d403ff85e7bfbf560d7ada37` |
| `0131_agent_model_checkpoint_claim.sql` | `0132_agent_model_checkpoint_claim.sql` | `ba861ea8f7752e5f8fd14ff64065b211a89208676fbca30b1b151fd5e4ca5a86` |
| `0132_agent_full_context_input.sql` | `0133_agent_full_context_input.sql` | `c105f0dc25dde00168bc26f0b073378601e706bbbcd86afa98959b0f2c14c476` |

Historical mappings retain their original filenames. Use this table for the current source paths.
Integration fixtures now read the current filenames.
The ledger regression accepts main's prefix through 126 and rejects unreconciled feature toolkit execution at 126.

No database or ledger changes occur during this merge.
Hold rehearsal deployment until its actual ledger is inspected and explicitly reconciled.
Do not disable checksum validation or rerun applied SQL to bypass a mismatch.

## CI corrections

The resolved merge is pushed as `f76fa006f`. GitHub reports it as mergeable.
Fresh checks run against this head and expose additional release repairs.
Those checks, rather than the older synthetic merge failures, determine release readiness.

The catalogue drift test now reads the implemented `ASK_USER_TOOL_NAME` constant.
The agentstate manifest assertion now includes the committed sandbox history through migration 9.
Both corrections retain strict catalogue and manifest validation.

Dependency scanning now covers the supervisor and compiled Rust adapter manifests.
Patched ADK manifests use the worker lockfile's dependency closure.
Their scanner exemptions explain why upstream source and patches move together.
The data-processing manifest remains a fixed acceptance fixture.

Fresh CI corrections preserve the production contracts:

| Failure | Owning source and correction | Proof |
| --- | --- | --- |
| Helm treats disabled sandbox grants as an invalid empty setting. | `deploy/helm/tests/render-capabilities.sh` now distinguishes empty disabled audiences from exact active identities. Main `ConfigFromEnv`, `validateSandboxAudiences`, and grant-issuer construction already implement this distinction. | Three sandbox rendering suites, three chart lint commands, and `TestSandboxGrantAudiencesAreOptionalExactAndBounded` pass. Production defaults and validation remain unchanged. |
| Binary guard rejects public peer-certificate fixtures. | `scripts/binary-allowlist.txt` names the five exact DER paths used by Rust `sandbox/peer_identity.rs`. The fixtures contain public certificates; their private keys were discarded. | The complete binary guard passes. No broad certificate or binary wildcard is added. |
| Python image resolver selects cryptography 50.0.2 although the artifact lock pins 50.0.1. | Python `Containerfile` exports the existing 23 exact pins through `verify_locked_artifacts.py` before dependency resolution. Filename and digest verification remain mandatory. | Twelve focused tests pass. All 22 wheels for both Linux architectures and the locked source archive match their existing SHA-256 values. The lock bytes are unchanged. Full shipping-image proof remains in CI. |
| Four DeepWiki fixtures exceed the 30-day freshness window. | The UI fixtures are explicitly identified as synthetic contract data and revalidated. The generation recorder executes source revision `ce679f11`; all five source digests, manifest projection, layout, and listed page identities match. | Twenty-two fixture, browser-list, and page-view tests pass. All 22 Web fixtures pass the unchanged freshness guard. These checks are not a new live HTTP capture. |
| Web model selection displays None for real numeric project identities. | Main `CurrentModelCatalogItem.ProjectID` emits an integer. `useChatBoxModelSelection::matchesModelProject` normalizes both primitive identities with `String`, retaining refusal of nonprimitive settings. | Nine focused hook tests pass, including numeric IDs supplied by the actual HTTP query path. This restores the configured model; the failing chat screenshots are not accepted as new references. |
| Branding export cannot be imported by its own dry-run endpoint. | `brandpackage.Service.Export` inlines the pack into a compiled app preview larger than the former 1 MiB entry cap. Import and export now share a bounded 2 MiB cap for `preview/app.html`. | The original 1029 KiB failure reproduces. Native export/import and HTTP multipart round-trip pass, including exact-limit acceptance and max-plus-one refusal. Other preview, pack, asset, and aggregate caps remain unchanged. |
| Fixture cleanup reports no pipelines. | `e2e/fixtures/api.ts` now requests classic agents explicitly before its separate pipeline sweep. Main's unfiltered application list legitimately includes both classes. | The unit proof and API journey retain per-class removal and unrelated-row preservation assertions. The branding surface journey opens the real `/app/chat` route to avoid competing redirects. Deployed CI proof remains required. |

The Linux sensitive-HITL test overflows its thread stack. Investigation identifies one unboxed
`close_no_ack` future in `native_agent_lifecycle::execute_started`; the other terminal paths already box it.
The correction boxes that same ownership boundary and retains the pause, acknowledgement, and skipped-completion assertions.
A dedicated 2 MiB test thread guards the stack budget without increasing production or global test stacks.
The 67 native output-delivery tests and strict all-feature library/test Clippy checks pass.
The focused Linux arm64 test passes with Rust 1.97.1 and the unoptimized debug profile.
Its explicit 2 MiB thread preserves the CI stack budget. Final x86_64 CI confirmation remains required.

The Rust image scan finds system `libssl3` in the distroless C++ base, although the worker's
HTTP, SQL, RPC, telemetry, and Kubernetes clients use rustls and have no native OpenSSL dependencies.
The runtime now uses Debian's TLS-free distroless base with the required GCC runtime library.
Its package records, checksums, and licenses remain available to scanners.
Build-time checks resolve both shipped ELF library closures against the final runtime and verify
CA roots, audit metadata, debug line tables, and symbols. The blocking vulnerability policy remains unchanged.
Cached arm64 worker and supervisor binaries pass the library, checksum, license, and diagnostic checks.
Trivy 0.72.0 detects eight Debian packages and 299 Rust package records in the resulting image.
The scan reports zero HIGH/CRITICAL findings. The unchanged image scan gate passes.
The image identity is `sha256:dc891e7f64922f6e5ffddf7be1600ab41fc13ac15c15e6c83ae613c3d1f50a01`.
This cached arm64 proof does not replace final shipping-image and x86_64 CI checks.

Seven visual references now match intentional controls introduced by earlier source changes.
The admin references include Default Secrets from `a2f5be525`.
The personalization references include Balanced and Full context controls from `95fbc6b96`.
The pipeline references include model settings and send controls from `26ba7bbd1`.
The references come from artifact `11172076649`, produced with Playwright `v1.62.1-noble`.
Its archive SHA-256 is `81ef521d5f37ca7148916263a60490e5c6844e3138a92d3ed7799149dd867b00`.
All seven references produce zero comparator differences across retries at the existing threshold.
Two empty-pipeline references have small raw raster differences. They are not byte-identical across retries.
The four chat references remain unchanged because numeric model selection requires a code repair.
No comparison thresholds, masks, or coverage rules change.
All 59 references use 6.56 MiB of the 12 MiB budget. The binary guard passes for 10,564 files.

Release repair `ecb6a3277` is pushed. The fresh Helm run reaches all eight worker assertions successfully.
It then fails because `render-worker-sandbox.sh` uses ripgrep, which is absent from the CI environment.
The literal assertion now uses standard `grep -Fq`. Chart behavior and assertion scope remain unchanged.
Verification runs with ripgrep excluded from the command path.
The worker suite passes eight assertions. All thirteen scripts called by the lint job have no remaining ripgrep calls.
Dependency-backed lint passes all three charts with the pinned NATS dependency in a temporary chart copy.
Local lint uses Helm 4.1.0. The fresh CI check uses Helm 3.16.0.

Project Context returns authoritative identity, revision time, and activation description in addition to content and enabled state.
Its browser test now checks the exact five-field response and preserves the previous identity and omitted activation description.
The HITL route test previously accepted a resolved review card as the completed continuation.
Its journal read then observed a previous turn before the current model request arrived.
The test now waits for the finalized continuation row and matches the exact project credential and user token.
It retains exact resume-action and route checks. Production routing remains unchanged.
Affected lint and Playwright collection pass. Fresh browser CI must confirm both repairs.

Native sensitive-tool resumes use `hitl_decisions` with exact interrupt and tool-call identities.
The toolkit browser test now checks those complete decisions. It preserves the SDK's existing root-action branch.
Native tool notices use the call identity `elitea-skipped-internal-tools`.
The mock previously treated every current-turn tool message as completion of the requested call.
It now matches the emitted call identity and preserves legacy function-name matching.
Thirteen mock tests pass, including unrelated notices, successful results, denials, errors, and repeated operation names.
No Swarm behavior or browser assertion changes.

The fresh-browser smoke test also exposes a send-path model-selection defect.
The picker updates `llmSettings`, but the send path overwrites its model name with the previous default.
Main `currentAdhocSnapshot` overlays that incorrect request value onto the persisted dummy settings.
`useChatBoxSend` now selects one authoritative name from configured model settings before the default fallback.
Conversation creation, initial requests, and regeneration use that name and preserve the configured model project identity.
The actual HTTP regression checks participant settings, initial requests, and regeneration.
Pipeline callback ownership remains unchanged.

Main `ConversationsRepo.Create` inserts supplied participants before the automatic user and blank dummy participants.
`addConversationParticipant` matches the existing dummy and uses `ON CONFLICT DO NOTHING` for its conversation mapping.
The former follow-up participant request therefore discards the newly selected model settings.
The creation adapter now supplies those settings in the initial request and removes the redundant follow-up request.
The lifecycle adapter passes through the existing participants contract. No application schema or Main behavior changes.
Fifty-three focused tests pass, including terminal-refresh restoration and the actual creation, start, and regeneration HTTP paths.
Owned lint, full Web typecheck, and all five production build targets pass.
The deployed browser creates conversation 778 with Haiku selected before the first message.
The model remains selected after completion, reload, regeneration, and a second reload.
The transcript retains one final answer. No new browser error appears during this check.
An earlier notification-stream warning remains in the session. Chat streaming and model persistence pass independently.

## Browser contract repairs

Fresh CI at `3c10d0666` passes all 1,453 Rust tests, strict Clippy, and the release build.
The Linux AMD64 shipping-image scan reports zero HIGH/CRITICAL findings.
Helm Lint, Helm Template, and the binary-file guard pass in GitHub CI.
These results cover that revision. Later browser repairs require new CI results.

Browser artifacts expose three product defects and several obsolete test assumptions.
The repairs preserve exact identities, authorization refusals, persisted values, and request readbacks.
They do not remove failing journeys or relax visual thresholds.

| Current source or contract | New implementation | Verification |
| --- | --- | --- |
| Core `rpc/chat_conversation.py:194-198` permits creator-only public-to-private changes. | Main `repos/conversations.go` checks the trusted actor and enforces the author condition atomically in SQL. | Nine PostgreSQL authority tests pass normally and with the race detector. Membership, grants, token ownership, and private visibility remain enforced. |
| Social `models/pins.py` defines one project pin per entity. Core `api/v2/folder.py:396-420` reads those shared pins. | Both Main pin route families use `repos/social_pins.go`. The folder reader uses the same `centry.social_pins` table. | Fourteen PostgreSQL HTTP tests and sixteen race-enabled tests pass. Private-chat visibility and repeated unpin behavior remain enforced. |
| Main participant metadata contains numeric user and project IDs. | Web `ChatBox.helpers.ts` accepts primitive numeric IDs and strings. Names remain strings; objects and unsafe integers remain invalid. | Eighty-one related tests pass. Solo-chat and multi-user browser acceptance pass. |
| Conversation creation automatically includes its author and the default chat participant. | Journey fixtures select participants by exact entity, ID, and project. Duplicate checks preserve every participant and its settings. | Scoped lint, collection, and fixture helper checks pass. Fresh browser execution remains required. |
| Tag discovery returns tags associated with visible entities. The public project forbids publishing private conversations. | Tag fixtures attach their own tags. Positive publish tests use the existing private author project. | Exact association, filtering, deletion, and visibility assertions remain. |
| Balanced and Full settings define the new context policy. | Context journeys check saved policy selection, immutable existing conversations, and unknown pre-run capacity. | The tests retain policy readbacks without restoring the obsolete output-only budget. |
| Canvas authority runs before range validation. | Unowned message groups require the exact non-disclosing 404. An owned group with an inverted range still requires 400. | The owned-message journey also checks that the stored message remains unchanged. |
| Python resumes into the admitted response row. Rust preserves segmented direct HITL history. | The route journey checks the exact Python response ID or the Rust history segment, according to the selected runtime. | Both route decisions and exact model journal checks remain. Thirty related tests pass. |
| Feedback is valid after a successful stored answer. | The feedback journey waits for its exact completed answer before applying reactions. | Exact reaction groups and persisted reaction readbacks remain. |

The Core reference revision is `43a79a7654f9df03feef4859074fbf46eb2b8701`.
The Social reference revision is `f53c2d67f751331f0834c3e67f94d6d58dfdc634`.
These references define business behavior. Main retains its own typed errors, project middleware, and chat authority.
OpenAPI and Web clients reproduce through their existing pinned generators.

The document-canvas unit test now waits for asynchronous editor content before checking its exact text.
Thirty-eight focused canvas tests pass. Three separate four-test repetitions also pass.
No editor behavior changes.

An isolated stack uses freshly built Main, Web, Python worker, gateway, and test services.
The full 23-file WebKit scope passes 89 tests without retries or skips.
The Chromium scope initially passes 87 tests and reports two fixture failures.
Both fixtures now select the exact application participant and resend its persisted version settings.
All 16 checks in the focused Chromium rerun pass.
The browser also verifies pin, server listing, reload, one visible row, and unpin without page errors.
The complete six-package Main test run passes against an isolated PostgreSQL database.
Both generated API clients reproduce through their pinned generators. The deployment edge tests pass.

The Python all-tools browser case initially ends with `DEPENDENCY_UNAVAILABLE` after successful admission.
Safe Python diagnostics locate the failure at Main's scoped input-materialization response, before SDK construction.
The frozen internal MCP references are valid. The executing actor has an active PAT.
Both agent and index dispatch are enabled in the isolated deployment. The selected worker is Python.
`internal/runtimecomposition/composition.go` incorrectly selects the index-only materializer unless the toolkit route uses Rust.
That materializer has no prebuilt MCP resolver. It rejects the agent's frozen MCP references.
The shared listener now selects the agent materializer whenever agent execution supplies it, for either worker implementation.
The index-only fallback remains available when agent execution is disabled.
Main logs fixed boundary, status, generation, and execution fields without private inputs or raw error text.
Storage and composition package tests pass. Vet passes. Python diagnostic tests pass all thirteen cases.
The unchanged browser retest passes input materialization and reaches SDK MCP construction.
The SDK then rejects internal MCP names because it expects `server_config`, rather than the flat materialized URL and headers.
The Python adapter now supplies the pinned SDK's explicit HTTP `server_config` from Main's materialized settings.
Application-version, application-extra, and ad-hoc tools retain their IDs, names, filters, and metadata.
The adapter copies inputs and refuses unresolved prebuilt references rather than reading a local MCP configuration.
Tests execute the installed SDK's constructor routing with the network transport replaced by a fixture.
All 132 focused checks pass. One unrelated real-sandbox dependency test is deselected in this offline probe.
The unchanged all-tools browser case now passes on the fresh Main and Python adapter images.
The run reports ten passes, including authentication and fixture checks, without retries.
The agent stores every internal-tool toggle and returns a nonempty persisted answer.

An intermediate local retest selects `gpt-4o-mini` and receives a provider 401.
The isolated seed command inherits an ambient provider key and defaults to OpenAI mode.
The immutable execution input already contains that model before SDK construction. The SDK does not replace the selected model.
The operator explicitly seeds mock mode without ambient provider keys and removes the exact stale test configurations through product APIs.
Both isolated projects then expose only `vllm/E2E-MOCK-MODEL` as their admitted chat model.
No centry or rehearsal provider configuration changes.

Fresh Python browser checks also pass HITL approve/reject routes, persisted message feedback, and canvas extraction.
The run reports twelve passes, including authentication and fixture checks, without retries.

The Python sensitive-tool fixture advertises HTTPS for its plain HTTP mock listener.
The base compose file now advertises the existing HTTP listener.
The native overlay retains its existing HTTPS schema contract.
No production TLS policy changes.

The pinned SDK also adds an empty header map to omitted arguments before checkpoint matching.
Its matcher then replaces the original model call identity during resume.
Python fixture markers explicitly send the supported nullable header argument to preserve that identity.
Native markers remain unchanged. This fixture correction does not repair the SDK's omitted-default defect.
The SDK reference revision is `b5113a129329b85d23c2d5c2bf55f18e307414ec`.
Its `elitea_sdk/runtime/langchain/langraph_agent.py:3049` matches checkpoints, and lines 2012-2029 construct replacement resume calls.
The argument normalizer is `elitea_sdk/runtime/langchain/utils.py:74-102`.
Fresh sensitive-tool browser proof remains required.

Builder journeys now verify exact application, project, version, participant, and admitted execution identities.
Persistence polling reports bounded terminal categories without exposing raw tool results.
Existing persistence assertions and deadlines remain unchanged.
Typecheck, scoped lint, collection, and 60 journey-shape tests pass. Native browser proof remains required.

## Preserved Point 5 work

The pre-merge checkpoint contains 81 tracked and untracked files.
A local preservation manifest verifies each file against its saved SHA-256.
Retain the checkpoint until restoration and verification finish on the follow-up branch.
The restoration audit verifies 48 tracked files and 33 new files against the checkpoint.
Three paths need reconciliation: Web locale text, worker runtime settings, and the internal-tools catalogue test.
Keep current release additions when applying their saved changes. Exclude the saved whitespace-only handler changes.

The checkpoint includes Python preparation, indexed content delivery, inert hydration, dispatch fencing, and deployment controls.
Focused verification passes before preservation:

- Strict feature Clippy passes.
- Sandbox tests report 66 passes and 20 ignored infrastructure tests.
- Code tests report 37 passes.
- An isolated PostgreSQL probe reports 14 recovery-test passes.
- The real Docker transfer probe reports one pass and removes its owned containers.

These results belong to the preserved checkpoint, not the release candidate.
On-demand deployment and UI acceptance remain open.
JavaScript, TypeScript, and Rust native preparation have component evidence.
Their shared-storage integration remains open.
Transfer timing and final-dispatch budget separation also remain open.

## Release acceptance

Sensitive-call resume now follows the current SDK's per-invocation policy.
SDK `sensitive_tool_guard.py` at `b5113a129` requires a separate decision for every sensitive invocation, including identical arguments.
Rust `ResolvedDirectHitlDecision::into_direct_replay` previously copied one decision to undecided same-name sibling calls.
It now preserves only exact settled decisions and leaves each undecided sibling for ADK confirmation.
Sixteen focused tests and the persisted replay regression pass. Strict Clippy and formatting pass.
ADK Runner tests verify two distinct pauses, exact result identities, denial isolation, and one execution per approved call.

The deployed Python browser also exposes a separate SDK resume defect.
After two approvals, retained checkpoints contain only the first identical call and its result.
SDK `LLMNode::_build_resume_completion` selects the first name-and-argument match, even when that call already has a result.
The fix selects an unfinished matching call and preserves the original call identities.
Completed-call replay behavior and sensitive-tool policy remain unchanged.
SDK draft PR `EliteaAI/elitea-sdk#679` publishes patch `9a0614571a693a86b497b0502456cf32745d530e`.
The worker retains its existing SDK version and four patches, then admits this fifth immutable patch.
Its 481-file package digest becomes `42cd111fbb59fdb82406f853718f8033561369624c5d66eb10350dd58ff2f1c3`.
Thirteen regressions pass with the installed shipping dependency closure, including a rebuilt MemorySaver graph.
The admitted five-patch source passes 43 contract and lock checks without skips.
All four source projection checks pass. Gateway conformance passes seven tier-one tests and 29 tier-two assertions.
The Gateway pin preserves both measured file digests. Its SDK patch list matches the worker lock.
Fresh deployed browser checks remain required for both corrected runtimes.

Native builder CI also identifies a Main skill-author regression.
`createSkillSQL` separates owner and author in source commit `171d1bedb3`.
The runtime skill repository still passes three arguments and uses the project owner as the version author.
The existing claim authorization now supplies its durable actor through `RuntimeSkillSink`.
The repository binds that actor as the fourth argument and keeps the owning project separate.
Request-supplied actors, invalid actors, and unauthorized claims cannot write.
Real PostgreSQL creation uses owner 1 and author 11. An update by actor 17 preserves both original authors and tags.
Focused HTTP, claim, and repository tests pass without skips. Storage tests, vet, and formatting pass.

Fresh Go CI rejects stale generated OpenAPI descriptions for both social pin operations.
The source descriptions include current line references; the embedded generated specification still contains the previous text.
The pinned generator runs with CI's Go 1.25.14 toolchain.
Its output matches the CI diff exactly and reproduces byte-for-byte on a second run.
Decoded specifications differ only in the two operation descriptions.
API paths, types, validation, and runtime behavior remain unchanged.

Native browser fixtures now use the current execution contracts.
HITL continuation carries the exact interrupt and tool-call identities with the selected answer.
MCP authorization uses the declared authorization proxy because protected tools remain hidden until authorization succeeds.
Same-name MCP tools retain their toolkit-qualified identities.
Project Context writes use the internal MCP tool `put_prompt_lib_project-context` and verify its JSON receipt.
Skills writes retain the native builder tool and verify persisted instructions.
Both runtimes require two decisions and two exact results for two sensitive invocations.
These fixture changes preserve persistence, project isolation, and actual tool execution assertions.

Local candidate checks pass:

- All eleven Go workspace modules pass tests, vet, and pinned strict lint.
- Isolated PostgreSQL 18, pgvector 0.8.1, and Redis tests execute 13,762 cases. Thirty-seven infrastructure cases skip.
- Rust runs 1,453 tests successfully. Nineteen infrastructure tests remain ignored.
- Strict Rust Clippy passes for all targets and features.
- Fifty-nine focused Python reconnect and execution tests pass.
- Web lint, typecheck, budgets, translation, cycle, dead-code, and contract checks pass.
- The full Web suite passes 15,440 tests and exposes two stale Code-node assertions.
- Both assertions are corrected. The twenty focused allow-list tests pass after correction.
- The production Web application builds. SQLC and OpenAPI outputs reproduce through their pinned generators.
- Helm lint, chart rendering, dependency coverage, and sixty-six drain-script assertions pass.

The new Web bundle is deployed into the existing rehearsal Web container.
A fresh browser verifies the ten supported node types and all four Code language controls.
Pipeline 141 initially fails at Python during its editor Test run. The UI displays the typed failure and support reference.
The rehearsal profiles selected older runtime images without the benchmark's prepared package closure.
The worker and supervisor now select the matching immutable Deno and Rust images.
Only those image identities change; policies, TLS, audiences, limits, and Main remain unchanged.
The fresh persistent-chat regeneration passes all four languages and returns the expected hash
`6a3c4d063a5ba0c21949cf7d67d4bda843cb50a0bc76169c19e2c5f7e272326d`.
Reload preserves one final result, including the accepted/rejected counts and total.
The fresh editor Test chat also completes all four stages with the same hash.
The browser reports no console errors. The failed receipt remains unchanged.
This check uses existing rehearsal Main and worker binaries. It does not prove deployment of the merged Main.

The fresh browser checks pass on the existing rehearsal backend.
Fresh GitHub CI remains required before release, including x86_64 stack and shipping-image checks.
Do not treat preserved checkpoint tests as merged-candidate evidence.
Do not close Gate 5 when PR 883 merges.
