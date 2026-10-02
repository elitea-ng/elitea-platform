# Structured summary work order

## Evidence

Chat 603 completes compaction after a worker restart.
Its saved summary retains the explicit handoff task but also turns pending approval into an open task.
The first answer chooses approval as the next step.
A follow-up retrieves the handoff task correctly, so this is not missing persisted content.
See [retention evidence](context-event-retention-20260921.md).

## Source mapping

Rust `src/agents/context_summary.rs::CONTRACT` owns the platform continuation-summary instructions.
Rust `src/agents/context_compaction.rs` uses the same contract for batch summaries, merges, and bounded correction.
The existing ADK `LlmEventSummarizer` remains the model invocation mechanism.
The structured output schema and durable summary version remain unchanged.
Current-platform compaction is not the target behavior under the user's explicit exception.
The current-to-new compatibility references remain in [summary compatibility](context-summary-compatibility-20260918.md).

## Change

Preserve explicitly requested next steps and their order before suggestions.
Keep pending status under unresolved issues unless the user requested an action.
Do not convert pending approval into a request to obtain approval.
Do not replace explicit work with an inferred prerequisite.
These rules apply to platform instructions; user summary guidance remains additive.

## Verification

A real rehearsal Luna request uses native structured output and fictional project notes.
The provider reports 17,648 input tokens and 433 output tokens.
The response lists only the handoff task under open work and explicit next steps.
Approval remains an unresolved status, with no inferred request to obtain approval.
This is one provider contract check, not complete repeated-compaction or browser acceptance.
Nine structured-summary tests and the Rust formatting check pass.
The contract change is deployed to rehearsal.

## Deployed browser verification

Worker image `sha256:a3303ba6acec197d923d049b14db6914e5331a4824653ed3474db9fb5cef5456` includes the fix.
A fresh headed Playwright browser submits the synthetic history in chat 604 without intercepted requests.
Execution `413364997198a4cc30bf51f7949e89c4` compacts 353,853 estimated input tokens to 875.
The route has a 400,000 combined window, an 8,192 output reserve, and a 272,000 input ceiling.
Luna uses its separately configured summary-model budget.

The first answer reports `CEDAR-731`, `Teal`, `Archive verification complete`, and `Prepare the handoff note`.
The answer remains identical after reload, and the browser reports no page errors.
The context panel reports the compacted state and provider input ceiling.
This verifies the observed work-order regression with real providers.
It does not prove perfect summary quality for all histories or close the other gate 4 requirements.
