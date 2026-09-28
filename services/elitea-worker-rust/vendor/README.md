# ADK runtime extensions

These packages preserve the published ADK 2.2.0 dependency graph.
History extensions change `adk-agent/src/llm_agent.rs` and `adk-runner/src/runner.rs`.
The optional sandbox supervisor extension changes `adk-sandbox/src/workspace/docker.rs`.
Each package includes the upstream Apache 2.0 license.

Upstream repository: https://github.com/zavora-ai/adk-rust

Published source commit: `74765eb04930648795b53c3db689ddd10032c31e`.

| Package | Published archive SHA-256 |
| --- | --- |
| adk-agent 2.2.0 | `e150b28775ad28d4a4d0a49267a64298052346b8faf6f93ce075d025faecdcb8` |
| adk-sandbox 2.2.0 | `eaca26dc4fce7f1dcef2a4ca6d6ca9f35ce4b6a6850b636a66de295d10a09596` |
| adk-runner 2.2.0 | `31262e6bb997df8daa5bed8cc54bdefb3d92c84014c8715b937eb51731446bb0` |

`retain_prepared_history` lets the agent retain history prepared by successful model callbacks.
Its default is false. Elitea enables it when its durable compaction callbacks own prepared history.

`with_session_event_refresh` reloads the session view after each persisted event.
Its default is false. Elitea enables it to use its durable active-history projection.
All stream fragments still reach the caller. Partial fragments do not accumulate in the refreshed session.
Storage failures stop execution instead of silently using an obsolete session view.

These extensions do not replace the ADK execution loop or implement another summarizer.
The initial session snapshot remains allocated by the native mutable session.
The retention tests do not prove a process RSS bound.

For an ADK upgrade, compare both modified files with these published sources.
Run the worker history, recovery, interruption, and stream delivery tests.
Remove these extensions when upstream supplies equivalent verified behavior.


The sandbox extension adds an explicit non-root offline Code-job policy with finite
CPU/memory limits, no extra swap, a PID limit, read-only root, restricted temporary
mounts, and no added capabilities. It bounds combined decoded output to 1 MiB,
fails on stream/input errors, terminates containers on command failure/timeout,
attempts cleanup after preparation failure, and retains session handles on removal
failure. Shell file-write destinations are arguments, not interpolated commands.
Code-job image snapshots are rejected because tmpfs contents are not captured.

This dependency is optional under `sandbox-supervisor`; the default worker does
not gain Docker access. The upstream in-memory session map is not a durable job
registry. Caller cancellation, lost create acknowledgements, supervisor restart,
persistent receipts, and Kubernetes execution still require supervisor ownership
and integration tests before production admission.
