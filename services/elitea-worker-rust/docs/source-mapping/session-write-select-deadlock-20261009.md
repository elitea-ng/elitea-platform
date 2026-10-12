# Session write self-deadlock behind a forwarded event (2026-10-09)

A pipeline turn could hang forever. The Worker stayed at 0% CPU and wrote no logs. A Worker restart ended the hang,
and Main then marked the execution FAILED. This change makes the stream wrappers cancellation-safe, so the hang cannot
happen. It also bounds every session-writer lock wait and every idle lock hold. A similar defect now fails readably
instead of hanging.

## Business behaviour

- **Behaviour kept from the current platform.** Nested agent and pipeline-node progress streams live into the parent
  chat while the children run. Progress means tool calls, model output and HITL cards. The parent conversation keeps
  one persisted history.
- **Not ported.** The current platform handles a stuck persistence step as "lost, run again from scratch". Here a
  stalled session write is impossible by construction. If it still occurs, it fails with a typed, retryable error
  within a bound.

## The defect

Observed on 2026-10-09 with the real-model fixture: pipeline "Gate5 agent variable parent 20260930" (project 2,
application 138). Its two nested application nodes each call agent 137, which pauses on `ask_user`. During the turn
"answer card 1", the child model of node 2 returned an `ask_user` call, and the execution hung.

- **What PostgreSQL showed.**
  - Backend A was `idle in transaction`. Its last statement was the `SELECT … FOR UPDATE` of `merge_session_state`.
    It held the root writer row `FOR SHARE` and the child model-scope writer.
  - Backend B waited on A for `FOR UPDATE OF writer` on the root writer row.
  - The reply of A sat unread in the Worker socket.
- **Mechanism, confirmed by test.**
  - `PipelineNodeEventStreamingAgent::run` raced `node_events.recv()` against `root_events.next()` in a `biased`
    `select!`. A forwarded node event won, and the `next()` future was dropped. The inner graph stream survived.
    It was suspended inside a child model-scope append (`model_scope.rs` awaits `checkpoint.append` in its
    `try_stream`). That append already held the root writer `FOR SHARE`.
  - The wrapper then yielded the node event. The Runner appended it to the root session. That append needs the root
    writer `FOR UPDATE`.
  - Nothing polled the suspended append again, so it never released the lock. The result was a self-deadlock across
    two pool connections. The defect depends on timing. The signal must arrive while the child append is
    mid-transaction.
- **The same shape elsewhere.**
  - `ApplicationEventStreamingAgent` (`application_tools.rs`) has the identical defect.
  - The pipeline-tool `drain_child` (`application_pipeline.rs`) and the scoped application node runtime
    (`pipeline/scoped_runtime.rs`) also have it. Both await a bounded `send` of a forwarded event while the child
    stream or tool future is parked. When the 64-slot channel is full, they deadlock one level down.
  - The change audited every other `select!` in the Worker and `libs/rust`. None parks an agent stream while it yields
    work that needs the same lock. One site is conditional (see Follow-ups).

## Changed paths and enforcing code

| Mechanism | Owner | `path:line` |
| --- | --- | --- |
| `DrivenTask` polls inline first. It moves only pending work to an owned task (`tokio::spawn`). It aborts on drop. `cancel()` waits until the task has released what it owns. A panic (inline or on the task) is a typed failure. | Worker | `services/elitea-worker-rust/src/agents/driven.rs:45`, `:58`, `:72`, `:81`, `:106` |
| `DrivenEventStream` sends each pull through one `DrivenTask`, in lockstep with the caller. It is cancellation-safe. `cancel()` returns only after the inner stream is dropped, including an in-flight pull. | Worker | `src/agents/driven.rs:116`, `:135`, `:160` |
| The pipeline node-event wrapper drives the graph. | Worker | `src/agents/graph/node_events.rs:428` |
| The application-event wrapper drives the root agent. It also stops polling a closed child channel. That was a latent busy loop. | Worker | `src/agents/application_tools.rs:4372`, `:4391` |
| Pipeline-tool `drain_child` drives the child. It stops the child on every exit, including each `?` and the pause return. It does this before it releases the node-event drain guard. | Worker | `src/agents/application_pipeline.rs:1561`, `:1627` |
| Scoped application node: one method (`run_child_draining`) drives the tool future, forwards its events and stops it. Then it drains its queue. | Worker | `src/agents/pipeline/scoped_runtime.rs:246`, `:293`, `:302`, `:313` |
| On lease loss, the Worker drops the agent run before it joins the lease actor. A write held inline rolls back at the drop. A write inside a driven pull is aborted at the drop. It rolls back when the Tokio runtime next runs that task. Writer fencing refuses any commit after the loss in both cases. | Worker | `src/execution/native_agent_lifecycle.rs:574` |
| Session transactions begin with `SET LOCAL lock_timeout` and `SET LOCAL idle_in_transaction_session_timeout`, in one round trip. One shared builder does this. | Worker | `src/state/postgres_session.rs:105`, `:112`, `:121` |
| Named bounds: `WRITER_LOCK_TIMEOUT` 10 s (maximum 60 s) and `IDLE_TRANSACTION_TIMEOUT` 30 s (maximum 300 s). The minimum is 1 ms. `SessionLimits` validates them. | Worker | `src/state/postgres_session.rs:44`, `:48`, `:96` |
| Graph checkpoint transactions lock `agent_graph_checkpoint_writers` the same way. A child share-locks its root. They carry the same bounds through the same builder and constants. | Worker | `src/state/postgres_checkpointer.rs:1497` (all six `begin` sites) |
| `25P03` (idle timeout) joins `55P03` (lock timeout) as retryable. The codes are `session.storage_unavailable` and `checkpoint.storage_unavailable`. | Worker | `src/state/postgres_session.rs:1676`, `src/state/postgres_checkpointer.rs:1528` |

This change has no edits in `libs/rust`, no new dependencies and no migrations. The bounds are transaction-local. They
apply only to the session and graph-checkpoint transactions. They do not apply to other users of the agentstate pool.

## Tests

The suite ran with `cargo test --offline --locked` (Worker) against a disposable PG18 (`pgvector/pgvector:0.8.1-pg18`).
It used `ELITEA_TEST_DATABASE_URL`, `ELITEA_REQUIRE_POSTGRES_RECEIPT_TESTS=1` and
`ELITEA_TEST_DATABASE_GUARD=disposable-pg18`.

- **Results (final commit).**
  - Full Worker suite: **1663 passed, 0 failed, 10 ignored**.
  - The 9 ignored PostgreSQL checkpointer tests ran explicitly with `--ignored`: **9 passed**.
  - The one remaining ignored test is a manual release-profile timing probe.
- **Checks.** `cargo fmt --check` is clean. `cargo clippy --locked --all-targets --all-features -D warnings` is clean.
- **New tests: 22, all passing.**
  - **Real PostgreSQL, the incident reproduction.**
    `agents::graph::node_events::session_lock_tests::node_event_yield_never_strands_a_child_append_holding_the_root_writer`.
    - Setup: a real root writer and a child `model_scope` writer.
    - A gate transaction holds the child session row. The child append then parks inside `merge_session_state` while it
      holds the root writer.
    - The test asserts the parked state through `pg_blocking_pids`.
    - The test sends a node event. It asserts that the root append of the Runner waits on the child backend (the
      incident shape). It then opens the gate. The turn must finish within 10 s with both events persisted.
    - **Before the fix, it failed with exactly that self-deadlock.** After the fix, it passes.
  - **Real PostgreSQL, the same story for the application wrapper.**
    `agents::application_tools::session_lock_tests::child_event_yield_never_strands_a_child_append_holding_the_root_writer`.
    - With only the wrapper change reverted, it fails after the 10 s lock bound with `session.storage_unavailable`.
      It no longer hangs. This proves the backstop on its own.
  - **Real PostgreSQL, the bounds.** These tests are in `state::postgres_session_tests`:
    - `session_writer_lock_wait_fails_typed_instead_of_waiting_forever`: a stalled root-writer holder makes the append
      fail in ≥ 250 ms and < 5 s with `session.storage_unavailable`. The retry then lands the event exactly once.
    - `an_idle_session_transaction_releases_its_writer_locks`: PostgreSQL ends an idle holder. A `NOWAIT` contender
      then gets the writer row. The connection of the holder is gone.
    - `session_timeouts_are_bounded_at_activation`: the limit + 1 and zero are refused before any database work. The
      value 1 ms and the maximum are accepted.
    - `state::postgres_checkpointer::lock_bound_tests::checkpoint_write_behind_a_stalled_writer_fails_typed_within_the_lock_bound`:
      a graph checkpoint save behind a stalled writer-row holder fails with `StorageUnavailable` after
      `WRITER_LOCK_TIMEOUT`. It succeeds once the holder releases.
    - Both lock-bound tests run on their own pool with a 1-minute acquire bound. They assert PostgreSQL `55P03` in the
      cause chain.
    - Under full-suite load, the shared 2 s harness pool once timed out first. That pool timeout also maps to
      `StorageUnavailable`, so it could otherwise stand in for the lock timeout.
  - **End-to-end full-channel tests.** Each test fails with the self-deadlock when its driven form is reverted. Each
    passes with the driven form.
    - `agents::application_pipeline_tests::a_full_parent_channel_never_strands_a_child_holding_the_root_writer`: the
      pipeline-tool `drain_child`, through a test-only shim with an injected child.
    - `agents::pipeline::scoped_runtime::tests::a_full_node_event_channel_never_strands_a_child_holding_the_root_writer`:
      `run_child_draining`, with a real graph-call receipt.
    - In both tests, the child takes the "root writer" for one append parked on I/O. A mutex stands in for the row
      lock. A sibling floods 70 events, and the consumer persists each event under that writer. That fills the 64-slot
      channel and blocks the forward.
    - Each test now asserts this precondition explicitly. It waits (5 s bound) until the consumer has received one
      event and the free capacity of the channel is 0. Only then does it release the child I/O.
    - The probes are `PipelineNodeEventSender::capacity()` and a sender clone, `parent_probe`. `capacity()` is a
      `#[cfg(test)]` helper in `src/agents/graph/node_events.rs`, used by the scoped-runtime test. The pipeline-tool
      test harness keeps `parent_probe`. The 256-yield heuristic is gone.
    - After the release, everything must finish within 10 s. Reverting the fix still fails with the self-deadlock after
      the precondition is reached.
  - **Lease loss.** `execution::output_delivery_tests::lease_loss_releases_a_parked_agent_stream_and_retains_the_command`.
    - A parked agent stream plus a failing lease renewal ends recovery-required. There is no settlement or retirement,
      and the parked stream is released.
    - The fake control has a `renew_fail_switch`. The test sets it only after the agent parks. The test no longer
      assumes that the lease activation renews exactly once.
    - Ordering: on lease loss, the Worker drops the agent run before it joins the lease actor. A write held inline
      rolls back at the drop. A write inside a driven pull is aborted at the drop. It rolls back when the Tokio runtime
      next runs that task. Writer fencing refuses any commit after the loss in both cases.
    - Limit: the exact drop-before-join order is not observable in-process. The lease close only signals and joins the
      actor, with no RPC to trace. Code enforces the order at `native_agent_lifecycle.rs:574`. This test proves the
      path.
  - **Budget.** `agents::driven::tests::nested_wrappers_spawn_no_task_for_ready_items_and_one_per_level_when_pending`:
    with three nested levels, the measured task counts per item are `[0, 0, 3]`.
  - **Teardown.** `agents::driven::tests::cancel_returns_only_after_an_in_flight_pull_dropped_the_inner_stream`.
  - **Structural guard.** `agents::driven::tests::no_select_arm_polls_a_raw_agent_stream`.
    - What it checks: every non-test `select!` arm in the Worker and `agent-runtime` that polls `x.next()` or
      `x.next_event()` must bind `x` through `DrivenEventStream::new`. The only exception is a justified allowlist
      entry.
    - Allowlist: `parallel.rs` `inflight`, and `native_agent_lifecycle.rs` `native`. A stale allowlist entry also fails
      the test.
    - Mutation check: reverting `node_events.rs` to a raw stream fails the test. The failure names the file and the
      identifier.
    - Hardening 1: the guard computes the relative path of each file with `strip_prefix(root)`. A checkout path that
      contains `src` no longer breaks it.
    - Hardening 2: a polled receiver that is not a plain local identifier is a violation. Examples are a field such as
      `self.inner.next()` and a call result such as `x.as_mut().next()`.
    - Hardening 3: the binding check uses the nearest `let mut x` before the `select!`. It no longer uses the first one
      in the file.
  - **Guard self-test.**
    `agents::driven::tests::the_guard_refuses_untraceable_receivers_and_uses_the_nearest_binding` proves all three
    hardening points.
  - **Unit tests, `agents::driven::tests` (7).**
    - A lost select arm keeps driving the item and never loses it.
    - Pulls stay in lockstep with the caller.
    - Ready items return inline without yielding to siblings.
    - Dropping the stream aborts an in-flight pull.
    - A panic, either inline or on the task, fails closed with `elitea_agent.driven_task_failed`. The panic text is not
      exposed.
    - `cancel()` returns only after the task has released what it owns.
    - Awaiting a `DrivenTask` is cancellation-safe.
- **Regression found and fixed during the change.**
  - Symptom: `pipeline_agent_node_resumes_parallel_nested_authorization_for_authorize_and_skip` failed with the first
    version, which always spawned a task per item.
  - Cause: parallel in-process siblings interleaved. The scripted model gateway of the test hands out responses in
    arrival order.
  - Fix: the inline first poll, now in `DrivenTask` for both the stream and the scoped tool future. Ready work takes no
    task, so work that never waits keeps its exact old scheduling. No test was weakened.
- **Skipped.** None besides the ignored timing probe.

## Performance

- **Budgets.**
  - The bounds add zero database round trips per session transaction. They ride in the `BEGIN` simple-query message.
  - Ready work costs zero tasks. Each inner item that is still pending after one poll costs one Tokio task and one
    small boxed future.
  - Nested wrappers cost one task per level for a pending item. Nesting depth bounds this cost. The budget test
    measures exactly `[0, 0, 3]` for three levels.
  - The change adds no locks and no buffering.
- **Measured.**
  - The added cost is one `format!` of a ~100-byte `BEGIN` statement per transaction.
  - Browser turns on the real-model stack completed in 1.7–4.9 s (9 executions).
  - No baseline exists for the hanging version, because it never completed.

## Durability

- **Crash windows.** A turn that previously hung forever held the root writer until the process died. Now the turn
  either completes or fails within 10 s with a retryable typed error.
  - `lock_timeout` aborts a transaction, and the abort rolls back atomically. No partial event is written.
  - The `event_id` deduplicates a retried append. The exact-replay check is unchanged.
  - The lock-wait test proves exactly one stored event after the retry.
- **Takeover.** A replacement claim can meet a stalled holder, for example a parked append after lease loss. It waits at
  most 10 s per attempt. The stalled holder ends after 30 s idle. Takeover can no longer be blocked indefinitely.

## Resilience

- **Everything bounded.** Lock waits last at most 10 s. Idle holds last at most 30 s. Both are typed
  (`session.storage_unavailable`, retryable).
- **Task ownership.** Inner pulls run on owned tasks that abort on drop. `scoped_runtime` cancels and awaits its tool
  task before it drains. No child sender outlives tool polling.
- **Panics.** A panic, inline or on the task, becomes `elitea_agent.driven_task_failed`. The Worker logs it once with no
  payload.
- **Latent busy loop removed.** `ApplicationEventStreamingAgent` no longer spins on a closed child channel.

## Security

- **Bounds are not caller-controlled.**
  - They are validated `Duration`s from `SessionLimits`, formatted as integer milliseconds.
  - No caller text reaches SQL.
  - The values stay parameter-free, because PostgreSQL `SET` takes no bind parameters.
- **No sensitive data in errors or logs.** The new error carries no data. The panic log has no payload.
- **No new egress, authorization surface or dependency.** Writer fencing is unchanged. It uses `ensure_current` plus
  `FOR UPDATE`/`FOR SHARE` on the writer row.
- **Reviews.**
  - `code-review` (high) reported 9 findings. Four are fixed:
    - The graph checkpointer was unbounded.
    - The scoped tool future had no inline fast path.
    - Panic handling differed between inline and task polling.
    - A doc comment was stale.
  - No change needed: the `BEGIN` string is formatted again for each transaction. `begin_with` needs an owned `'static`
    statement either way.
  - The other four findings are recorded as follow-ups below.
    - Asynchronous teardown on early return.
    - A task per nesting level.
    - Full-channel end-to-end tests.
    - A structural guard against new raw-stream `select!` sites.
  - `security-review`: no findings.
    - Injection: the code formats only validated integers.
    - Writer fencing: the lease check, the writer-row lock and the commit check are unchanged. None depends on
      task-locals.
    - Secrets and PII: the panic payload is discarded.
    - Crypto and deserialization: not applicable.
- **Audit.** `cargo deny --all-features check advisories` (Worker) reports one pre-existing finding. It is
  RUSTSEC-2023-0071 (`rsa`, Marvin attack, via `sqlx-mysql`). This PR does not change it, because `Cargo.toml` and
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
| Worker × lease loss while a session write is parked | I (command retained for recovery) | `native_agent_lifecycle.rs:574` | `lease_loss_releases_a_parked_agent_stream_and_retains_the_command` |
| Worker × fan-out child forwarding under a full channel | R | `application_pipeline.rs:1561`, `:1627`; `scoped_runtime.rs:293` | both full-channel end-to-end tests |

Pre-existing L closed: the hung execution. Only a Worker restart resolved it, and Main then marked it FAILED.

## Real-browser evidence

- **Stack.** Standalone stack `elitea-sessionlock` with `STANDALONE_HOST=sessionlock.localhost`, browsed at
  `http://sessionlock.localhost:18360/app/`. The stack restored the real-model dump
  `product-real-models-main-c0f2e5f9b.dump`. The migration ledger is at shared 158, tenants 148.
- **Images.**
  - Worker: `ghcr.io/elitea-ng/elitea-worker-rust:session-deadlock-a97c56daf`, image
    `sha256:a17724997fc06bafeda28096dfecb743f45035e142e50a518db43f381b85bea1`. It was built from this branch, which
    contains #1160.
  - Main, web, gateway, scheduler and subapp-host: `main-c0f2e5f9b-verify`.
  - Mocks and engines: `main-0def77b22-verify`.
  - No migrations differ between c0f2e5f9b and the base of this branch, a1d38fb4d.
- **Binary check (§3b).** The extracted `/usr/local/bin/elitea-worker-rust` contains `elitea_agent.driven_task_failed`
  and `SET LOCAL idle_in_transaction_session_timeout`. The incident image (`nestedapp-restart-fix`) contains neither.
- **Journeys.**
  - Fixture: pipeline 138, version 145. Agent 137 (version 144) runs on `eu.anthropic.claude-haiku-4-5-20251001-v1:0`
    through the real gateway. The LLM mock received 0 requests.
  - Three persistent chats (880, 881, 882) ran the full flow.
  - The flow: first message → card 1 → answer card 1 → answer card 2 → both node results.
  - In "answer card 1", node 1 completes and the child of node 2 asks card 2. This is the incident step.
- **Executions.** All `SUCCEEDED`. The table shows seconds from admission to settlement.

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

- **After reload.**
  - Each chat showed the user message and the final message with both blocks.
  - The blocks were OVERRIDE CALL with `Audience=operators; tone=brief`, and DEFAULT CALL with
    `Audience=users; tone=formal`.
  - The card in chat 880 also survived a reload before it was answered.
- **Server side.**
  - No `agentstate` backend was ever `idle in transaction`.
  - The Worker logged 0 `lock_not_available`, `storage_unavailable`, `driven_task_failed` or panic lines.
- **Final image, second pass.**
  - The evidence above is from image `a97c56daf`. After the review fixes, the stack was rebuilt on the final commit:
    Worker `session-deadlock-cd18caa56`, image
    `sha256:906f2eff85db90875dab08d6c8211cf0a305dd8319b96b81ae11645daeb7a67d`.
  - Its binary contains the two strings above. It also contains `an agent task panicked; the execution fails closed`.
  - **An interrupted run, recorded honestly.** The first journey on this image (chat 880) was cut short by the host,
    not by the fix.
    - What happened: the Mac went to sleep on battery from 22:16 to 22:37 UTC (`pmset` log: "Maintenance Sleep" with
      second-long dark wakes). The claim lease of Main expired while the Worker was frozen.
    - What the Worker logged: the stream ended with `stream_disposition="lease_failed"` / `claim_lease_control_failure`
      at 22:37:58. The lifecycle span shows only about 20 s awake (`time.busy=242ms time.idle=19.7s`).
    - The Worker retained the command for recovery. Main retired it (`agent_delivery.authorization_terminal_retired`).
      Execution `0a2f0c0ec5b83bf197e8caa066d070f9` settled as FAILED.
    - What it was not: no agentstate backend was ever `idle in transaction`. This is the expected lease-loss path. It is
      not the session-lock hang.
  - **Clean pass.** The host stayed awake. A fresh journey in chat 881 ran the full flow. The steps were: first message, card 1, answer
    card 1, card 2, answer card 2, and both node results. All three executions `SUCCEEDED`:

    | Execution | Seconds |
    | --- | --- |
    | `039624375e13953a332ea986eef5dff4` | 2.9 |
    | `15cbc1e8aa8ca4216f5df6fd3aee6a23` | 3.8 (the incident step) |
    | `9e1bd650e821b39fddf8d9f561dff3a8` | 1.9 |

    - After reload, the final message showed both the OVERRIDE CALL and DEFAULT CALL blocks.
    - The log had 12 Haiku 4.5 model lines and 0 LLM-mock requests.
    - There were 0 `idle in transaction` agentstate backends.
    - There were 0 lock-timeout, driven-task or panic log lines.
- **Follow-up pass: A/B against the unfixed Worker.**
  - Setup:
    - Image `session-deadlock-48e2832d0`, `sha256:b045d19144e788806cafd9462eb1d14212da6fa873081c1c3bde25fad0f262b4`.
      The binary check found all three branch strings.
    - The stack used a fresh restore of the real-model dump. Haiku 4.5 ran throughout (54 model log lines, 0 LLM-mock
      requests).
    - Only the Worker image changed between runs. The control is unfixed merged `main`
      (`main-c0f2e5f9b-verify`, `sha256:500d9904…`).
  - **Baseline.** On the fixed image, with no injected contention, chat 880 ran the incident journey. Three turns
    `SUCCEEDED` in 3.4, 3.5 and 1.8 s. The result persisted after reload.
  - **Forced contention.** A test-only trigger on the throwaway agentstate DB (never committed) makes the race likely.
    - It sleeps 300 ms inside every child model-scope event insert, while the child holds the root writer `FOR SHARE`.
    - It sleeps 150 ms inside every root event insert, so forwarded node events back up.
  - **Incident pipeline under contention.** The unfixed Worker still completed the sequential incident pipeline
    (chats 881 and 882, 5–10 s per turn). With sequential nodes, a forwarded event is rarely queued in the same poll as a
    child append.
  - **Parallel sub-agents under contention.** Agent "Full Name Resolver" (application 9) calls its two sub-agents in
    parallel, which supplies concurrent senders. The prompt shape was the same in every run.

    | Worker | Attempts | Hung | Detail |
    | --- | --- | --- | --- |
    | Unfixed `main` | 3 | **2** | chats 884 and 889, executions `6da40a37…` and `3254ee4f…` |
    | Fixed | 5 | **0** | chats 885–887 and 890–891: `b68a046b…` 14 s, `772db6f9…` 15 s, `3a1e844e…` 13 s, `6ee87556…` 16 s, `02c6d68e…` 14 s |

    - Both unfixed hangs showed the incident signature. It held for over 45 s and 60 s respectively.
    - A backend sat `idle in transaction` after the child `INSERT INTO agent_session_events`. Another backend waited on
      the root `agent_session_writers` row lock, blocked by it (`pg_blocking_pids`).
    - The Worker sat at 0% CPU with no log lines for the execution.
    - Only a Worker restart ended each hang, as in the incident.
    - The answers of the fixed runs were complete and persisted after reload.
  - **Lock bound in the browser.** Chat 892, same agent, no trigger:
    - Turn 1 `SUCCEEDED` (`94b71aa3…`).
    - Then a separate `psql` session held the root writer row of that conversation `FOR UPDATE` for 90 s.
    - Turn 2 (`8e7e38fe…`): the writer activation waited on the held row. It then failed after the 10 s bound
      (`native_agent.dependency_unavailable`). The chat showed "A required runtime dependency is unavailable." with
      "Details for support", instead of spinning forever.
    - After the release, the retry (`c257ec8c…`) `SUCCEEDED` in 14 s with the conversation history intact. After reload,
      the chat showed all three turns.
- **Teardown.** Each pass ended with `down -v` on the stack, after the evidence was recorded.

## Fixtures

- **Browser.** The fixture came from the restored real-model dump, unchanged. The chats were created in the UI through
  the Chat button of the pipeline.
- **Tests.** Test data was created in code against isolated, throwaway PostgreSQL databases.

## Follow-ups

This PR completes all five follow-ups from the first review. They are the fatal-lease path, synchronous teardown in
`drain_child`, full-channel end-to-end tests, the task-per-level budget test, and the structural guard. Two items
remain:

- **Lease-ordering observability.**
  - Code enforces the fatal-lease order, but a test cannot observe it in-process.
  - On lease loss, the Worker drops the agent run before it joins the lease actor. A write held inline rolls back at the
    drop. A write inside a driven pull rolls back when the Tokio runtime next runs that task.
  - A process-level crash and lease test in the crash-recovery suite could observe this with real lock waits.
- **Root-cause trade-off.**
  - The guard is a source scan. Code enforces it, and a mutation check covers it.
  - A type-level alternative exists. It would make agent `EventStream`s non-pollable outside `DrivenEventStream`.
  - That alternative needs a wide type change.
