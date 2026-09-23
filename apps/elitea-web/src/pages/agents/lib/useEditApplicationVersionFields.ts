import { useCallback, useMemo, useRef, useState } from 'react';

import type { Tag } from '@/entities/tag';
import {
  areAgentLlmSettingsEqual,
  toAgentLlmSettings,
  type AgentLlmSettings,
} from '@/shared/api/agentLlmSettings';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

/**
 * The version-level fields `CreateAgentForm` renders on the agent EDIT page
 * that `applicationCreationSchema` does not validate — held outside the RHF
 * form, exactly as `pages/agents/CreateApplication.tsx` already holds them
 * for the CREATE page (see its `CreateAgentFormExtraFields` doc comment for
 * why widening the form's generic would need an unsound resolver cast for
 * fields nothing ever validates).
 *
 * #307: before this hook existed these four fields were rendered from the
 * server response and routed nowhere — `useEditApplicationEditorBridge`
 * early-returned for every path except `name`/`description`, so typing in
 * instructions or the welcome message updated nothing at all and the Save
 * button reported success having sent only `conversation_starters`.
 */
export interface EditApplicationVersionFields {
  readonly instructions: string;
  readonly welcomeMessage: string;
  readonly variables: readonly { readonly name: string; readonly value: string }[];
  readonly stepLimit: number | undefined;
  /**
   * `meta.internal_tools` — the Tools panel's internal-tool switches (#307's
   * mount). Held here with the other version-level fields because it is
   * saved the same way they are: inside the `meta` blob the Go
   * `UpdateVersion` handler assigns wholesale, which
   * `toVersionSaveBody` merges rather than replaces. Unlike the attached
   * TOOLKITS (immediate `entity_tool_mapping` writes, no Save involved),
   * these switches are ordinary unsaved form state until Save.
   */
  readonly internalTools: readonly string[];
  /**
   * `version_details.llm_settings` — the model this version runs on. Held
   * here with the other version-level fields because the model picker is not
   * an `applicationCreationSchema` field either, and because `areEqual`
   * below has to see it: a picked model that the nav blocker cannot observe
   * is a model the user loses by navigating away (#133).
   *
   * `undefined` means the version names no model. It is preserved rather
   * than defaulted so a save omits the key and leaves the platform's
   * catalogue-default fallback in charge.
   */
  readonly llmSettings: AgentLlmSettings | undefined;
  /**
   * #345 — the version's topical tags. Held here with the other
   * version-level fields for the same reason they are: they are ordinary
   * unsaved form state until Save, and `applicationCreationSchema` does not
   * validate them. Unlike the fields above they do NOT arrive through
   * `CreateAgentForm`'s path-based `onFieldChange`: the control is a slot
   * the page owns, and unlike the model picker's slot — which routes its
   * object back through that same path API — it gets its own setter
   * (`setTags`) instead of a case in `applyFieldChange`.
   */
  readonly tags: readonly Tag[];
  /**
   * `meta.notes` — the editor's free-text Editor Notes (#898). Held here with
   * the other version-level fields, and stored where pylon stores it: inside
   * the `meta` blob, not in a column of its own (elitea_issues #5410 chose
   * that to avoid a per-tenant migration, and
   * `internal/api/v2/applications/handler.go`'s `notesMetaKey` says the same
   * on the Go side). It is documentation only — never sent to the model.
   */
  readonly notes: string;
}

export interface EditApplicationVersionFieldsState {
  readonly fields: EditApplicationVersionFields;
  /**
   * Applies one `CreateAgentForm` field change. Returns `true` when `path`
   * is one this hook owns, so the bridge can fall through to the RHF form
   * for the paths it does not (`name`/`description`) without duplicating
   * the path list in two places.
   */
  readonly applyFieldChange: (path: string, value: unknown) => boolean;
  /** Replaces the whole tag list — the shape `AgentTagEditor`'s `onChange` hands back (#345). */
  readonly setTags: (tags: readonly Tag[]) => void;
  /** Feeds the page's `useUnsavedChangesNavBlocker` — RHF's own `isDirty` cannot see these fields (#133's create-page half made the same point). */
  readonly isDirty: boolean;
  /** Called after a successful save so the edits just persisted stop counting as unsaved (mirrors `form.reset(form.getValues())` on the RHF half). */
  readonly markSaved: () => void;
}

function toStringArray(value: unknown): readonly string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((entry): entry is string => typeof entry === 'string');
}

/**
 * The generated `VersionTag[]` (`{id?, name?, data?}` — the union of the
 * applications read shape and the looser export/fork echoes) -> the
 * `entities/tag` `Tag` the control binds to. A nameless entry is dropped:
 * it cannot be stored (`tags.name` is NOT NULL) and has no label to show.
 * `id` falls back to 0 only for the echo shapes that omit it; the
 * applications version-detail read always sends the real `tags.id`.
 */
function toTags(version: ApplicationVersionDetail | undefined): readonly Tag[] {
  return (version?.tags ?? [])
    .filter((tag): tag is { id?: number; name: string; data?: unknown } => typeof tag.name === 'string' && tag.name !== '')
    .map((tag) => ({ id: tag.id ?? 0, name: tag.name, data: tag.data ?? null }));
}

function fromVersion(version: ApplicationVersionDetail | undefined): EditApplicationVersionFields {
  const metaRecord: Record<string, unknown> = version?.meta ?? {};
  return {
    instructions: version?.instructions ?? '',
    welcomeMessage: version?.welcome_message ?? '',
    variables: (version?.variables ?? []).map((variable) => ({
      name: variable.name ?? '',
      value: variable.value ?? '',
    })),
    stepLimit: typeof metaRecord['step_limit'] === 'number' ? metaRecord['step_limit'] : undefined,
    internalTools: toStringArray(metaRecord['internal_tools']),
    llmSettings: toAgentLlmSettings(version?.llm_settings),
    tags: toTags(version),
    notes: typeof metaRecord['notes'] === 'string' ? metaRecord['notes'] : '',
  };
}

function areEqual(a: EditApplicationVersionFields, b: EditApplicationVersionFields): boolean {
  if (a.instructions !== b.instructions) return false;
  if (a.welcomeMessage !== b.welcomeMessage) return false;
  if (a.stepLimit !== b.stepLimit) return false;
  if (a.notes !== b.notes) return false;
  // Key by key, never by identity: the settings dialog hands back a fresh
  // object each time, so identity would report "dirty" from the first render.
  if (!areAgentLlmSettingsEqual(a.llmSettings, b.llmSettings)) return false;
  if (a.internalTools.length !== b.internalTools.length) return false;
  if (a.internalTools.some((name, index) => b.internalTools[index] !== name)) return false;
  if (a.tags.length !== b.tags.length) return false;
  // Compared by NAME, not by id: a tag the user just typed carries a
  // placeholder id (`AgentTagEditor`), so an id comparison would report the
  // page dirty forever after a save that stored that very tag.
  if (a.tags.some((tag, index) => b.tags[index]?.name !== tag.name)) return false;
  if (a.variables.length !== b.variables.length) return false;
  return a.variables.every((variable, index) => {
    const other = b.variables[index];
    return other !== undefined && variable.name === other.name && variable.value === other.value;
  });
}

/**
 * The chat panel writes the settings one key at a time rather than as a whole
 * object — `features/agents/lib/hooks/useApplicationChat.hooks.ts`'s
 * `onSetLLMSettings` fans a settings object out over `setFieldValue(
 * 'version_details.llm_settings.<key>', value)`. Same pattern, same regex, as
 * `pages/pipelines/lib/useEditPipelineConfigurationTabBridge.ts`.
 */
const LLM_SETTINGS_KEY_PATTERN = /^version_details\.llm_settings\.(.+)$/;

/**
 * Merges one fanned-out key back onto the held settings, then re-reads the
 * result through `toAgentLlmSettings` so a partial write can never leave a
 * half-built profile behind: an update that has not yet supplied a model name
 * or project id yields `undefined` (the version still names no model) rather
 * than an object the worker would refuse.
 */
function mergeLlmSettingsKey(
  previous: AgentLlmSettings | undefined,
  key: string,
  value: unknown,
): AgentLlmSettings | undefined {
  // temperature and reasoning_effort are mutually exclusive on the wire (the
  // worker refuses a profile carrying both, and `toAgentLlmSettings` keeps
  // the effort when they collide). A per-key write of one must therefore
  // CLEAR the other, or setting a temperature on a version that stored an
  // effort silently loses the write to the XOR instead of replacing it.
  const merged: Record<string, unknown> = { ...previous, [key]: value };
  if (key === 'temperature') delete merged['reasoning_effort'];
  if (key === 'reasoning_effort') delete merged['temperature'];
  return toAgentLlmSettings(merged);
}

function toVariables(value: unknown, previous: EditApplicationVersionFields['variables']) {
  return Array.isArray(value) ? (value as { name: string; value: string }[]) : previous;
}

/**
 * @param activeVersion The version whose fields are being edited. Seeded
 * from it on first arrival and RE-seeded only when the version's IDENTITY
 * changes (a version switch), NOT on every new response object: the detail
 * query refetches (window focus, a sibling mutation) return a fresh object
 * for the same version, and keying the resync on object identity would
 * clobber whatever the user had typed since. Same reasoning
 * `features/agents/ui/WelcomeMessageInput.tsx` already applies to its own
 * `versionId` resync dependency.
 */
export function useEditApplicationVersionFields(
  activeVersion: ApplicationVersionDetail | undefined,
): EditApplicationVersionFieldsState {
  const [fields, setFields] = useState<EditApplicationVersionFields>(() => fromVersion(activeVersion));
  const [baseline, setBaseline] = useState<EditApplicationVersionFields>(fields);
  // The identity this state was seeded from. `undefined` while the detail
  // fetch is still in flight, which is the normal first render — the seed
  // below fires as soon as the real version resolves.
  const seededFrom = useRef<string | undefined>(activeVersion?.id);

  // Render-phase resync rather than an effect: an effect would render one
  // frame of blank inputs over a version that has already arrived, which is
  // the exact staleness `useEditApplicationEditorBridge`'s own `useWatch`
  // doc comment records hitting on this page.
  if (seededFrom.current !== activeVersion?.id) {
    seededFrom.current = activeVersion?.id;
    const seeded = fromVersion(activeVersion);
    setFields(seeded);
    setBaseline(seeded);
  }

  const applyFieldChange = useCallback((path: string, value: unknown): boolean => {
    switch (path) {
      case 'version_details.instructions':
        setFields((previous) => ({ ...previous, instructions: typeof value === 'string' ? value : '' }));
        return true;
      case 'version_details.welcome_message':
        setFields((previous) => ({ ...previous, welcomeMessage: typeof value === 'string' ? value : '' }));
        return true;
      case 'version_details.variables':
        setFields((previous) => ({ ...previous, variables: toVariables(value, previous.variables) }));
        return true;
      case 'version_details.meta.internal_tools':
        setFields((previous) => ({ ...previous, internalTools: toStringArray(value) }));
        return true;
      case 'version_details.notes':
        setFields((previous) => ({ ...previous, notes: typeof value === 'string' ? value : '' }));
        return true;
      case 'version_details.meta.step_limit':
        setFields((previous) => ({ ...previous, stepLimit: typeof value === 'number' ? value : undefined }));
        return true;
      // A whole-object replace, which is what the settings dialog's Apply
      // emits — the picker owns every key at once, so a per-key merge would
      // let a stale `temperature` survive a switch to a reasoning model,
      // which the worker refuses as an `invalid_profile`.
      case 'version_details.llm_settings':
        setFields((previous) => ({ ...previous, llmSettings: toAgentLlmSettings(value) }));
        return true;
      default: {
        const key = LLM_SETTINGS_KEY_PATTERN.exec(path)?.[1];
        if (key === undefined) return false;
        setFields((previous) => ({
          ...previous,
          llmSettings: mergeLlmSettingsKey(previous.llmSettings, key, value),
        }));
        return true;
      }
    }
  }, []);

  const setTags = useCallback((tags: readonly Tag[]) => {
    setFields((previous) => ({ ...previous, tags }));
  }, []);

  const markSaved = useCallback(() => setBaseline(fields), [fields]);

  const isDirty = useMemo(() => !areEqual(fields, baseline), [fields, baseline]);

  return { fields, applyFieldChange, setTags, isDirty, markSaved };
}
