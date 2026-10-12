# Code failed-journal recovery after Worker loss, 2026-10-07

The isolated candidate passes the original failed-journal recovery case.
Root accepts this finite case after technical, browser, isolation, and removal checks.
Complete Code acceptance and Point 5 remain open.

The current application remains the business behavior reference.
The [terminal failure mapping](code-terminal-failure-20261006.md) retains the current-to-new behavior and implementation history.
This record adds deployed proof for durable failure restoration after loss of its original Worker.

## Required behavior and source ownership

| Required behavior | Source owner | Actual proof |
| --- | --- | --- |
| Save the completed Code visit and its original activation | Worker `src/agents/graph/code_attempt_remote.rs`, `code_result.rs`, and `turn_checkpointer.rs` | Original preparation and execution identities, two dispatches, and the completed predecessor journal remain unchanged. |
| Save the failure decision before terminal publication | Worker `src/agents/graph/node_recovery.rs`, `node_recovery_codec.rs`, and `node_recovery_runtime.rs` | The original `Failed.Stop` journal has revision 2, one history entry, two events, class `invalid_input`, and reason `retry_disabled`. |
| Grant recovery under current authority | Main `internal/infra/db/repos/claims.go`, `model_checkpoint_authority.go`, and `node_recovery_claim.go` | A new claim at attempt 2 and epoch 2 receives `AGENT_MODEL_CHECKPOINT` authority and a nonzero checkpoint digest. |
| Restore the recorded failure without Code execution | Worker `src/execution/checkpoint_recovery.rs` and `native_agent_lifecycle.rs` | The original generation settles as FAILED. No new node dispatch or execution occurs. |
| Commit one typed terminal result | Worker `src/execution/output_delivery.rs`, `src/protocol/output.rs`, and Main `internal/infra/db/repos/output_inbox.go` | One projected `RUNTIME_FAILURE` output and one committed FAILED settlement have the same digest. |
| Retain the safe failure and operator reference | Main terminal projection and Web chat history | The live UI and ordinary reload show `PIPELINE_CODE_FAILED` and the same support reference. |

Worker paths are relative to this service. Main paths are relative to `services/elitea-main/`.
No product source change is required by this successful recovery case.

## Deployed proof

Chat 843 uses actor 3, project 2, application 147, version 170, and participant 135.
Execution `692bf01411fc54220c2c47f2fff59f4f` remains at generation 1.
The controller captures the real failed journal before it kills the original paused Worker.
The two copied-database row locks last at most three seconds each and roll back.
No journal, claim, checkpoint, or spool content is rewritten or transplanted.

The replacement uses the same accepted Worker image and a fresh empty private spool.
The old Worker and spool remain preserved. The replacement never mounts or opens the old spool.
Normal NATS delivery and Main checkpoint authority restore the recorded failure.
The two original claims are released. The original failure revision, history, attempt, and digest remain unchanged.
The completed Code journal and both original preparation and execution identities also remain unchanged.

Both captured sandbox runtimes pass all 16 isolation checks.
No actual candidate runtime lacks inspection evidence.
Separate final readback returns HTTP 404 for both runtimes and confirms their absence from job containers.
The historical 49 jobs remain present; the candidate now has 51 jobs, 51 dispatches, 111 checkpoints, and zero active jobs.
These are observation-time counts, not fixed requirements for later cases.

The support reference is `36b7ada7-8b9a-58e5-b5a0-7132721509dc` in both live and reloaded UI.

| Frozen evidence | SHA-256 |
| --- | --- |
| Root finite acceptance | `cbbdffb369169b57d1c6456928bc0b233d9b4cdc8b69886d5bd17a049bf6d262` |
| Actual failed boundary capture | `ad60d0e2a8f5cc0c08748d902a8e39d7ebd131a2074e27724f2d988e07d190fa` |
| Reviewed replacement authorization | `c9bd17d402bbff19dce438368c6a409bdbe2cde5cb8076c770d53be26e112dd2` |
| Technical recovery result | `2ace8510e5b699a4e108c2bf0aaf34ba998e3b86fd19d95eea36fd0d44dd7f73` |
| Root successor adapter | `443996d935a85c391285947339844c871d5ec9b07058946d4e4b5fe3d526f6e3` |
| Independent settled snapshot | `024b38ce5ca790172fb91f799e76a5cc4fe6b061c563239af48c251ed007d084` |

The accepted image source remains `c53ab7d5496a571c1844e9906a83c261ff0da06e`.
Worker image is `f4427ea1769bc38992bbd0b54b66cc59b487f0cdd955c1eb691eb11069944f98`.
Supervisor image is `6f98394a26cd4f91c261c30e6bbe6d698bc47baafc7f4d1ab1e2b9cfdde848e6`.
Main and Web remain at source efa. Web source correction 42a0 is not deployed at this checkpoint.

## Operator correction history and proof limits

Earlier capture attempts refuse safely and remain preserved.
Two observer defects prevent admission of the actual blocked writer: SQL normalization retains edge whitespace, and PostgreSQL inet text includes the mask.
The corrected matcher trims after whitespace normalization and compares typed inet values.
Its exact writer, lock, blocker, and deadline predicates remain required.

Attempt 5 captures the real failed boundary but refuses during new-spool initialization before the Worker kill.
Guarded rollback verifies that the exact original Worker runs unpaused.
The initializer's final pathname reopen conflicts with mode 0700 after ownership transfer under only CHOWN capability.
An isolated check reproduces permission error 13 for the old program and passes the repaired program.
The repaired program holds a directory descriptor before transfer and uses descriptor operations for ownership and both emptiness checks.
It adds no capability, mount, network access, or privilege.
The deleted attempt-5 helper's exact error remains unobserved.
Attempt 6 verifies actual repaired initialization and then passes full recovery acceptance.

The Product and AgentState read-only snapshots are sequential, not atomic.
Destination-level preparation egress enforcement remains unproved.
Stored sandbox cleanup flags are false; actual HTTP 404 and absence checks prove removal for these two runtimes.
Preparation exit 137 is the observed removal outcome. Execution exit 0 and no OOM are observed.
UI elapsed time is not a performance benchmark.

The efa Web still changes the displayed answer author to Elitea and defaults the composer after reload.
Failure content and support reference remain intact.
The separate [Web reload correction](code-chat-reload-ui-20261007.md) requires its own deployment and browser acceptance.

This case does not prove pending-preparation cancellation or recovery, active-execution owner loss, Supervisor loss, Main loss, NATS loss, or Kubernetes recovery.
These cases retain their separate completion requirements.
Graph gates 5a–5e follow complete Code acceptance. Workspaces remain deferred until full worker completion and release.
