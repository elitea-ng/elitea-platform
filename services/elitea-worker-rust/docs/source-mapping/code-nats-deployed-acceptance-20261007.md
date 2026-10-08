# Deployed Code acceptance on NATS

## Source and delivery boundary

The isolated candidate uses source `efa7213e803d6f314ee8b6c482b78f45544140b4`.
This merge retains Main `781bf6ece` and the existing graph extensions.
The candidate uses fresh execution state and private NATS, storage, and runtime material.
Historical pending work and original services remain unchanged.

All seven application images build from this source and pass their applicable scan gates.
The Python gate retains its committed vulnerability exception: 105 HIGH and three CRITICAL findings.
This exception is not a vulnerability-free result or a new user acceptance.
The local scanner uses Trivy 0.72; CI uses 0.74.
Only Main, Web, Worker, and Supervisor have current candidate deployment evidence.
The Gateway, Scheduler, and Python builds do not prove their runtime acceptance.

The Supervisor build passes one bounded 9 GiB trial with the authorized 16 GiB Docker VM.
The builder returns to its 8 GiB limit after this trial.
The earlier 8 GiB failure remains preserved.
Source resolver policies do not prove destination enforcement on resolver networks.

The following table records the earlier efa deployment.
The later c53 native image and Worker successor boundaries appear in the [recovery record](code-failed-stop-owner-recovery-20261007.md).

| Earlier deployed role | Exact image SHA-256 |
| --- | --- |
| Main | `2ad1810c426b8cb8846a2988e2dfb063e7a71046d002bdb097b524a345a55074` |
| Web | `1c343b749892ac042f5389e4018432e21776d97a902e9b0eb7e8fc18adb91714` |
| Worker | `366bd20fc64333703e6842605def77239e2ea70ecaaa1d2fa6d3aa90769ab4ec` |
| Supervisor | `74ac4fa096e95467decc17bb10bdfb6152af1633bcfdbdbab6cd3033b411f447` |

## Behavior and ownership

The current application remains the behavior reference.
The existing [Code mappings](code-terminal-failure-20261006.md) retain current-to-new source ownership and implementation history.
The [NATS mapping](main-nats-integration-20261006.md) retains transport and storage history.

| Required behavior | Current implementation owner | Deployed evidence |
| --- | --- | --- |
| Selected state and language execution | `src/agents/graph/code_runtime.rs`, `src/agents/graph/code_preparation.rs`, and `src/sandbox/preparation.rs` | Chat 826 completes four languages and the exact 20,000-row oracle. |
| Safe failure and downstream suppression | `src/agents/graph/code_workspace_failure.rs` and Main terminal projection | Chats 827 and 830 save failure journals before publication. |
| Stop the original running Code job | Main `internal/db/queries/agent_cancel.sql` and Supervisor cancellation | Chat 831 records `sandbox.cancelled` before the execution deadline. |
| Preserve cancelled-turn history policy | Main cancellation transaction and CANCELLED projection | Main removes an empty persistent turn; durable cancellation and settlement remain recorded. |
| Export resolved source and selected input | `src/agents/graph/code_debug.rs` and Main `internal/infra/storage/runtime_code_debug.go` | Chats 832 and 833 prove default-off and explicit-on behavior. |
| Read the immutable debug reference | Web `src/shared/lib/readCodeDebugArtifact.ts` and Main artifact access | The authenticated artifact preview returns the exact synthetic snapshot. |
| Restore the original visit and writer | Main original Code visits and AgentState checkpoint writers | Metadata correlates the original claim, thread, activation, request, and committed reference. |

Paths under `src/` refer to this Rust service.
Main paths refer to `services/elitea-main/`.
Web paths refer to `apps/elitea-web/`.

## Accepted deployed cases

| Case | Result and proof boundary | Frozen root receipt SHA-256 |
| --- | --- | --- |
| Chat 826, four languages | Exact live, persisted, and reloaded oracle; eight runtimes meet their guards and are removed. | `57cc3e8a17bccba880016b73981c5509d3e6782bddcbec91347c9b426fe631d1` |
| Chat 829, platform reads | Eight distinct reads commit and the final answer survives reload. Runtime capture fails its actor-type guard. | `d8b4d6254084341f9cfb18c910885c65825140c9a80456de8f04c47bccbc3e93` |
| Chat 827, empty source | Typed failure, one settlement, support reference, and no sandbox dispatch or downstream work. | `0dcdc18d14a1037e9542150850405fee7ef90c81a5cb48fcc2565e1e99144f6d` |
| Chat 830, failed preparation | The exact safe preparation message survives reload. One preparation runtime fails; no execution runtime starts. | `779b2af30883bafe43bd5f6479056ccb375a3636970b3c45c20fe38b653cb189` |
| Chat 831, active Stop | The original job cancels before its deadline. Two original runtimes are removed; one CANCELLED settlement commits. | `ea96d29c006abf96ffdc4313fb0bd5f330471f4e5142aa4a4c5ca453296ae8fc` |
| Chat 832, debug off | No debug row exists for the exact execution and generation. The answer survives reload. | `6be80aa9026d8e65e7d1de0b10319b08c6b9b68e482d25230a2d034cbfa49e46` |
| Chat 833, debug on | One committed artifact matches the original visit, writer, trace, object metadata, and authenticated UI preview. | `f330a3b92031454e2d924cfe110509fa2b86a01aaef33070044b195aac93c51e` |

The chat 829 capture failure gives no runtime or cleanup acceptance.
Chat 830 records failure class `unknown`; it does not prove the package parser's internal error producer.
The earlier chat 831 attempt reaches its deadline and pauses before Stop.
That attempt remains separate from the successful active Stop case.
The active Stop journal contains `Running`; it does not contain `ControlStopped`.
Reload shows the existing empty-turn policy, not restored answer history.

Debug-on retains base version 173 and creates named version 174 through the normal editor.
The Debug checkbox changes only the semantic `debug: true` field.
UI serialization also changes formatting and key order.
The 297-byte preview matches SHA-256 `c3534056269f85c277194d32101c56251f76e2e43b7bd1f5fc1851842654ddbf`.
Its four fields are `schema_version`, `language`, `source`, and `selected_input`.
The source retains its final newline; selected input contains only `probe_payload`.
Unselected state and framework or credential keys are absent.

The download event capture times out twice and provides no downloaded-file receipt.
The normal authenticated artifact preview provides the byte evidence instead.
One clipboard read after a tool reset returns empty and remains preserved.
Visible DOM text verifies the exact reloaded 50-byte answer against its stored digest.
Actor 3 has central administration authority, although its project-admin flag is false.
The bucket has no actor exception, so default read and write access apply.
These cases do not prove non-admin access or permission refusal.
Retained writer metadata does not grant new publication authority.

## Open completion requirements

Keep Code and Point 5 open.
The later [failed-journal recovery record](code-failed-stop-owner-recovery-20261007.md) closes the exact current-claim failed-journal restoration case.
Explicit human approval authorizes its exact copied-row locks and candidate Worker pause, kill, and fresh-spool replacement.
The accepted case includes live and reloaded UI, normal checkpoint authority, one terminal settlement, and actual runtime removal.
Earlier refusals remain preserved and receive no recovery credit.

Later-request Cargo bundle reuse exposes an existing preparation gap.
The [lookup correction](code-frozen-cargo-lookup-20261007.md) later passes deployed cold, warm, and editor acceptance on the coordinated c53 Worker and Supervisor.
The frozen root receipt is `b8d5aab58fdb00534bf5b2ae09051bd2e04bffe41fbb5ed15dc81dadd5fb6467`.
The exact typed oracle survives persistent reload and editor history restoration.
The same compiled descriptor serves one cold compilation and two later executions.

Preparation Stop, pending Stop across Worker replacement, and the remaining preparation messages stay open.
The later chat 839 fixture completes normally in 14 seconds after its controller refuses before Stop or Worker loss.
The controller records an original-preparation identity mismatch; this run receives no cancellation or recovery credit.
Its earlier helper readback refusal is limited to inherited immutable-image labels. All requested host fields match.
The label correction passes actual fresh-spool initialization and helper removal. The prior refused helper and unused spool remain preserved.
The corrected identity controller later refuses chat 844 during normal Main `PENDING` admission.
The original Worker remains unchanged. No Stop or Worker fault occurs.
The run completes normally in 15 seconds and returns the downstream sentinel 9999.
The actual idle store then contains 59 jobs, 59 dispatches, and 125 checkpoints.
This result receives no preparation-cancellation or recovery credit.
The next controller requires the complete source-defined admission and preparation startup transitions.

The later chat845 controller observes the original preparer running, but the normal UI Stop misses that preparation window.
The first Code job completes before Stop cancels downstream preparation.
The original execution settles as CANCELLED with one output and one settlement.
All three sandbox runtimes later return HTTP 404 and are absent from the runtime list.
The store contains 62 terminal jobs, 62 dispatches, and 130 checkpoints after this run.
No Worker loss occurs. The original Worker and unused fresh spool remain preserved.

This run receives no preparation-Stop or Worker-recovery credit.
The readback digest is `2b60dc93c0155027612eaf0cdf43e0b39ada2223126a2385d910a53ba79a89cd`.
The removal digest is `f9dc33b72c4a5c6157d8a50b9096de2d642f69407e83910cf6e4949255b613a7`.

Chat847 later completes normally after the observer misses the original preparation window.
Its four original jobs complete, and all four runtimes return HTTP 404 with list absence.
The Worker stays unchanged. This run supplies no Stop or recovery proof.
Its root readback digest is `2c9f83a8fdd2833344c46b4a7a4011718d961649d22ce76e436a6cd2e0746606`.

Chat848 uses one continuous normal browser Run and Stop flow.
It settles as CANCELLED before any user Code or downstream job starts.
One projected 28-byte failure contains registered code9 and the exact canonical cancellation message.
One claim1/epoch1 settlement commits, and the original preparation runtime returns HTTP 404 with list absence.
The original preparation completes during the Stop transition; no pending cancellation cut or Worker loss occurs.
Reloaded History retains CANCELLED/TERMINAL, and Restore Test selects the original terminal run.
The root readback digest is `d8bd9093c3ebde4345e46494a4bdeed0d707dd53aa9fb394ab03922903492d6d`.
The browser receipt digest is `bd18f164e4102ddf2400d9c1475fcf75c07fec1b0db6d334af4a6084cce100f9`.
This proves early Stop before user Code. It supplies no pending preparation cancellation or recovery credit.

The live canvas still reports an active run after this durable cancellation.
Chat and History already report cancellation.
The [terminal callback correction](code-live-canvas-terminal-20261008.md) passes 75 focused source tests.
Its shipping image and deployed browser verification remain open.

The source review also confirms four private harness corrections.
Forbidden material mount sources can use an immutable conservative set from full locked preservation.
Runtime, job, claim, phase, and all16 isolation checks must stay current.
Canonical cancellation uses nullable database error columns; its wire payload supplies code9.
Main command and outbox identifiers are distinct. The observer must retain their exact execution/generation join.
Ordinary claimed cancellation leaves the Product outbox retirement flag false and its retirement code null.
Terminal execution state excludes publication and visibility repair. Independent NATS queue observations must prove acknowledgement drain.
These corrections supply no additional application or recovery proof.

Chat849 completes normally after a root browser selector error misses the Stop control.
Its four original jobs complete, and all four runtimes return HTTP 404 with list absence.
The original Worker remains unchanged. The idle store retains71 jobs,71 dispatches, and149 checkpoints.
The root readback digest is `65e3f66b0488d90fcf6a055dff8a21d4607cccd9fdb2e73155e34541294aae33`.
This run receives no Stop or recovery credit.

Chat850 later closes [pending preparation Stop across Worker loss](code-pending-stop-owner-recovery-20261008.md).
The original running preparer has no result at the fault cut, and all16 isolation checks pass.
The fresh-spool replacement settles the same run under claim2/epoch2, with no user Code or downstream job.
One canonical cancellation, one settlement, original runtime removal, and independent NATS acknowledgement drain pass.
Normal live chat, reloaded History, and Restore Test retain the same cancelled run.
The root acceptance digest is `56984a80275c3d3b1be7f0f2f55baa5bef5c6b4169ae84c9f6aad07e9e80f77c`.
The idle store now retains72 terminal jobs,72 dispatches, and151 checkpoints.

Later chat851 closes [live Worker and Supervisor recovery](code-live-owner-recovery-20261008.md) for the same original JavaScript runtime.
Its typed result2 survives both independently verified live cuts. Main grants checkpoint recovery at claim2/epoch2.
The next empty-source node fails safely, without another Code job or sentinel.
One settlement, both runtime removals, NATS drain, and normal live/reloaded support reference pass.
The root acceptance digest is `320c26ce098c1ba52137a0beeb2f5640b0b4a66f1901977ebd5aa5d3b7969f01`.
The idle store now retains74 terminal jobs,74 dispatches, and157 checkpoints.

Later chat852 closes [Main recovery during original compilation publication](code-compiled-publication-main-recovery-20261008.md).
The strict cut retains Publishing after Main stops and binds the original successful compiler receipt and descriptor.
The same snapshot becomes Ready after Main restarts. One result and matching SUCCEEDED settlement commit.
Both original runtimes are removed. NATS drains, and the same140-byte answer survives normal live chat and reload.
The root acceptance digest is `bbd428515651d077ed20779901512e8d4623ca412a6907e000f3fbdea4f361b9`.
The idle store retains76 terminal jobs,76 resolved dispatches,161 checkpoints, and one compiled snapshot.

Preparation outcomes and debug replacement, NATS restart, and Kubernetes remain separate runtime groups.

Supported application-list denial and outer artifact RBAC refusal later pass their finite cases.
Their frozen root receipt is `6b20be7632be1bffca3de94806b16c586de0daba5968f88acf13c0ea812140b9`.
The artifact HTTP 403 occurs at the outer RBAC gate; it does not prove a separate bucket ACL refusal.
Unchanged registered-child, quota, signer, and ACL component proofs retain their source boundaries.
Replacement reconciliation of the original debug writer and reference stays open.
Accepted Worker, Main, and Supervisor cases retain their recorded phase boundaries. Remaining acquisition and assembled NATS proofs stay open.
Current Kubernetes positive, cache, refusal, Stop, and owner-recovery boundaries remain separate.
Earlier unchanged component proofs retain their original scope.
These requirements do not create new load or HA gates for Code.

The later closure audit verifies 45 source and receipt pins without runtime actions.
Its checklist digest is `4c6a93ab083535e6920af90d0334c4bc5a26a09095562bc81449a8123ed12eb5`.
Its source and receipt ledger digest is `cd7c03a77d1407ed1053d29374fde17540fdfd76a6e8ef89ffcfddc7f6f862b1`.
Chat850 closes preparation Stop and pending cancellation across Worker loss.
Chat851 closes live Worker and Supervisor recovery for the original JavaScript job.
Chat852 closes Main recovery during publication of the original successful compilation.
Three runtime groups remain:

1. Verify preparation fault outcomes and applicable debug replacement reconciliation.
2. Verify queued and in-flight NATS recovery with the original broker storage.
3. Verify current hybrid Kubernetes execution, cache, refusals, Stop, recovery, and physical cleanup.

The separate [live canvas image](code-live-canvas-terminal-20261008.md) passes Node26 production build and strict local Alpine scan.
Its deployment and normal browser verification remain open.

The later exact Web deployment and normal browser checks close the separate author and fresh-selection correction.
Their receipt digest is `9406b43e2e008c791f2487476d2699097287773473936b8fcde6b5830e43de5f`.
The remaining runtime cases retain their required live, History, and reload checks.

Existing accepted language, cache, failure, active Stop, authority, and component proofs remove repeat checks.
Normal runtime isolation guards remain mandatory for each new case.
The existing Python scan exemption and unproved resolver destination enforcement remain explicit limits.
This list does not close Code or add new requirement families.

Graph gates 5a–5e follow complete Code acceptance.
Gates 6, 7, 7a, 7b, and 8 remain part of full worker completion.
Workspaces remain deferred until completion and release of the full worker.

## Local Web reload correction, 2026-10-07

The [reload mapping](code-chat-reload-ui-20261007.md) records the later source correction.
Chat838 retains the safe failure message, code, and reference after reload, but its displayed author changes.
The correction separates persisted answer authors from the composer selection.
It also preserves the fresh-chat participant before navigation and accepts both ID spellings.
Six focused suites pass 76 unique tests with zero failures or skips.
TypeScript, lint, and whitespace checks return direct status zero.
The Web correction is published as commit `42a0c390bc19bd46f0a3eae2f244714024595d71` on PR #1084.
Its image `sha256:218f6dfde4dca36f50d78b45bbf6c888c11855dab928888322e26580bd2677a4` builds from the frozen context.
The strict local Alpine scan reports zero HIGH or CRITICAL findings.
Root verifies the build, exported archive, report, and gate chain in the reload mapping.
Deployment and real browser acceptance remain open at this checkpoint.
Committed a409 head later reports 67 successful CI checks and the same two documented skips.
The readback remains separate from deployment and browser acceptance.
Main and Web remain at efa. The separately accepted native pair remains at c53.
The chat838 selection-loss branch and notification Offline cause remain unproved.
These checks do not close recovery, capacity, or the complete Code gate.

## Acceptance controller and application ownership

The private Python controller is an acceptance harness. It is not a shipped application service.
It reads bounded database metadata and exact runtime specifications.
Root owns normal UI Run and Stop actions.
The controller applies only the exact fault in a reviewed root authorization.
It does not write claims, checkpoints, journals, or business results.

Refused or missed cuts provide no recovery credit.

The UI submits normal application requests to Main.
Main owns PostgreSQL execution state and sends durable command identifiers through NATS.
The Rust Worker obtains Main claim authority and evaluates the graph.
The Code owners create durable preparation and execution dispatches.
The Sandbox Supervisor owns the isolated preparer and Code runtime lifecycle.
PostgreSQL remains authoritative; NATS supplies transport.

| Application owner | Source owner | Harness observation |
| --- | --- | --- |
| Graph Code activation and preparation | Worker `src/agents/graph/code_runtime.rs`, `code_preparation.rs`, and `code_attempt_remote.rs` | Original activation, request, preparation, and dispatch identities. |
| Claim and recovery authority | Main `internal/infra/db/repos/claims.go` and `model_checkpoint_authority.go` | Current claim attempt, lease epoch, and checkpoint authority. |
| Preparation cancellation and cleanup | Worker `src/sandbox/docker_preparation.rs`, `docker_supervisor.rs`, and vendored Docker job adapter | Exact original child state, isolation guards, cancellation, and physical removal. |
| Terminal result delivery | Worker `src/execution/output_delivery.rs` and Main `internal/infra/db/repos/output_inbox.go` | One original output and settlement. |

The latest misses concern harness identity, admission, diagnostics, and UI timing.
They do not establish a Code product defect or successful recovery.
The source maps retain each accepted application behavior and each remaining fault boundary.

## Normal broker fixture, 2026-10-08

Chat853 uses application147 and named version178 through the normal UI.
The original JavaScript node returns `2`; the same answer survives reload.
One original visit, one completed Code journal, one output, and one committed settlement remain recorded.
Two original dispatches and jobs complete.
Both captured runtimes pass all16 isolation checks and durable binding checks.
Both runtime IDs return HTTP404 and are absent from the complete runtime list.

The root fixture receipt is `3e94673b25d905c0a46be0480b0489d80d50c3c8b30908b815ac870ad8916581`.
The runtime capture is `1593fb3e0275819dee15e0670a99b35b24291d9fa27de4250db5f382aa7118f7`.
The physical removal receipt is `6dbeb972adafc75503873e4498103eac90ab17a20931787f11da461780d807b3`.
The normal stream and durable consumer finish empty.

The collector exits before case authorization or broker control.
Its source omits Main's legitimate `CLAIMED` waiting state; the exact exit cause was not captured.
The same-owner run has no owner-adoption receipt, so that verifier expectation requires source review.
The historical schema placeholder is replaced with the accepted deployment schema before this run.
All prior source packets and evidence remain preserved.
This fixture supplies no broker-loss, redelivery, owner-replacement, or complete Code acceptance.
