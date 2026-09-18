/**
 * The EDIT-mode configuration form for a pipeline opened from a chat
 * conversation (#940 A12, ELITEA-0928).
 *
 * `PipelineEditor` asks for this through two deps that travel together and
 * that nothing in the chat composition root supplied before:
 * `renderConfigurationPanels` (the Configuration tab's body) and
 * `onSaveVersion` (what its Save button calls outside create mode). Passing
 * neither is what made "edit this pipeline participant" open an editor with a
 * title bar, a permanently-disabled Save, and an EMPTY Configuration tab —
 * `PipelineEditorParts.tsx` renders `deps.renderConfigurationPanels?.(…)` and
 * `disabled={!onSaveVersion}`, so both degraded silently.
 *
 * They are one hook because they share one piece of state. The panels own the
 * live field values; the save has to send them. Splitting them would mean two
 * copies of the draft, which is the shape that made the model picker and the
 * chat panel write into different stores on the standalone pipeline page (see
 * `pages/pipelines/lib/useEditPipelineConfigurationTabBridge.ts`'s own note on
 * that defect).
 *
 * WHAT IT COMPOSES, and why it is not a new form. `CreateAgentForm` with
 * `entityType="pipeline"` is the same component the baseline mounts for a
 * pipeline's configuration and the same one the standalone
 * `pages/pipelines/ui/EditPipelineConfigurationPanel.tsx` mounts — Name,
 * Description, Variables, Welcome Message, Chat starters and Advanced, with
 * the tag editor in its `tagsSlot`. `processes/` may import `features/agents`
 * and `features/pipelines` both, which is exactly why this wiring belongs at
 * this layer and not inside either slice (`no-sideways-features`).
 *
 * WHAT IT DELIBERATELY OMITS. The Tools panel. `ApplicationConfigurationLayout`
 * has a `tools` slot and this hook passes `null` into it: attaching a toolkit
 * is a relation write (`entity_tool_mapping`), not a version field, and the
 * version PUT this hook issues has no branch for `tools` at all (the same
 * reason `useEditPipelineConfigurationTabBridge` gives for keeping `tools` an
 * overlay). Rendering the panel here would offer a control whose changes Save
 * cannot persist — the dead-wiring shape this whole package exists to remove,
 * not add. The pipeline's own flow graph stays where it already is, on the
 * editor's Flow tab.
 *
 * `showInstructions={false}` is load-bearing rather than cosmetic: a
 * pipeline's `instructions` IS its YAML graph
 * (`usePipelineEditorCreate.ts`'s own comment), so a free-text field over it
 * lets prose replace a graph no runtime can then compile.
 */
import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { AgentTagEditor, CreateAgentForm, applicationWriteHooks } from '@/features/agents';
import type { Tag } from '@/entities/tag';
import { useGetApplication, useGetApplicationVersionDetail } from '@/shared/api/generated/applications/applications';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';

/** The participant identity the chat editor opens a pipeline with. */
interface ChatConfigParticipant {
  readonly entity_meta?: { readonly id?: string | number } | undefined;
  readonly entity_settings?: { readonly version_id?: string | number } | undefined;
}

export interface UseChatPipelineConfigParams {
  readonly projectId: string | undefined;
  readonly participant: ChatConfigParticipant | null | undefined;
  /** Edit mode only — create mode has its own form and its own POST. */
  readonly enabled: boolean;
}

/**
 * The placeholder id a tag typed in the editor carries.
 *
 * `entities/tag`'s `Tag` requires `id` and `data`, and a tag the user has just
 * typed has neither — the server matches by NAME on write
 * (`VersionWriteRequest.tags`, api/openapi/v2.yaml), which is why
 * `AgentTagEditor`'s own doc comment already describes a negative placeholder
 * for exactly this case. Every tag this hook rebuilds from stored names gets
 * one, since the draft keeps names and not rows.
 */
const TAG_PLACEHOLDER_ID = -1;

interface DraftVariable {
  readonly name: string;
  readonly value: string;
}

/**
 * The draft `CreateAgentForm` reads and the save body writes. Its field set is
 * the INTERSECTION of what that form renders and what the version PUT (plus
 * the application PATCH `useSaveVersion` folds in) can actually store, so
 * every control on screen corresponds to something Save persists.
 */
interface ChatPipelineDraft {
  readonly name: string;
  readonly description: string;
  readonly version_details: {
    readonly id?: number | undefined;
    readonly instructions?: string | undefined;
    readonly welcome_message?: string | undefined;
    readonly conversation_starters?: readonly string[] | undefined;
    readonly tags?: readonly string[] | undefined;
    readonly variables?: readonly DraftVariable[] | undefined;
    readonly meta?: { readonly step_limit?: number | undefined } | undefined;
  };
}

export interface UseChatPipelineConfigResult {
  /** Passed straight to `PipelineEditorDeps.renderConfigurationPanels`. */
  readonly renderConfigurationPanels: () => { readonly tools: ReactNode };
  /** Passed straight to `PipelineEditorDeps.onSaveVersion`. */
  readonly onSaveVersion: ((onSuccess: (saved: unknown) => void) => void) | undefined;
  readonly isSavingVersion: boolean;
  /**
   * `true` once `draft` has actually changed from the last-seeded server
   * snapshot. Passed straight to `PipelineEditorProps.isConfigurationDirty`.
   *
   * `PipelineEditor.tsx`'s own `isDirty` state only ever tracks the Flow tab
   * (node graph) and `isYamlDirty` only the YAML tab
   * (`PipelineEditorParts.tsx`'s `SaveButtonSlotProps.isDirty` doc comment) —
   * neither is ever set by anything this Configuration tab renders. Since
   * `elitea_issues #2223/#2664`'s fix gated Save on `!onSaveVersion ||
   * !isDirty`, an edit made through THIS hook's own fields (welcome message,
   * tags, variables, …) left the button permanently DISABLED — `isDirty`
   * stays `false` forever, the same button that used to be permanently
   * ENABLED before that fix, just failing the opposite way.
   */
  readonly isConfigurationDirty: boolean;
}

function numericId(value: string | number | undefined): number | undefined {
  if (value === undefined) return undefined;
  const parsed = Number(value);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

/** `tags` reaches the browser as objects and the form's editor works in objects; the write body wants names. Kept as names in the draft so there is one representation to reason about. */
function tagNames(version: ApplicationVersionDetail | undefined): readonly string[] {
  const tags: unknown = version?.tags;
  if (!Array.isArray(tags)) return [];
  return tags
    .map((tag) => (typeof tag === 'string' ? tag : (tag as { readonly name?: unknown })?.name))
    .filter((name): name is string => typeof name === 'string' && name.trim() !== '');
}

function draftVariables(version: ApplicationVersionDetail | undefined): readonly DraftVariable[] {
  const variables: unknown = version?.variables;
  if (!Array.isArray(variables)) return [];
  return variables.map((variable) => {
    const row = variable as { readonly name?: unknown; readonly value?: unknown };
    return {
      name: typeof row.name === 'string' ? row.name : '',
      value: typeof row.value === 'string' ? row.value : '',
    };
  });
}

function draftStarters(version: ApplicationVersionDetail | undefined): readonly string[] {
  const starters: unknown = version?.conversation_starters;
  if (!Array.isArray(starters)) return [];
  return starters.filter((entry): entry is string => typeof entry === 'string');
}

function draftStepLimit(version: ApplicationVersionDetail | undefined): number | undefined {
  const meta: Record<string, unknown> = (version?.meta as Record<string, unknown> | undefined) ?? {};
  const limit = meta['step_limit'];
  return typeof limit === 'number' ? limit : undefined;
}

/**
 * Applies one `setFieldValue(path, value)` write.
 *
 * Only the two path depths the shared form emits are handled — a top-level
 * key and one `version_details.*` key — and an unknown path is DROPPED rather
 * than stored under a made-up key. Silently storing it would put a value in
 * the draft that the save body never reads, which is how a control comes to
 * look like it works.
 */
function applyFieldChange(draft: ChatPipelineDraft, path: string, value: unknown): ChatPipelineDraft {
  if (path === 'name') return { ...draft, name: typeof value === 'string' ? value : '' };
  if (path === 'description') return { ...draft, description: typeof value === 'string' ? value : '' };
  if (!path.startsWith('version_details.')) return draft;
  const key = path.slice('version_details.'.length);
  if (key.includes('.')) return draft;
  return { ...draft, version_details: { ...draft.version_details, [key]: value } };
}

const EMPTY_DRAFT: ChatPipelineDraft = { name: '', description: '', version_details: {} };

/** The stored version and application, as the form's draft. Its own function for the oxlint complexity budget (12). */
function seedDraft(
  version: ApplicationVersionDetail,
  versionId: number | undefined,
  application: { readonly name?: string; readonly description?: string } | undefined,
): ChatPipelineDraft {
  return {
    name: application?.name ?? '',
    description: application?.description ?? '',
    version_details: {
      ...(versionId !== undefined ? { id: versionId } : {}),
      instructions: typeof version.instructions === 'string' ? version.instructions : '',
      welcome_message: typeof version.welcome_message === 'string' ? version.welcome_message : '',
      conversation_starters: draftStarters(version),
      tags: tagNames(version),
      variables: draftVariables(version),
      meta: { step_limit: draftStepLimit(version) },
    },
  };
}

/** The draft in version-write shape. Own function for the same budget reason. */
function toVersionBody(draft: ChatPipelineDraft, versionName: string | undefined) {
  const details = draft.version_details;
  return {
    name: versionName ?? 'latest',
    instructions: details.instructions ?? '',
    welcome_message: details.welcome_message ?? '',
    conversation_starters: [...(details.conversation_starters ?? [])],
    variables: (details.variables ?? []).map((variable) => ({ name: variable.name, value: variable.value })),
    tags: (details.tags ?? []).map((name) => ({ name })),
    meta: { step_limit: details.meta?.step_limit ?? 25 },
  };
}

interface ChatPipelineStoredParams {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly enabled: boolean;
}

/**
 * The two stored reads this form seeds from, and whether it may read at all.
 *
 * Its own hook purely for the oxlint complexity budget (12): the identity
 * guard plus the two generated hooks' placeholder arguments are six branches
 * on their own, and the caller has its own save and render logic to spend the
 * budget on.
 */
function useChatPipelineStored({ projectId, applicationId, versionId, enabled }: ChatPipelineStoredParams) {
  const active = enabled && projectId !== undefined && applicationId !== undefined && versionId !== undefined;
  const versionQuery = useGetApplicationVersionDetail(projectId ?? '', applicationId ?? 0, versionId ?? 0, {
    query: { enabled: active },
  });
  const applicationQuery = useGetApplication(projectId ?? '', applicationId ?? 0, {
    query: { enabled: active },
  });
  const refetch = useCallback(() => {
    void versionQuery.refetch();
    void applicationQuery.refetch();
  }, [versionQuery, applicationQuery]);
  return {
    active,
    // Same unwrap contract every other reader of the generated client follows
    // (`eliteaFetch` returns the ENVELOPE — see
    // `shared/api/runtimeCapabilities.ts`): reading `query.data` as the body
    // is how a 200 renders empty fields.
    version: versionQuery.data?.data as ApplicationVersionDetail | undefined,
    application: applicationQuery.data?.data as { readonly name?: string; readonly description?: string } | undefined,
    refetch,
  };
}

export function useChatPipelineConfig({
  projectId,
  participant,
  enabled,
}: UseChatPipelineConfigParams): UseChatPipelineConfigResult {
  const applicationId = numericId(participant?.entity_meta?.id);
  const versionId = numericId(participant?.entity_settings?.version_id);
  const { active, version, application, refetch } = useChatPipelineStored({
    projectId,
    applicationId,
    versionId,
    enabled,
  });

  const [draft, setDraft] = useState<ChatPipelineDraft>(EMPTY_DRAFT);
  // Set by every field/tag edit below, cleared by the re-seed effect right
  // under it — the same "own state, reset on identity/version change" shape
  // `PipelineEditor.tsx`'s `isDirty`/`onIdentityReset` already uses for the
  // Flow tab. A successful save runs through the SAME path: `onSaveVersion`'s
  // `refetch()` changes `version`, which re-fires the seed effect below.
  const [isConfigurationDirty, setIsConfigurationDirty] = useState(false);

  /*
   * Re-seeded whenever the SERVER's version changes — which includes the
   * refetch after a save, so re-opening the editor shows what was stored
   * rather than what was typed (ELITEA-0928 step 7). Keyed on the two ids as
   * well, so switching participants inside one open editor cannot leave the
   * previous pipeline's text in the fields.
   */
  useEffect(() => {
    if (!active || version === undefined) return;
    setDraft(seedDraft(version, versionId, application));
    setIsConfigurationDirty(false);
  }, [active, version, application, versionId]);

  const onFieldChange = useCallback((path: string, value: unknown) => {
    setDraft((previous) => applyFieldChange(previous, path, value));
    setIsConfigurationDirty(true);
  }, []);

  const onTagsChange = useCallback((next: readonly Tag[]) => {
    const names = next.map((tag) => tag.name).filter((name) => name.trim() !== '');
    setDraft((previous) => ({ ...previous, version_details: { ...previous.version_details, tags: names } }));
    setIsConfigurationDirty(true);
  }, []);

  const tagValue = useMemo<readonly Tag[]>(
    () => (draft.version_details.tags ?? []).map((name) => ({ id: TAG_PLACEHOLDER_ID, name, data: undefined })),
    [draft.version_details.tags],
  );

  const { onSave, isSaving } = applicationWriteHooks.useSaveVersion();

  const renderConfigurationPanels = useCallback(
    () => ({
      // The layout's one REQUIRED slot. `null` on purpose — see this module's
      // own doc comment for why the Tools panel is not offered here.
      tools: null,
      information: (
        <CreateAgentForm
          entityType="pipeline"
          showInstructions={false}
          values={draft}
          onFieldChange={onFieldChange}
          tagsSlot={
            <AgentTagEditor
              projectId={projectId}
              value={tagValue}
              onChange={onTagsChange}
            />
          }
        />
      ),
    }),
    [draft, onFieldChange, projectId, tagValue, onTagsChange],
  );

  const onSaveVersion = useCallback(
    (onSuccess: (saved: unknown) => void) => {
      if (projectId === undefined || applicationId === undefined || versionId === undefined) return;
      void onSave({
        projectId,
        applicationId,
        versionId,
        version: toVersionBody(draft, version?.name),
        applicationName: draft.name,
        applicationDescription: draft.description,
      }).then((result) => {
        // Only a real save reports success. `useSaveVersion` resolves with
        // `undefined` on failure (it keeps the error in its own state), and
        // reporting that as saved would clear the editor's dirty flag over an
        // edit the server never took.
        if (result !== undefined) {
          refetch();
          onSuccess(result);
        }
      });
    },
    [projectId, applicationId, versionId, onSave, draft, version?.name, refetch],
  );

  return {
    renderConfigurationPanels,
    onSaveVersion: active ? onSaveVersion : undefined,
    isSavingVersion: isSaving,
    isConfigurationDirty,
  };
}
