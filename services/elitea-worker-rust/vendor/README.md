# ADK history retention extensions

These packages preserve the published ADK 2.2.0 dependency graph.
Only `adk-agent/src/llm_agent.rs` and `adk-runner/src/runner.rs` contain source changes.
Each package includes the upstream Apache 2.0 license.

Upstream repository: https://github.com/zavora-ai/adk-rust

Published source commit: `74765eb04930648795b53c3db689ddd10032c31e`.

| Package | Published archive SHA-256 |
| --- | --- |
| adk-agent 2.2.0 | `e150b28775ad28d4a4d0a49267a64298052346b8faf6f93ce075d025faecdcb8` |
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
