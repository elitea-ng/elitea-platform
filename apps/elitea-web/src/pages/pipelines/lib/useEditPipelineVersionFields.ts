import { useCallback, useMemo, useRef, useState } from 'react';

import type { Tag } from '@/entities/tag';
import {
  areAgentLlmSettingsEqual,
  toAgentLlmSettings,
  type AgentLlmSettings,
} from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

/**
 * The version-level fields the pipeline editor's configuration form renders
 * that `applicationCreationSchema` does not validate — held outside the RHF
 * form for the same reason `pages/agents/lib/useEditApplicationVersionFields
 * .ts` holds the agent editor's: widening the form's generic would need an
 * unsound resolver cast for fields nothing ever validates.
 *
 * This hook REPLACES `./useEditPipelineLlmSettings.ts`, which held exactly
 * one of these six. That file existed because the pipeline configuration
 * form was a disclosed stand-in and the model picker was the only control on
 * it; now that the form is real, one state slice for the whole set is what
 * keeps the dirty flag and the save body honest.
 *
 * **`instructions` is deliberately NOT here, and that is the one real
 * difference from the agents twin.** A pipeline's `instructions` column IS
 * its flow graph's YAML document (baseline `useSaveVersion.js:96`:
 * `instructions: !isFromPipeline ? version_details.instructions : yamlCode`),
 * authored on the canvas and read back at save time through
 * `features/pipelines`' `usePipelineGraphDraft`. A second, textual editor for
 * the same column would let the two disagree, and the last writer would
 * silently win.
 */
export interface EditPipelineVersionFields {
  readonly welcomeMessage: string;
  readonly variables: readonly { readonly name: string; readonly value: string }[];
  readonly stepLimit: number | undefined;
  /** `meta.internal_tools` — the MODULES switches. Saved inside the `meta` blob `UpdateVersion` assigns wholesale, which is why the save body merges rather than replaces it. */
  readonly internalTools: readonly string[];
  /** `version_details.llm_settings` — `undefined` when the version names no model, which is what leaves the project's catalogue default in charge. */
  readonly llmSettings: AgentLlmSettings | undefined;
  /** The version's topical tags. `UpdateVersion` writes them as `application_version_tag_association` rows (#345). */
  readonly tags: readonly Tag[];
}

export interface EditPipelineVersionFieldsState {
  readonly fields: EditPipelineVersionFields;
  /** Applies one `CreateAgentForm` field change; returns `true` when `path` is one this hook owns, so the bridge can fall through to RHF for the paths it does not. */
  readonly applyFieldChange: (path: string, value: unknown) => boolean;
  /** Replaces the whole tag list — the shape `AgentTagEditor`'s `onChange` hands back. */
  readonly setTags: (tags: readonly Tag[]) => void;
  /** Feeds the page's `useUnsavedChangesNavBlocker` — RHF's own `isDirty` cannot see these fields. */
  readonly isDirty: boolean;
  /** Called after a successful save so the edits just persisted stop counting as unsaved. */
  readonly markSaved: () => void;
  /** The discard direction — reverts every field to the last saved/loaded value, so a later Save cannot carry a discarded edit. */
  readonly reset: () => void;
}

function toStringArray(value: unknown): readonly string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((entry): entry is string => typeof entry === 'string');
}

/**
 * The generated `VersionTag[]` -> `entities/tag`'s `Tag`. A nameless entry is
 * dropped: it cannot be stored (`tags.name` is NOT NULL) and has no label to
 * show. Same narrowing the agents twin applies.
 */
function toTags(version: ApplicationVersionDetail | undefined): readonly Tag[] {
  return (version?.tags ?? [])
    .filter((tag): tag is { id?: number; name: string; data?: unknown } => typeof tag.name === 'string' && tag.name !== '')
    .map((tag) => ({ id: tag.id ?? 0, name: tag.name, data: tag.data ?? null }));
}

function fromVersion(version: ApplicationVersionDetail | undefined): EditPipelineVersionFields {
  const metaRecord: Record<string, unknown> = version?.meta ?? {};
  return {
    welcomeMessage: version?.welcome_message ?? '',
    variables: (version?.variables ?? []).map((variable) => ({
      name: variable.name ?? '',
      value: variable.value ?? '',
    })),
    stepLimit: typeof metaRecord['step_limit'] === 'number' ? metaRecord['step_limit'] : undefined,
    internalTools: toStringArray(metaRecord['internal_tools']),
    llmSettings: toAgentLlmSettings(version?.llm_settings),
    tags: toTags(version),
  };
}

function areStringListsEqual(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((entry, index) => b[index] === entry);
}

function areVariablesEqual(
  a: EditPipelineVersionFields['variables'],
  b: EditPipelineVersionFields['variables'],
): boolean {
  return (
    a.length === b.length &&
    a.every((variable, index) => {
      const other = b[index];
      return other !== undefined && variable.name === other.name && variable.value === other.value;
    })
  );
}

/** Split out of `areEqual` purely to keep it under this codebase's oxlint cyclomatic-complexity budget (12). */
function areListsEqual(a: EditPipelineVersionFields, b: EditPipelineVersionFields): boolean {
  if (!areStringListsEqual(a.internalTools, b.internalTools)) return false;
  if (a.tags.length !== b.tags.length) return false;
  // Compared by NAME, not by id: a tag the user just typed carries a
  // placeholder id (`AgentTagEditor`), so an id comparison would report the
  // page dirty forever after a save that stored that very tag.
  if (a.tags.some((tag, index) => b.tags[index]?.name !== tag.name)) return false;
  return areVariablesEqual(a.variables, b.variables);
}

function areEqual(a: EditPipelineVersionFields, b: EditPipelineVersionFields): boolean {
  if (a.welcomeMessage !== b.welcomeMessage) return false;
  if (a.stepLimit !== b.stepLimit) return false;
  // Key by key, never by identity: the settings dialog hands back a fresh
  // object each time, so identity would report "dirty" from the first render.
  if (!areAgentLlmSettingsEqual(a.llmSettings, b.llmSettings)) return false;
  return areListsEqual(a, b);
}

/** The chat panel fans a settings object out over `setFieldValue('version_details.llm_settings.<key>', value)`; same pattern, same regex, as the agents twin. */
const LLM_SETTINGS_KEY_PATTERN = /^version_details\.llm_settings\.(.+)$/;

/**
 * Merges one fanned-out key back onto the held settings, then re-reads the
 * result through `toAgentLlmSettings` so a partial write can never leave a
 * half-built profile behind. `temperature` and `reasoning_effort` are
 * mutually exclusive on the wire (the worker refuses a profile carrying
 * both), so a per-key write of one must CLEAR the other.
 */
function mergeLlmSettingsKey(
  previous: AgentLlmSettings | undefined,
  key: string,
  value: unknown,
): AgentLlmSettings | undefined {
  const merged: Record<string, unknown> = { ...previous, [key]: value };
  if (key === 'temperature') delete merged['reasoning_effort'];
  if (key === 'reasoning_effort') delete merged['temperature'];
  return toAgentLlmSettings(merged);
}

function toVariables(value: unknown, previous: EditPipelineVersionFields['variables']) {
  return Array.isArray(value) ? (value as { name: string; value: string }[]) : previous;
}

/**
 * @param activeVersion The version whose fields are being edited. Seeded from
 * it on first arrival and RE-seeded only when the version's IDENTITY changes
 * (a version switch), NOT on every new response object: the detail query
 * refetches on window focus and after `useRefetchPipelineAfterSave`, and
 * keying the resync on object identity would clobber whatever the user had
 * typed since.
 */
export function useEditPipelineVersionFields(
  activeVersion: ApplicationVersionDetail | undefined,
): EditPipelineVersionFieldsState {
  const [fields, setFields] = useState<EditPipelineVersionFields>(() => fromVersion(activeVersion));
  const [baseline, setBaseline] = useState<EditPipelineVersionFields>(fields);
  // `undefined` while the detail fetch is in flight, which is the ordinary
  // first render — the seed below fires as soon as the real version resolves.
  const seededFrom = useRef<string | undefined>(activeVersion?.id);

  // A render-phase resync rather than an effect: an effect renders one frame
  // of blank inputs over a version that has already arrived.
  if (seededFrom.current !== activeVersion?.id) {
    seededFrom.current = activeVersion?.id;
    const seeded = fromVersion(activeVersion);
    setFields(seeded);
    setBaseline(seeded);
  }

  /**
   * Applies `next` only when it differs from what is already held, returning
   * the SAME state object otherwise.
   *
   * Every write into this hook arrives from one of two directions: a control
   * the user is operating, or `features/pipelines`' chat hooks re-publishing
   * a settings object they were just handed. The second kind writes the value
   * that is already stored, and without these guards each one still minted a
   * new `fields` object — which the bridge turns into a new `versionDetails`,
   * which the chat reads, which republishes. The measured livelock on this
   * page had a different immediate cause (see
   * `./useEditPipelineConfigurationTabBridge.ts`'s `applyOverrides`), and
   * these guards did NOT fix it; they close the same class of feedback path
   * one step earlier, which is worth having on its own.
   */
  const applyFieldChange = useCallback((path: string, value: unknown): boolean => {
    switch (path) {
      case 'version_details.welcome_message': {
        const next = typeof value === 'string' ? value : '';
        setFields((previous) => (previous.welcomeMessage === next ? previous : { ...previous, welcomeMessage: next }));
        return true;
      }
      case 'version_details.variables':
        setFields((previous) => {
          const next = toVariables(value, previous.variables);
          return areVariablesEqual(previous.variables, next) ? previous : { ...previous, variables: next };
        });
        return true;
      case 'version_details.meta.internal_tools': {
        const next = toStringArray(value);
        setFields((previous) => (areStringListsEqual(previous.internalTools, next) ? previous : { ...previous, internalTools: next }));
        return true;
      }
      case 'version_details.meta.step_limit': {
        const next = typeof value === 'number' ? value : undefined;
        setFields((previous) => (previous.stepLimit === next ? previous : { ...previous, stepLimit: next }));
        return true;
      }
      // A whole-object replace, which is what the settings dialog's Apply
      // emits — the picker owns every key at once, so a per-key merge would
      // let a stale `temperature` survive a switch to a reasoning model.
      case 'version_details.llm_settings': {
        const next = toAgentLlmSettings(value);
        setFields((previous) => (areAgentLlmSettingsEqual(previous.llmSettings, next) ? previous : { ...previous, llmSettings: next }));
        return true;
      }
      default: {
        const key = LLM_SETTINGS_KEY_PATTERN.exec(path)?.[1];
        if (key === undefined) return false;
        setFields((previous) => {
          const next = mergeLlmSettingsKey(previous.llmSettings, key, value);
          return areAgentLlmSettingsEqual(previous.llmSettings, next) ? previous : { ...previous, llmSettings: next };
        });
        return true;
      }
    }
  }, []);

  const setTags = useCallback((tags: readonly Tag[]) => {
    setFields((previous) => ({ ...previous, tags }));
  }, []);

  const markSaved = useCallback(() => setBaseline(fields), [fields]);
  const reset = useCallback(() => setFields(baseline), [baseline]);
  const isDirty = useMemo(() => !areEqual(fields, baseline), [fields, baseline]);

  return { fields, applyFieldChange, setTags, isDirty, markSaved, reset };
}
