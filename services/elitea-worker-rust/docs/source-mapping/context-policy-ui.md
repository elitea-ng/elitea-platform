# Context policy controls and usage presentation

Status: UI component and fresh-browser verification, 2026-09-17. Live worker occupancy and compaction notifications remain open.

## Functional source mapping

| Current-platform source | New source and behavior |
| --- | --- |
| EliteaUI `src/[fsd]/features/settings/ui/memory/MemoryContextManagement.jsx` edits account defaults. | `apps/elitea-web/src/features/settings/ui/memory/MemoryContextManagement.tsx` exposes Balanced and Full instead of the legacy numeric context limit. The shared control also serves the older profile route and chat-rail editor. |
| EliteaUI `MemorySummarization.jsx` edits summary instructions and output length. | `MemorySummarization.tsx` retains those settings and adds `SummaryModelSelect.tsx`, using the existing authorized model catalogue. The selection is the exact model-name/project pair. |
| EliteaUI `src/[fsd]/widgets/context-budget/ui/ContextBudgetCompact.jsx` and `ContextBudgetTooltipContent.jsx` show usage. | `widgets/context-budget` distinguishes unknown usage from zero and adds a composer button with a keyboard- and touch-accessible details dialog. |
| Centry account and conversation context settings retain user defaults and overrides. | UI serializers write the preset to the existing context settings. Main's [immutable policy delivery](context-policy-delivery.md) resolves and freezes the settings for Rust. |
| Current SDK compaction implementation is a reference exception agreed by the user. | Rust's [combined budget](model-context-budget.md) and [durable compaction](durable-context-compaction.md) define the new semantics. The UI does not retain the old 64k output counter as a context window. |

## Wire and presentation behavior

New settings use `budget_mode: balanced` or `full` and omit `max_context_tokens`.
Old numeric records remain readable and display Balanced; saving the form changes that account preference without migrating historical records.
The rail editor preserves other account fields and the existing enabled flag.
Choosing the chat model clears the separate summary-model owner; choosing a dedicated model sends its numeric project ID.
An unavailable saved model selection stays visible until the user chooses another model.

Balanced is capped at 272,000 total tokens and the model window. Full uses the model window.
Both reserve output and a safety margin internally. The UI explains the 90-percent usable-input trigger.
The warning uses the unrounded ratio, so rounding 89.6 percent for display does not incorrectly trigger a warning.
Unknown runtime occupancy shows “Usage not yet measured”; it does not imply zero usage or disabled compaction.
The root composer and the rail reuse the same status query. Nested activities do not receive separate meters.

The next integration must project current worker measurements through accepted durable events, with exact execution and model scope.
Show a brief “Compacting context…” status for a nested agent, and a detailed input/output-reservation/window breakdown for the main chat.
Do not aggregate child occupancy into the parent, use cumulative billing counts as occupancy, or interpret a pipeline container as an LLM context.
Status transitions must survive reconnect/recovery and settle when the execution stops or fails.
This slice does not yet implement those worker events or claim live compaction notification acceptance.

## Verification and delivery

- 295 focused UI tests pass across 44 files, including account serialization, identical model names in different projects, unknown usage, preset editing, and composer keyboard interaction.
- TypeScript and changed-source Oxlint pass. The app build succeeds with the existing bundle-size warning.
- A fresh headed Chromium session logs into the rehearsal, opens Memory settings, switches Full/Balanced, and opens the summary-model selector with five catalogue options.
- An existing copied-skill test conversation opens the composer details with Enter and closes with Escape. The unknown reading is visible. There are no browser page errors.
- Browser reads use the rehearsal backend. Profile PUTs are intercepted to inspect request shape without activating new settings against the older deployed Main. This is UI/request-contract proof, not persisted full-stack acceptance.
- The regenerated Go API compiles; focused Main context and execution tests pass.

The temporary evidence prefix is `elitea-point4-context-`; browser results are in `context-browser-result.json` under that prefix.
No application schema change or deployment occurs in this slice.
Deploy compatible Rust, then Main and UI, before the real save/reload/model-loop/recovery acceptance.
Tool-output editing remains separate point 4 work; its existing control is not evidence of a working Rust implementation.
