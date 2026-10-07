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

| Deployed role | Exact image SHA-256 |
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
The exact current-claim failed-journal restoration test has not run.
Automatic approval review rejects its copied-row locks and candidate Worker pause before any mutation.
Human approval remains pending for that reviewed fault action.

Later-request Cargo bundle reuse exposes an existing preparation gap.
The [lookup correction](code-frozen-cargo-lookup-20261007.md) passes owning-source checks but has no deployed cache acceptance at this checkpoint.
Cold, warm, matching compiled, and editor runs remain required after coordinated Worker and Supervisor delivery.

Preparation Stop, pending Stop across Worker replacement, and the remaining preparation messages stay open.
Supported-operation denial, debug permission refusal, registered-child export, and debug reconciliation stay open.
Worker, Main, Supervisor, acquisition, and assembled NATS recovery require their recorded phase proofs.
Current Kubernetes positive, cache, refusal, Stop, and owner-recovery boundaries remain separate.
Earlier unchanged component proofs retain their original scope.
These requirements do not create new load or HA gates for Code.

Graph gates 5a–5e follow complete Code acceptance.
Gates 6, 7, 7a, 7b, and 8 remain part of full worker completion.
Workspaces remain deferred until completion and release of the full worker.
