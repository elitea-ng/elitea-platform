# Nested agent compaction acceptance

## Settings and ownership

`src/agents/assembly.rs::from_nested_version_for_type` inherits the admitted parent context policy and dedicated summary-model selection.
It recalculates the budget against the child's authorized task-model limits and output reserve.
A nested child's saved context policy does not override this inherited policy.
This follows the earlier inheritance agreement; explicit child overrides remain a separate product decision.

`src/agents/model_scope.rs` gives each invocation separate history and checkpoint identity.
The identity includes the execution, generation, child session, and agent identity.
`src/agents/application_tools.rs::drain_child` forwards execution events to the trace and returns the selected child answer to its caller.
The parent does not receive the child's entire tool history as its tool result.
The child can compact repeatedly within one delegated task.

The current functional reference is `projects/elitea-sdk/elitea_sdk/runtime/tools/application.py`.
Its `client.application` call constructs the saved child application separately.
This reference defines delegated application behavior; it does not define the new compaction algorithm.
The existing [durable compaction mapping](durable-context-compaction.md) records ADK and PostgreSQL ownership.

## Live rehearsal

Fresh headed Playwright opens chat 614 and sends a task to saved parent 41, version 48.
The parent delegates once to child 40, version 47.
Both use the configured Haiku task model, with a 128,000-token window and 4,000 reserved output tokens.
The caller selects Full mode and the dedicated Luna summary model, with 8,192 maximum output tokens.
The child has no separate saved compaction policy.
The fixture serves fictional, read-only records over the existing trusted rehearsal TLS route.
No browser requests are intercepted.

Execution `c98e88dde9166d84c6de435d5f972537` settles as `SUCCEEDED`.
The durable tool ledger contains one `elitea_agent_40_v_47` call and 24 `read_compaction_record` calls.
All 25 records have `is_error = false`.

The child's context events carry the same nested call identity for both compactions:

| Scope | Before | After |
| --- | ---: | ---: |
| Child, first compaction | 151466 | 1137 |
| Child, second compaction | 152203 | 1206 |
| Parent, initial/final measurement | 354 | 766 |

These values are estimated input tokens, not provider usage counters.
The child events retain Full mode, 122,720 usable input tokens, and a 110,448-token trigger.
The parent emits no compaction event.
Its final answer preserves CEDAR-731, teal, archive verification, the handoff step, and the 24-read count.
The browser reports no page errors, and its final answer remains exactly equal after reload.
The final screenshot is inspected visually.

The run uses worker image `sha256:0c1c3a9673a0855142988cc5856f82f1ee297a68925fac34fd27d2b3070705ac`.
It uses UI image `sha256:84af8476b3ff188e2909552a748d180c622e0d2ac328f115331ee4924dc495a8`.
Local evidence files use the `elitea-nested-compaction` prefix.

## Limits

An earlier chat 613 selected the child directly and did not test nesting.
It is not counted as nested acceptance.
This run proves one nested child, repeated compaction, parent isolation, and stable final-answer reload.
It does not prove sibling concurrency, nested worker replacement, pipeline-model recovery, or a million-token provider route.
