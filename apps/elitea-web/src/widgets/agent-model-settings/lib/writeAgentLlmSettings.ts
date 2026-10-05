/**
 * The one place a chosen catalogue row plus a set of dialog values becomes a
 * `version_details.llm_settings` object.
 *
 * Port of the baseline's `generateLLMSettings` + `cleanLLMSettings`
 * (`apps/elitea-ui/src/[fsd]/shared/lib/utils/llmSettings.utils.js:24-65`),
 * with the one rule the baseline did not have. The baseline wrote
 * `temperature` unconditionally and ADDED `reasoning_effort` on top for a
 * reasoning model; the Rust worker refuses that profile outright —
 * `services/elitea-worker-rust/src/agents/assembly.rs` returns
 * `invalid_profile` when `temperature` is present alongside a
 * `reasoning_effort` that is not `"none"`. Enforcing it here rather than at
 * the call site is the point of this module: a caller that forgets produces
 * an agent that saves with a 200, freezes fine, and then fails on its first
 * message with an error naming neither field.
 *
 * `max_tokens` fails the same way and for the same reason, which is why its
 * range is enforced here as well as in the dialog — see {@link MIN_MAX_TOKENS}.
 */
import { DEFAULT_MAX_TOKENS, DEFAULT_TEMPERATURE } from '@/shared/lib/constants';
import {
  selectEffortAndTemperature,
  toAgentReasoningEffort,
  type AgentLlmSettings,
} from '@/shared/api/agentLlmSettings';

/** The three facts about the chosen catalogue row that decide the profile. */
export interface ChosenCatalogueModel {
  readonly name: string;
  /**
   * The catalogue's own `project_id`. Typed loosely on purpose:
   * `ConfigModel.project_id` (`shared/api/configurationsApi.ts`) is declared
   * `string`, the Go catalogue marshals an int32, and the worker parses the
   * field with `positive_u32` — which hard-fails on a string. `Number(...)`
   * below is the single conversion point, so no caller has to know that.
   */
  readonly projectId: string | number | undefined;
  readonly supportsReasoning: boolean;
}

/**
 * Whatever tunables the caller is carrying: the version's current settings,
 * or the settings dialog's own values. `LLMSettingsValues` also carries
 * `steps_limit`, which is not read here, and that is deliberate — the
 * agent's step limit belongs to `version_details.meta` where
 * `ApplicationAdvanceSettings` already owns it.
 */
export interface AgentLlmTunables {
  readonly max_tokens?: number | string | undefined;
  readonly temperature?: number | undefined;
  /**
   * Typed as a bare `string` because that is what the dialog hands back
   * (`LLMSettingsValues.reasoning_effort`, written by `ReasoningSlider`);
   * `toAgentReasoningEffort` is the single narrowing point, and a value the
   * worker would refuse is dropped rather than written.
   */
  readonly reasoning_effort?: string | undefined;
}

/** `parse_temperature` in `assembly.rs` accepts a finite `0.0..=1.0` and refuses everything else. */
const MIN_TEMPERATURE = 0;
const MAX_TEMPERATURE = 1;

/**
 * The worker admits `-1` (Auto) or a POSITIVE integer and nothing else:
 * `normalized_max_tokens` short-circuits on `-1` and otherwise routes the
 * value through `positive_u32`, which refuses `0` (`*value > 0`).
 *
 * `0` is the one that mattered. The field's own parse admits the string
 * `"0"`, `Number.isFinite(0)` is true, and the version then SAVES with a
 * 200 — after which every single turn is refused `invalid_profile` by a
 * message that names no field. So a value outside the admitted range falls
 * back to Auto here rather than being written: the same "a blank field
 * becomes Auto, not a number nobody chose" rule this function already had,
 * extended to the values the worker will not run. The dialog's own Apply
 * button is disabled for them too (`lib/validation.ts`'s
 * `BELOW_MIN_TOKENS`), so this is the second line, not the first — reachable
 * from `handleSelectModel`, which re-writes the profile from a stored value
 * no dialog has validated.
 */
const MIN_MAX_TOKENS = 1;

function readMaxTokens(raw: number | string | undefined): number {
  const parsed = typeof raw === 'string' ? Number.parseInt(raw, 10) : raw;
  if (parsed === undefined || !Number.isFinite(parsed)) return DEFAULT_MAX_TOKENS;
  if (parsed === DEFAULT_MAX_TOKENS) return DEFAULT_MAX_TOKENS;
  if (!Number.isInteger(parsed) || parsed < MIN_MAX_TOKENS) return DEFAULT_MAX_TOKENS;
  return parsed;
}

/**
 * Clamped, because the two ends disagree: `CreativitySlider` runs to 2.0
 * (`widgets/llm-model-selector/ui/settings/CreativitySlider.tsx`, `max={2}`)
 * while the worker's ceiling is 1.0. Saving what the slider allows would
 * produce a version that refuses its first turn, so the top of the slider's
 * travel is pinned to the top of the accepted range instead.
 */
function readTemperature(raw: number | undefined): number {
  if (raw === undefined || !Number.isFinite(raw)) return DEFAULT_TEMPERATURE;
  return Math.min(Math.max(raw, MIN_TEMPERATURE), MAX_TEMPERATURE);
}

/**
 * `undefined` when the row cannot produce a valid profile — an empty name, or
 * a `project_id` that is absent or not a positive number. The caller emits
 * nothing in that case, which leaves the version carrying whatever it had:
 * for a version that carried nothing, that is the platform's own fallback to
 * the project catalogue default, and that fallback answering turns today is
 * exactly what must not be traded for a profile the worker will refuse.
 */
export function writeAgentLlmSettings(
  model: ChosenCatalogueModel,
  tunables: AgentLlmTunables | undefined,
): AgentLlmSettings | undefined {
  const projectId = Number(model.projectId);
  if (model.name === '' || !Number.isFinite(projectId) || projectId <= 0) return undefined;
  return {
    model_name: model.name,
    model_project_id: projectId,
    max_tokens: readMaxTokens(tunables?.max_tokens),
    ...selectProfileTunables(model, tunables),
  };
}

/**
 * The `temperature` XOR `reasoning_effort` half of the profile, decided by
 * the CHOSEN model's capability rather than by which keys the caller happens
 * to carry.
 *
 * That is what makes the exclusion survive a model switch. The settings
 * dialog shows one slider or the other on `supports_reasoning`
 * (`LLMSettings.tsx`) but seeds `temperature` on mount for EVERY model
 * (`computeMissingDefaults`) whether its slider is on screen or not, so the
 * values handed back for a reasoning model routinely carry both. Picking by
 * capability means applying a temperature to a version that had a
 * `reasoning_effort` drops the effort, and moving that version onto a
 * reasoning model drops the temperature — in each case the key the newly
 * chosen model cannot run with is the one that goes.
 *
 * A reasoning model with no effort to carry emits neither key: `None` is a
 * profile the worker runs, and inventing an effort here would author a
 * budget the user never picked.
 */
function selectProfileTunables(
  model: ChosenCatalogueModel,
  tunables: AgentLlmTunables | undefined,
): { temperature?: number; reasoning_effort?: ReturnType<typeof toAgentReasoningEffort> } {
  if (model.supportsReasoning) {
    return selectEffortAndTemperature(undefined, toAgentReasoningEffort(tunables?.reasoning_effort));
  }
  return selectEffortAndTemperature(readTemperature(tunables?.temperature), undefined);
}
