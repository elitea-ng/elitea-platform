# Session write self-deadlock behind a forwarded event (2026-10-09)

A pipeline turn could hang forever with the Worker at 0% CPU and no logs, until the Worker was restarted and Main
marked the execution FAILED. This change makes the stream wrappers cancellation-safe, so the hang cannot happen. It
also bounds every session-writer lock wait and idle lock hold, so any similar defect fails readably instead of hanging.

## Business behaviour

- **Behaviour kept from the current platform.** Nested agent and pipeline-node progress (tool calls, model output,
  HITL cards) streams live into the parent chat while the children run. The parent conversation keeps one persisted
  history.
- **Not ported.** The current platform's failure mode for a stuck persistence step is "lost, run again from scratch".
  Here a stalled session write is either impossible by construction, or it fails with a typed, retryable error within a
  bound.

## The defect

Observed on 2026-10-09 with the real-model fixture: pipeline "Gate5 agent variable parent 20260930" (project 2,
application 138). Its two nested application nodes each call agent 137, which pauses on `ask_user`. During the turn
"answer card 1", node 2's child model returned an `ask_user` call and the execution hung.

- **What PostgreSQL showed.**
  - Backend A was `idle in transaction`. Its last statement was `merge_session_state`'s `SELECT … FOR UPDATE`. It held
    the root writer row `FOR SHARE` and the child model-scope writer.
  - Backend B waited on A for `FOR UPDATE OF writer` on the root writer row.
  - A's reply sat unread in the Worker socket.
- **Mechanism, confirmed by test.**
  - `PipelineNodeEventStreamingAgent::run` raced `node_events.recv()` against `root_events.next()` in a `biased`
    `select!`. When a forwarded node event won, the `next()` future was dropped. The inner graph stream survived,
    suspended inside a child model-scope append (`model_scope.rs` awaits `checkpoint.append` in its `try_stream`),
    which already held the root writer `FOR SHARE`.
  - The wrapper then yielded the node event. The Runner appended it to the root session, which needs the root writer
    `FOR UPDATE`.
  - The suspended append was never polled again, so it never released the lock. The result was a self-deadlock across
    two pool connections. It is timing-dependent: the signal must arrive while the child append is mid-transaction.
- **The same shape elsewhere.**
  - `ApplicationEventStreamingAgent` (`application_tools.rs`), identically.
  - The pipeline-tool `drain_child` (`application_pipeline.rs`) and the scoped application node runtime
    (`pipeline/scoped_runtime.rs`). Both await a bounded `send` of a forwarded event while the child stream or tool
    future is parked. When the 64-slot channel is full, they deadlock one level down.
  - Every other `select!` in the Worker and `libs/rust` was audited; none parks an agent stream while yielding work that
    needs the same lock (see Follow-ups for the one conditional site).

## Changed paths and enforcing code

| Mechanism | Owner | `path:line` |
| --- | --- | --- |
| `DrivenTask`: polls inline first and moves only pending work to an owned task (`tokio::spawn`). It aborts on drop, `cancel()` waits until the task has released what it owns, and a panic (inline or on the task) is a typed failure. | Worker | `services/elitea-worker-rust/src/agents/driven.rs:45`, `:58`, `:72`, `:79`, `:104` |
| `DrivenEventStream`: each pull goes through one `DrivenTask`, in lockstep with the caller; cancellation-safe | Worker | `src/agents/driven.rs:114`, `:133` |
| Pipeline node-event wrapper drives the graph | Worker | `src/agents/graph/node_events.rs:428` |
| Application-event wrapper drives the root agent; it also stops polling a closed child channel (that was a latent busy loop) | Worker | `src/agents/application_tools.rs:4372`, `:4391` |
| Pipeline-tool `drain_child` drives the child | Worker | `src/agents/application_pipeline.rs:1561` |
| Scoped application node drives the tool future, and cancels it before the queue drain on a drain failure | Worker | `src/agents/pipeline/scoped_runtime.rs:247`, `:259` |
| Session transactions begin with `SET LOCAL lock_timeout` and `SET LOCAL idle_in_transaction_session_timeout`, in one round trip (one shared builder) | Worker | `src/state/postgres_session.rs:105`, `:112`, `:121` |
| Named bounds (`WRITER_LOCK_TIMEOUT` 10 s, max 60 s; `IDLE_TRANSACTION_TIMEOUT` 30 s, max 300 s; min 1 ms), validated in `SessionLimits` | Worker | `src/state/postgres_session.rs:44`, `:48`, `:96` |
| Graph checkpoint transactions, which lock `agent_graph_checkpoint_writers` the same way (a child share-locks its root), carry the same bounds through the same builder and constants | Worker | `src/state/postgres_checkpointer.rs:1497` (all six `begin` sites) |
| `25P03` (idle timeout) joins `55P03` (lock timeout) as retryable: `session.storage_unavailable` / `checkpoint.storage_unavailable` | Worker | `src/state/postgres_session.rs:1676`, `src/state/postgres_checkpointer.rs:1528` |

There are no changes in `libs/rust`, no new dependencies, and no migrations. The bounds are transaction-local, so
they apply only to the session and graph-checkpoint transactions, not to other users of the agentstate pool.

## Tests

The suite ran with `cargo test --offline --locked` (Worker) against a disposable PG18 (`pgvector/pgvector:0.8.1-pg18`).
It used `ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and
`ELITEA_TEST_DATABASE_GUARD=disposable-pg18`.

- **Results (final commit).**
  - Full Worker suite: **1656 passed, 0 failed, 10 ignored**.
  - The 9 ignored PostgreSQL checkpointer tests were run explicitly with `--ignored`: **9 passed**.
  - The one remaining ignored test is a manual release-profile timing probe.
- **Checks.** `cargo fmt --check` clean; `cargo clippy --locked --all-targets --all-features -D warnings` clean.
- **New tests: 15, all passing.**
  - **Real PostgreSQL, the incident reproduction.**
    `agents::graph::node_events::session_lock_tests::node_event_yield_never_strands_a_child_append_holding_the_root_writer`.
    - Setup: a real root writer and a child `model_scope` writer.
    - A gate transaction holds the child session row, so the child append parks inside `merge_session_state` while
      holding the root writer.
    - The test asserts the parked state through `pg_blocking_pids`.
    - It then sends a node event and asserts that the Runner's root append waits on the child backend (the incident
      shape), opens the gate, and requires the turn to finish within 10 s with both events persisted.
    - **Before the fix it failed with exactly that self-deadlock.** After the fix it passes.
  - **Real PostgreSQL, the same story for the application wrapper.**
    `agents::application_tools::session_lock_tests::child_event_yield_never_strands_a_child_append_holding_the_root_writer`.
    - With only the wrapper change reverted, it now fails after the 10 s lock bound with `session.storage_unavailable`
      instead of hanging, which proves the backstop on its own.
  - **Real PostgreSQL, the bounds.** In `state::postgres_session_tests`:
    - `session_writer_lock_wait_fails_typed_instead_of_waiting_forever`: a stalled root-writer holder makes the append
      fail in ≥ 250 ms and < 5 s with `session.storage_unavailable`. The retry then lands the event exactly once.
    - `an_idle_session_transaction_releases_its_writer_locks`: PostgreSQL ends an idle holder. A `NOWAIT` contender
      then gets the writer row, and the holder's connection is gone.
    - `session_timeouts_are_bounded_at_activation`: limit + 1 and zero are refused before any database work; 1 ms and
      the maximum are accepted.
    - `state::postgres_checkpointer::lock_bound_tests::checkpoint_write_behind_a_stalled_writer_fails_typed_within_the_lock_bound`:
      a graph checkpoint save behind a stalled writer-row holder fails with `StorageUnavailable` after
      `WRITER_LOCK_TIMEOUT`, and succeeds once the holder releases.
  - **Unit tests, `agents::driven::tests` (7).**
    - A lost select arm keeps driving the item and never loses it.
    - Pulls stay in lockstep with the caller.
    - Ready items are returned inline without yielding to siblings.
    - Dropping the stream aborts an in-flight pull.
    - A panic, either inline or on the task, fails closed with `elitea_agent.driven_task_failed`, without the panic
      text.
    - `cancel()` returns only after the task has released what it owns.
    - Awaiting a `DrivenTask` is cancellation-safe.
- **Regression found and fixed during the change.**
  - Symptom: `pipeline_agent_node_resumes_parallel_nested_authorization_for_authorize_and_skip` failed with the first
    version, which always spawned a task per item.
  - Cause: parallel in-process siblings interleaved, and the test's scripted model gateway hands out responses in
    arrival order.
  - Fix: the inline first poll, now in `DrivenTask` for both the stream and the scoped tool future. Ready work takes no
    task, so work that never waits keeps its exact old scheduling. No test was weakened.
- **Skipped.** None besides the ignored timing probe. Not run: the drain paths of `drain_child` and `scoped_runtime`
  under a full 64-slot channel. They use the same helper, which the unit tests cover, but they have no dedicated
  end-to-end test.

## Performance

- **Budgets.**
  - Zero extra database round trips per session transaction: the bounds ride in the `BEGIN` simple-query message.
  - Zero tasks for ready work; one Tokio task (and one small boxed future) per inner item that is still pending after
    one poll.
  - No added locks or buffering.
- **Measured.**
  - A per-transaction `format!` of a ~100-byte `BEGIN` statement.
  - Browser turns on the real-model stack completed in 1.7–4.9 s (9 executions). No baseline exists for the hanging
    version, since it never completed.

## Durability

- **Crash windows.** A turn that previously hung forever (holding the root writer until the process died) now either
  completes, or fails within 10 s with a retryable typed error.
  - A transaction aborted by `lock_timeout` rolls back atomically, so no partial event is written.
  - A retried append is deduplicated by `event_id`; the exact-replay check is unchanged. The lock-wait test proves
    exactly one stored event after the retry.
- **Takeover.** A replacement claim that meets a stalled holder (for example, a parked append after lease loss) waits
  at most 10 s per attempt, and the stalled holder is ended after 30 s idle. Takeover can no longer be blocked
  indefinitely.

## Resilience

- **Everything bounded.** Lock waits: 10 s. Idle holds: 30 s. Both are typed (`session.storage_unavailable`,
  retryable).
- **Task ownership.** Inner pulls run on owned tasks that are aborted on drop. `scoped_runtime` cancels and awaits its
  tool task before draining, so no child sender outlives tool polling.
- **Panics.** A panic, inline or on the task, becomes `elitea_agent.driven_task_failed`, logged once with no payload.
- **Latent busy loop removed.** `ApplicationEventStreamingAgent` no longer spins on a closed child channel.

## Security

- **Bounds are not caller-controlled.** They are validated `Duration`s from `SessionLimits`, formatted as integer
  milliseconds; no caller text reaches SQL. The values stay parameter-free because PostgreSQL `SET` takes no bind
  parameters.
- **No sensitive data in errors or logs.** The new error carries no data, and the panic log has no payload.
- **No new egress, authorization surface or dependency.** Writer fencing (`ensure_current` plus `FOR UPDATE`/`FOR SHARE`
  on the writer row) is unchanged.
- **Reviews.**
  - `code-review` (high) reported 9 findings. Fixed: the graph checkpointer was unbounded; the scoped tool future had
    no inline fast path; panic handling differed between inline and task polling; a stale doc comment.
  - No change needed: re-formatting the `BEGIN` string per transaction (`begin_with` needs an owned `'static` statement
    either way).
  - Recorded as follow-ups below: asynchronous teardown on early return, a task per nesting level, full-channel
    end-to-end tests, and a structural guard against new raw-stream `select!` sites.
  - `security-review`: no findings. Injection: only validated integers are formatted. Writer fencing: the lease check,
    writer-row lock and commit check are unchanged, and none depend on task-locals. Secrets/PII: the panic payload is
    discarded. Crypto and deserialization: not applicable.
- **Audit.** `cargo deny --all-features check advisories` (Worker) reports one pre-existing finding,
  RUSTSEC-2023-0071 (`rsa`, Marvin attack, via `sqlx-mysql`). It is unchanged by this PR: `Cargo.toml` and
  `Cargo.lock` are untouched.

## Recovery guarantees

| Component × phase | Class | Enforcing code | Proof |
| --- | --- | --- | --- |
| Worker × tool call (child model-scope append while a forwarded event persists) | R | `driven.rs:133`, `node_events.rs:428`, `application_tools.rs:4372` | both `session_lock_tests`; browser runs |
| Worker × HITL pause and decision (nested `ask_user` answer turn) | R | as above | browser: three two-card journeys, each turn SUCCEEDED |
| Worker × fan-out child forwarding (pipeline tool, scoped application node) | R | `application_pipeline.rs:1561`, `scoped_runtime.rs:247` | `driven::tests`; full suite |
| PostgreSQL × session write under a stalled lock holder | I | `postgres_session.rs:107`, `:1672` | `session_writer_lock_wait_fails_typed_instead_of_waiting_forever` (retry lands once) |
| PostgreSQL × takeover behind an idle holder | I | `postgres_session.rs:112` | `an_idle_session_transaction_releases_its_writer_locks` |
| PostgreSQL × graph checkpoint write under a stalled writer holder | I | `postgres_checkpointer.rs:1497` | `checkpoint_write_behind_a_stalled_writer_fails_typed_within_the_lock_bound` |
| Worker × panic inside an inner agent stream | F | `driven.rs:72` | `a_panicking_pull_fails_closed_with_a_typed_error` |

Pre-existing L closed: the hung execution, which was resolved only by a Worker restart and then marked FAILED.

## Real-browser evidence

- **Stack.** Standalone stack `elitea-sessionlock` with `STANDALONE_HOST=sessionlock.localhost`, browsed at
  `http://sessionlock.localhost:18360/app/`. The real-model dump `product-real-models-main-c0f2e5f9b.dump` was
  restored; migration ledger at shared 158, tenants 148.
- **Images.**
  - Worker: `ghcr.io/elitea-ng/elitea-worker-rust:session-deadlock-a97c56daf`, image
    `sha256:a17724997fc06bafeda28096dfecb743f45035e142e50a518db43f381b85bea1`, built from this branch (it contains
    #1160).
  - Main, web, gateway, scheduler and subapp-host: `main-c0f2e5f9b-verify`.
  - Mocks and engines: `main-0def77b22-verify`.
  - No migrations differ between c0f2e5f9b and this branch's base, a1d38fb4d.
- **Binary check (§3b).** The extracted `/usr/local/bin/elitea-worker-rust` contains `elitea_agent.driven_task_failed`
  and `SET LOCAL idle_in_transaction_session_timeout`. The incident image (`nestedapp-restart-fix`) contains neither.
- **Journeys.**
  - Fixture: pipeline 138, version 145. Agent 137 (version 144) runs on `eu.anthropic.claude-haiku-4-5-20251001-v1:0`
    through the real gateway; the LLM mock received 0 requests.
  - Three persistent chats (880, 881, 882) ran the full flow: first message → card 1 → answer card 1 (node 1
    completes, node 2's child asks card 2, the incident step) → answer card 2 → both node results.
- **Executions.** All `SUCCEEDED` (seconds from admission to settlement):

  | Chat | Execution | Seconds |
  | --- | --- | --- |
  | 880 | `71cac438a324fff3ad862250bb76f77d` | 3.5 |
  | 880 | `8af105690479bc8a88f620a130d18bc6` | 3.8 |
  | 880 | `1a72bf8bb834da8b5e448052eb381058` | 1.7 |
  | 881 | `ce23124147e05d446e31ad89c9bf5818` | 4.5 |
  | 881 | `7bc74abdb4688c7422a81130c8c32f96` | 4.9 |
  | 881 | `a00161709ef180a451bca7e4bb5cf824` | 1.8 |
  | 882 | `a5f24e02ad0af71d1bba1d383bd1c229` | 2.6 |
  | 882 | `85cabb0bc5e2daa4578da81fafa7b2c8` | 3.4 |
  | 882 | `ce391a1d4258d963b573c9690da7d1c4` | 1.8 |

- **After reload.** Each chat showed the user message and the final message with both blocks (OVERRIDE CALL with
  `Audience=operators; tone=brief`, and DEFAULT CALL with `Audience=users; tone=formal`). The card in chat 880 also
  survived a reload before it was answered.
- **Server side.**
  - No `agentstate` backend was ever `idle in transaction`.
  - The Worker logged 0 `lock_not_available`, `storage_unavailable`, `driven_task_failed` or panic lines.
- **Final image, second pass.** The evidence above is from image `a97c56daf`. After the review fixes, the stack was
  rebuilt on the final commit: Worker `session-deadlock-cd18caa56`, image
  `sha256:906f2eff85db90875dab08d6c8211cf0a305dd8319b96b81ae11645daeb7a67d`. Its binary contains the two strings above
  plus `an agent task panicked; the execution fails closed`.
  - **An interrupted run, recorded honestly.** The first journey on this image (chat 880) was cut short by the host,
    not by the fix.
    - What happened: the Mac went to sleep on battery from 22:16 to 22:37 UTC (`pmset` log: "Maintenance Sleep" with
      second-long dark wakes). Main's claim lease expired while the Worker was frozen.
    - What the Worker logged: the stream ended with `stream_disposition="lease_failed"` / `claim_lease_control_failure`
      at 22:37:58. The lifecycle span shows only about 20 s awake (`time.busy=242ms time.idle=19.7s`). The command was
      retained for recovery, and Main retired it (`agent_delivery.authorization_terminal_retired`), so execution
      `0a2f0c0ec5b83bf197e8caa066d070f9` settled as FAILED.
    - What it was not: no agentstate backend was ever `idle in transaction`. This is the expected lease-loss path, not
      the session-lock hang.
  - **Clean pass.** With the host kept awake, a fresh journey in chat 881 ran first message → card 1 → answer card 1
    → card 2 → answer card 2 → both node results. All three executions `SUCCEEDED`:

    | Execution | Seconds |
    | --- | --- |
    | `039624375e13953a332ea986eef5dff4` | 2.9 |
    | `15cbc1e8aa8ca4216f5df6fd3aee6a23` | 3.8 (the incident step) |
    | `9e1bd650e821b39fddf8d9f561dff3a8` | 1.9 |

    - After reload, the final message showed both the OVERRIDE CALL and DEFAULT CALL blocks.
    - 12 Haiku 4.5 model log lines and 0 LLM-mock requests.
    - 0 `idle in transaction` agentstate backends.
    - 0 lock-timeout, driven-task or panic log lines.
- **Teardown.** The stack was torn down with `down -v` after each pass's evidence was recorded.

## Fixtures

- **Browser.** The fixture came from the restored real-model dump, unchanged. The chats were created in the UI through
  the pipeline's Chat button.
- **Tests.** Test data was created in code against isolated, throwaway PostgreSQL databases.

## Follow-ups

- **Fatal-lease path.** `execution/native_agent_lifecycle.rs:699`: on `FatalLease`, the native stream stays parked
  (possibly mid-append) while `close_no_ack` joins the lease actor. This is not a self-deadlock, because the close
  needs no session lock, and the new bounds release any held writer row within 30 s. Dropping or draining `native`
  before the close would remove the window entirely.
- **End-to-end drain tests.** Add tests for `drain_child` / `scoped_runtime` under a full 64-slot event channel.
- **Asynchronous teardown on early return.** On an early `?` return in `drain_child` or the wrappers, a pending pull is
  aborted rather than dropped in place. It can finish its current poll, and a late node event is then discarded by the
  existing stale-event guard. An explicit `cancel().await` on those paths would make the teardown synchronous.
- **One task per nesting level.** Nested wrappers can spawn one task per level for an item that stays pending. This is
  bounded by nesting depth; it was measured as negligible in the browser runs.
- **Structural guard.** Add a lint or a single forwarding combinator, so no new `select!` can race a raw `EventStream`
  against a side channel.
