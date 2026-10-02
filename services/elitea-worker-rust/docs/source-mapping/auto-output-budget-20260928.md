# Auto output and combined context capacity

## Source mapping

The current SDK selects the dialect in `elitea_sdk/runtime/clients/client.py::get_llm`.
It treats missing or `-1` output limits as Auto.
It omits optional OpenAI limits and resolves the required Anthropic limit from model configuration.
The model name and compatibility override jointly select the dialect.

Main freezes this choice in `internal/application/agentexecution/tools.go`.
Rust admits the frozen model in `src/agents/assembly.rs::validate_model`.
`src/agents/context_budget.rs::RequestContextBudget` owns combined-window arithmetic.
`src/transport/openai_compatible_facade.rs::encode_request_body` owns the compatible wire cap.

## Change

Previously, Auto reserved the complete catalogue output maximum for every compatible call.
A 272,000-token Balanced window with a 128,000-token output maximum left 141,280 tokens for input.
Auto now reserves a minimum output allowance of 1,024 tokens, bounded by the model output maximum.
The same Balanced window leaves 268,256 tokens for input after its 2,720-token safety margin.
Compaction starts at 241,431 estimated input tokens, or 90% of usable input.

Each compatible request receives an output cap based on remaining combined capacity.
The cap cannot exceed the catalogue output maximum.
Short inputs can still receive the complete model output allowance.
Explicit output limits retain their original reservation and wire cap.
Previously admitted requests without catalogue limits retain Auto omission.
Summary requests retain their independent explicit output allowance.

The transport measures the complete serialized body with the maximum output field present.
If necessary, it reduces that field and serializes again into the same buffer.
This avoids retaining a second encoded request allocation.
The shorter numeric field cannot increase the input estimate.
Admission still checks input and transport bounds before dispatch.

Main now matches SDK and Rust dialect selection when resolving Auto.
An OpenAI model without the compatibility flag no longer receives Anthropic normalization.
Main retains the native numeric fallback and adds `max_tokens_auto: true` to frozen settings.
Rust reads that marker for saved applications and ad-hoc chat.
Native Anthropic calculates its numeric cap from remaining combined capacity.
Legacy reasoning reserves at least its thinking budget plus one output token.
Adaptive thinking keeps the ordinary minimum output allowance.
Explicit native caps retain the existing reasoning-padding behavior.

## Verification and limits

Compatible transport tests inspect actual dispatched JSON for short input, Balanced pressure, Full pressure, and an explicit cap.
They assert input estimate plus output cap plus safety margin remains within the selected combined window.
They verify the model maximum remains available with short input.
Focused Main tests cover inferred OpenAI, forced compatible Claude, and native Anthropic.
No database schema changes occur.
All 30 compatible-facade tests and 10 context-budget checks pass.
Strict Rust library and test Clippy checks pass.
The full library run passes 1,223 tests and ignores one manual diagnostic benchmark.
Seven local HTTP fixture tests fail at sandbox listener creation.
The permitted toolkit rerun passes all 372 tests, including those seven cases.

This change does not replace estimation with a tokenizer or provider usage.
Provider usage arrives after dispatch and cannot alone admit newly added content.
The status contract adds optional `auto_output`; absent means the existing fixed reservation.
Main validates this field and preserves it in the existing context read model.
The UI distinguishes minimum Auto allowance from a fixed output reservation.
The coordinated rehearsal deployment completes. Browser acceptance remains open.
The separate Full-window test in chat 706 uses an explicit 8,192-token cap and does not prove this Auto policy.

## Native and UI verification

All 1,232 Rust library tests pass; one manual diagnostic benchmark remains ignored.
The 26 native-facade tests also pass after the model-family update.
Native tests cover short requests, near-boundary requests, adaptive thinking, legacy thinking, and overfull histories.
Overfull histories remain measurable for compaction and cannot dispatch before admission passes.
The 58 context-widget tests and UI typecheck pass.
Focused Go tests cover marker preservation in saved and ad-hoc settings, plus optional status-field validation.

The native facade now recognizes Fable 5 and Fable 5.1 as adaptive-thinking models at the user's request.
Opus 5.5 already matches the Opus 5 family rule.
Regression cases verify adaptive thinking and sampling rejection for hyphen, dot, and underscore version spellings.
These tests establish local request behavior, not live availability of those model names.

Use a coordinated rehearsal rollout with no active executions.
Main must accept the optional status field before the new worker emits it.
Older Rust workers do not understand the new frozen Auto marker.
Retain the numeric fallback for SDK workers, which read the existing numeric setting.
This change adds no database tables or migrations.

## Chat settings admission

The UI sends `max_tokens: -1` for Auto through `agentLlmSettings.ts::toLlmSettingsBody`.
The first deployed Auto fixture exposes HTTP 400 before execution starts.
Main incorrectly applies positive identity validation to this output control.
`conversations/participant_settings.go::normalizeParticipantLLM` now admits `-1` only for `max_tokens`.
Project and version identities remain positive integers.
Explicit output caps retain the existing signed 32-bit integer bound.
Zero, other negatives, fractions, booleans, and invalid strings remain invalid.
Both read and write normalization preserve the Auto sentinel for downstream Rust assembly.
The focused conversation package tests pass.
The admission correction is deployed. Chat 707 saves Auto settings successfully.

## Deployed Auto acceptance

Commit `bc46563b2` deploys the chat-settings correction after `bc898d0df` deploys Main, Rust, and UI Auto support.
Fresh headed Playwright sessions use real rehearsal endpoints without browser response mocks.
Synthetic history is inserted only into new acceptance chats.

- Chat 707 succeeds with Balanced Auto and preserves all four project facts after reload.
  Its estimated input is 239,637 tokens against 268,256 usable tokens.
  This stays below the 241,431 compaction trigger and verifies ordinary Auto execution.
  Provider usage reports 206,230 input tokens and 42 output tokens.
- Chat 708 succeeds with Full Auto and the configured 1,000,000-token window.
  Compaction reduces estimated input from 956,800 to 48,570 tokens.
  Provider summary usage reports 852,726 input tokens and 396 output tokens.
  The resumed call reports 41,968 input tokens and 42 output tokens.
  The answer retains CEDAR-731, teal, completed archive verification, and the pending handoff note.
  Reload retains the answer, 5% context display, and the Auto allowance explanation.
- The first chat 709 execution does not prove native Anthropic acceptance.
  The browser submits the configured default fixture model instead of the intended Haiku model.
  Gateway metadata confirms `CONTINUATION-REPAIR-FIXTURE`; its terminal success is not a native-provider pass.
  The test used a shortened alias absent from the shared catalogue.
  Correcting it to `eu.anthropic.claude-haiku-4-5-20251001-v1:0` restores the intended native selection.
  Native Auto then succeeds with stable answer and context-indicator reload.
  Its estimated input is 111,577 tokens against 125,696 usable tokens.
  Provider usage reports 98,796 input tokens and 70 output tokens.

The Full summary takes 27,781 milliseconds; the resumed call takes 4,224 milliseconds.
These synthetic checks do not establish concurrency capacity or tokenizer accuracy.
Provider usage and pre-dispatch estimates remain distinct measurements.

### Native threshold and separate summarizer

A follow-up in chat 709 crosses the native Auto compaction threshold.
The first summary request returns HTTP 503 with `server_error` from Luna.
Execution `49b5b78ba731eeca0890f3251ef899b0` fails before summary completion.
The browser preserves the availability message and support details after reload.

A new short user request retries from the preserved history after that confirmed terminal failure.
Execution `ccaaff493662e385c3bc89e1d2b18790` succeeds.
Estimated input falls from 114,953 to 9,289 tokens.
Luna summary usage reports 100,433 input tokens and 565 output tokens in 7,785 milliseconds.
Native Haiku continuation reports 7,723 input tokens and 62 output tokens in 1,705 milliseconds.
The answer preserves all four required facts; reload retains the answer and 7% context indicator.
This verifies a separate compatible summarizer with a native operating model.
It proves a successful user retry after provider failure, not automatic provider retry.

All three Auto scenarios have live browser and reload evidence.
Full and native follow-up scenarios also exercise actual compaction.
Balanced Luna ordinary execution stays below its threshold.
The earlier alias mismatch remains an invalid native test, not an implementation pass.
