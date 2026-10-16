# Main NATS integration and preserved Code acceptance

The incoming Main baseline is `22403f640df1ac59f034f3132a95e44679ed21ef`.
It replaces Redis and Valkey with NATS, including the Rust Worker command transport.
The [command bus contract](../../../../docs/runtime-command-bus.md) defines the current deployment and delivery boundaries.
PostgreSQL remains the execution authority. Worker checkpoints and Supervisor receipts retain their existing owners.

## Conflict resolution

Five conflicts affect Redis files that Main deletes.
The integration preserves those deletions and does not restore a second transport.
The old same-consumer pending scan becomes unnecessary under the NATS redelivery contract.

| Previous behavior | Current owner | Preserved contract |
| --- | --- | --- |
| Recover unacknowledged work after a retryable failure | `src/execution/command_delivery.rs::answer_processed` | Request delayed redelivery unless durable retirement is confirmed. |
| Recover after a lost connection or process | `src/transport/nats_jetstream.rs` and the durable consumer | Preserve the command; `AckWait` redelivers when ownership heartbeats stop. |
| Preserve capacity while running | `CommandDeliveryRuntime` | Acquire bounded capacity before intake and retain ownership through processing. |
| Admit a replacement execution | Main claim routing | Keep generation, claim, and lease fencing authoritative. |
| Retire completed work | Command retirement | Confirm durable settlement before double acknowledgment. |

The new regression delivers one command twice, with identical bytes and stream sequence.
The first delivery requests a delayed retry. The second retires without another retry.
Capacity remains one throughout the test.
This component test does not prove a deployed NATS restart.

## Focused integration checks

The merged source passes 52 Rust tests, strict all-feature library/test Clippy, and formatting.
Those tests have no failures or skips. They run on Darwin arm64.
Main output and repository selectors pass 37 tests and 192 subtests.
Two additional database tests skip because no disposable PostgreSQL URL is supplied.
All four Web Code-debug tests pass without skips. Local Node 24 differs from CI's Node 26.
Worker Helm rendering passes eight assertions. NATS security rendering passes 267 assertions.
The local NATS render omits Kubernetes schema validation because `kubeconform` is unavailable.

## Docker storage correction

The secure overlay used temporary storage for `/data`.
The other Docker profiles had no named JetStream volume.
Container replacement could therefore lose broker state despite successful command publication.

Both base profiles now mount a declared named volume at `/data`.
The secure overlay preserves that volume and keeps only its PID path temporary.
The public local config sets `sync_interval: always`, as the Kubernetes config does.
Nine new render assertions check these Docker contracts.
This change preserves the existing Kubernetes persistent-volume contract.

An isolated Docker test publishes one command to file storage, then kills the server with `SIGKILL`.
A new container mounts the same named volume with the same public config and NATS 2.12.0 image.
It restores the command and consumer, then redelivers the same bytes and stream sequence.
Confirmed acknowledgment leaves zero messages, pending deliveries, and pending acknowledgments.
The test completes in 60 seconds. It changes no product service or database.
Its acceptance SHA256 is `2231d72b6775ed951ef28ede6d0cecb5b92d46ed709d12a66fbde95486e568c7`.

The first probe stops at ten seconds, before the consumer's 60-second `AckWait` expires.
The corrected probe permits that timeout and passes. Both receipts remain retained.
This storage proof does not establish secured transport, Main claim recovery, or complete pipeline recovery.

## Preserved browser evidence

The pre-integration Worker uses `d6568bae8`, image `3a86177965a1`.
Ephemeral chat 826 and persistent chat 827 show the exact Code failure and support reference.
Persistent reload retains them. Failed journals commit before publication, with zero dispatches and no later-node execution.
The fresh positive run in chat 825 completes four languages and eight unique platform reads.
Seven dispatches resolve, five graph checkpoints remain, and the final answer matches after reload.
The [typed failure mapping](code-terminal-failure-20261006.md) records the exact proof hashes.
The [v7 Worker-loss mapping](code-supervisor-task-ownership-20261006.md) retains its earlier restart proof.
These results remain evidence for their deployed revisions. They do not become NATS runtime evidence.

## Isolated migration evidence

The user authorizes both complete rehearsal databases in a private backup volume and a network-isolated PostgreSQL clone.
The copy retains stored credentials and tokens locally. It does not change the original databases.
The existing Code migration receipt bridge and owning Product and AgentState migrators pass in that clone.
Row, sequence, function, security, and captured catalog checks pass for source `d6568bae8`.
The root acceptance SHA256 is `1474cb9f0b2fed77504234f6f4ba2ecf53244928e38eed4708cf4fe5c79c57f1`.
Uncaptured comments and catalog dependency internals remain outside that proof.
The proof does not cover later NATS migrations or live rolling compatibility.

## Remaining deployment boundary

Require integrated CI, NATS bootstrap and mTLS checks, and migration verification before cohort replacement.
Preserve existing execution data and rollback material during deployment.
Retest Code completion and restart recovery through the browser on the assembled NATS stack.
Main preparation-message display and actual failed-journal replay remain separate acceptance cases.
Keep Point 5 open until its recorded gates pass.
Keep all workspace extensions in [WF-01](../wanted_feature.md#wf-01--code-workspaces) until full Worker release.
