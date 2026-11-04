import type { LlmSettings } from '@/shared/api/generated/model';

/**
 * `version_details.llm_settings` — the model an agent or pipeline version
 * runs on, as its author picked it.
 *
 * Lives in `shared/api/` rather than in `entities/application-form` because
 * three layers need the same shape and none of them may import the others:
 * the draft (`entities/application-form/model/initialValues.ts`), the two
 * page mappers (`pages/agents/lib/`, `pages/pipelines/lib/`) and the model
 * picker that writes it (`widgets/`). `entities/application-form/index.ts`
 * is at the §3.5 slice-public-api cap (exactly 20 exports), so re-exporting
 * from there was not available even for the two layers allowed to reach it,
 * and `shared/` is the only layer everything above may import (R-L1,
 * `.dependency-cruiser.cjs:20-27`). `shared/` index budgets do not apply —
 * `isSliceIndex` in `scripts/lib/budgets-core.mjs` matches only
 * `features|entities|widgets|processes`.
 *
 * **Exactly these keys, and no others.** The Rust worker validates the
 * profile against a closed allow-list and refuses the whole turn with
 * `unsupported_profile` when it meets a key outside it
 * (`services/elitea-worker-rust/src/agents/assembly.rs`, `validate_model`).
 * Two keys that look available are deliberately absent:
 *
 * - `openai_compatible` — Configurations-owned. elitea-main deletes whatever
 *   the version carries and re-derives it from the project catalogue row
 *   (`services/elitea-main/internal/application/agentexecution/tools.go`,
 *   `resolveCurrentAgentModel`), so authoring it here can only ever be
 *   ignored or wrong.
 * - `top_p` / `top_k` — present in `api/openapi/v2.yaml` and therefore in
 *   the generated `LlmSettings` zod above, but NOT in the worker's
 *   allow-list. A version carrying `top_p` saves with a 200, freezes fine,
 *   and then fails at the worker on the first message. Do not surface a
 *   control for either.
 *
 * `reasoning_effort` IS modelled, because dropping it corrupts versions this
 * app did not author. The worker's `APPLICATION` allow-list carries it
 * (`assembly.rs`, `ModelFieldNames::APPLICATION`) and honours it, so a
 * version can legitimately hold one; before it was modelled here, seeding an
 * edit through `toAgentLlmSettings` stripped it and EVERY subsequent save
 * wrote the version back without it, silently moving a reasoning agent onto
 * the model's default effort. It is deliberately NOT in the generated
 * `LlmSettings` zod object — that is a closed `zod.object` with no such
 * field, and adding it needs a `v2.yaml` edit (its own PR — a spec edit trips
 * six separate CI gates). Nothing parses a write body through that schema at
 * runtime (`shared/api/generated/mutator.ts` hands the already-stringified
 * body straight to `eliteaFetch`), so the key reaches the wire today; until
 * the spec edit lands, {@link LlmSettingsBody} is the widened body type that
 * says so out loud rather than a cast at each call site.
 */
export interface AgentLlmSettings {
  /**
   * The catalogue alias, e.g. `qwen3.5`. Required: the worker's
   * `validate_model` rejects an absent or blank name outright, and
   * elitea-main only substitutes the project's catalogue default when the
   * name misses the catalogue entirely.
   */
  readonly model_name: string;
  /**
   * A JSON **number**, not a string. `ConfigModel.project_id`
   * (`shared/api/configurationsApi.ts`) is declared `string`, but the Go
   * catalogue marshals an int32 and the worker parses this field with
   * `positive_u32`, which hard-fails on a string. Always coerce with
   * `Number(...)` at the point the catalogue row is read.
   */
  readonly model_project_id: number;
  /** `-1` means "let the model decide"; otherwise a positive cap. */
  readonly max_tokens: number;
  /**
   * Mutually exclusive with {@link AgentLlmSettings.reasoning_effort} at the
   * worker, which returns `invalid_profile` when both are present. Omitted
   * (not `undefined`) for a reasoning model, which is why this is an optional
   * key rather than a nullable one.
   */
  readonly temperature?: number | undefined;
  /**
   * The reasoning setting, for a model that reasons. Only the strings
   * `assembly.rs`'s `parse_reasoning_effort` accepts are modelled — anything
   * else is `invalid_profile` at the worker, so an unrecognised stored value
   * is dropped on read rather than carried into the next save.
   *
   * `'auto'` and `'off'` are the canonical values of ADR-0038 decision 1
   * (`'none'` is the older spelling of `'off'`). The server gives a new agent
   * `'auto'`, so dropping it here would erase it on the next save. The slider
   * still offers only the three levels (#1452).
   *
   * `'off'` and `'none'` are the values that may coexist with `temperature`:
   * the worker refuses the pair only when the setting thinks.
   */
  readonly reasoning_effort?: AgentReasoningEffort | undefined;
}

/** Exactly the strings `parse_reasoning_effort` accepts (`assembly.rs`); anything else is `invalid_profile`. */
const AGENT_REASONING_EFFORTS = ['auto', 'off', 'none', 'low', 'medium', 'high'] as const;

export type AgentReasoningEffort = (typeof AGENT_REASONING_EFFORTS)[number];

/** `undefined` for a value the worker would refuse — including `null`, which a stored blob uses to mean "unset". */
export function toAgentReasoningEffort(value: unknown): AgentReasoningEffort | undefined {
  return AGENT_REASONING_EFFORTS.find((effort) => effort === value);
}

/**
 * The `temperature`/`reasoning_effort` pair, with the worker's exclusion rule
 * already applied — the single place that rule is encoded, so no assembled
 * settings object and no request body can carry both.
 *
 * A real effort wins over a temperature, because that is the direction the
 * worker's own check runs: `validate_model` refuses the pair outright, and of
 * the two it is the effort that a reasoning model cannot run without. The
 * dialog never offers both at once — `LLMSettings.tsx` renders the reasoning
 * slider OR the creativity slider on `supports_reasoning` — so the only way
 * to reach this with both is a stored blob that was already refused, or a
 * model switch, and `writeAgentLlmSettings` resolves the switch by the newly
 * chosen model's own capability before this is ever reached.
 */
export function selectEffortAndTemperature(
  temperature: number | undefined,
  effort: AgentReasoningEffort | undefined,
): { temperature?: number; reasoning_effort?: AgentReasoningEffort } {
  if (effort !== undefined && effort !== 'none' && effort !== 'off') return { reasoning_effort: effort };
  return {
    ...(effort === undefined ? {} : { reasoning_effort: effort }),
    ...(temperature === undefined ? {} : { temperature }),
  };
}

/** Every key `AgentLlmSettings` models, in the order the wire body writes them. */
const ALLOWED_KEYS = ['model_name', 'model_project_id', 'max_tokens', 'temperature', 'reasoning_effort'] as const;

function readNumber(value: unknown): number | undefined {
  if (typeof value === 'number') return Number.isFinite(value) ? value : undefined;
  // The catalogue and the export/import path both hand back numeric strings
  // for `model_project_id` (`internal/api/v2/eliteacore/handler.go` normalises
  // float64 to a string on export), so a stored version can legitimately hold
  // one and still be the settings the user picked.
  if (typeof value === 'string' && value.trim() !== '') {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : undefined;
  }
  return undefined;
}

/**
 * A stored `llm_settings` blob (or any opaque record) -> the typed settings,
 * or `undefined` when it carries no usable model.
 *
 * Returning `undefined` rather than a half-filled object is the load-bearing
 * part: a version saved before this feature existed holds `{}`, and the
 * platform's own fallback to the project catalogue default is what makes
 * that version answer turns today. Anything this function returns is later
 * written back verbatim, so inventing a model here would replace a working
 * fallback with a guess.
 *
 * Keys outside `ALLOWED_KEYS` are dropped, not carried: a version imported
 * from elsewhere can hold `top_p` or `openai_compatible`, and re-sending
 * either is what turns a green save into a refused turn. `reasoning_effort`
 * is IN that set — it used to be dropped here, which meant every save of a
 * version authored with one wrote it back without one.
 */
export function toAgentLlmSettings(raw: unknown): AgentLlmSettings | undefined {
  if (raw === null || typeof raw !== 'object') return undefined;
  const record = raw as Record<string, unknown>;
  const modelName = record['model_name'];
  const projectId = readNumber(record['model_project_id']);
  if (typeof modelName !== 'string' || modelName === '' || projectId === undefined) return undefined;
  const maxTokens = readNumber(record['max_tokens']);
  return {
    model_name: modelName,
    model_project_id: projectId,
    max_tokens: maxTokens ?? -1,
    ...selectEffortAndTemperature(readNumber(record['temperature']), toAgentReasoningEffort(record['reasoning_effort'])),
  };
}

/**
 * The generated body type, widened by the one worker key `v2.yaml` does not
 * model yet. See {@link AgentLlmSettings}' own note: nothing zod-parses a
 * write body at runtime, so the key reaches the wire; this alias is what
 * keeps that a typed, greppable decision instead of a cast.
 */
export type LlmSettingsBody = LlmSettings & { readonly reasoning_effort?: AgentReasoningEffort };

/**
 * The typed settings -> the generated request body's `llm_settings` value.
 *
 * Spread per key rather than `{ ...settings }`: under
 * `exactOptionalPropertyTypes` an optional key whose value type includes
 * `undefined` is not assignable to the generated schema's own optional key
 * (TS2375), the same reason `pages/pipelines/lib/editPipelineMappers.ts`'s
 * `buildChatLlmSettings` exists.
 */
export function toLlmSettingsBody(settings: AgentLlmSettings): LlmSettingsBody {
  return {
    model_name: settings.model_name,
    model_project_id: settings.model_project_id,
    max_tokens: settings.max_tokens,
    // Re-resolved rather than copied, so the exclusion rule holds even for a
    // settings object assembled somewhere that did not apply it.
    ...selectEffortAndTemperature(settings.temperature, settings.reasoning_effort),
  };
}

/**
 * Key-by-key equality, for the unsaved-changes blocker.
 *
 * Object identity is not usable here: the picker hands back a fresh object
 * on every change, so an identity check reports "changed" forever, while a
 * check that skips these fields entirely reports "unchanged" after the user
 * has picked a different model — #133's failure, where a nav-blocker that
 * could not see a field let navigating away discard it in silence.
 */
export function areAgentLlmSettingsEqual(
  a: AgentLlmSettings | undefined,
  b: AgentLlmSettings | undefined,
): boolean {
  if (a === undefined || b === undefined) return a === b;
  return ALLOWED_KEYS.every((key) => a[key] === b[key]);
}
