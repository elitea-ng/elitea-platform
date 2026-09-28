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
Deployment and browser acceptance remain open.
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
