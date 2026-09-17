import type { ReactNode, Ref, SyntheticEvent } from 'react';
import { forwardRef, useCallback, useImperativeHandle, useMemo, useRef, useState } from 'react';

import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { PipelineEditorBody, PipelineEditorSaveButton, PipelineEditorTabs, type EditorPanelHandle, type PipelineEditorDeps } from './PipelineEditorParts';
import {
  usePipelineConversationStartersSync,
  usePipelineEditorActions,
  usePipelineEditorHandle,
  usePipelineIdentityReset,
  usePipelineVersionQuery,
  usePipelineVersionSync,
} from './usePipelineEditorLifecycle';
import { useSelectedProjectId } from '../lib/flow-editor/hooks/useSelectedProjectId';
import {
  PUBLIC_PROJECT_ID,
  canEditPipeline,
  getPipelineId,
  isPublicPipeline,
  pipelineDisplayName,
  pipelineViewMode,
  type PipelineEditorPipelineLike,
} from '../lib/pipelineEditorViewState';
import { useHasPermission } from '../lib/useHasPermission';
import { usePipelineEditorCreate } from '../lib/usePipelineEditorCreate';

export type { PipelineEditorDeps, PipelineEditorShellProps, PipelineCreateFormSlotProps } from './PipelineEditorParts';

/**
 * `PipelineEditor` — ported from `apps/elitea-ui/src/pages/NewChat/
 * PipelineEditor.jsx`.
 *
 * **MUST be exported through `src/features/pipelines/index.ts`** — Wave-2
 * unit C6 needs it cross-feature (this batch's own mission brief; broader
 * than the brief's `partitionRisks` note, which named only this file — all
 * three of `PipelineEditor`/`useEditPipeline`/`usePipelineCreation` are the
 * hard requirement, per the mission preamble).
 *
 * **STILL UNREACHABLE from the live app — confirmed, and OUT OF THIS
 * CLUSTER'S (A2-editor-composition) file scope to fix; exact routing for
 * whoever picks this up:**
 *  - Nothing calls `<PipelineEditor>`/`useEditPipeline()`/
 *    `usePipelineCreation()` anywhere outside this slice (verified: `grep
 *    -rn "<PipelineEditor\b" src | grep -v test` — only this file's own
 *    definition; `grep -rln "useEditPipeline\b\|usePipelineCreation\b" src`
 *    — only this slice's own files/tests).
 *  - The intended composition root, `processes/chat/model/useEditorMutex.ts`
 *    (`onShowPipelineEditor`/`onEditPipeline`/`onShowPipelineEditorCreator`
 *    params), is itself never CALLED anywhere (verified: `grep -rn
 *    "useEditorMutex(" src` — zero call sites outside its own file/tests).
 *    This is not pipeline-specific: the identical gap blocks
 *    `features/agents`' `AgentEditor` (also unreachable — `grep -rn
 *    "<AgentEditor\b" src | grep -v test` — same result) and, presumably,
 *    the toolkit/canvas/artifact editors `useEditorMutex` also orchestrates.
 *  - `widgets/chat-box/ui/ChatBox.helpers.ts`'s `buildAgentEditorProps`
 *    (consumed by `ChatBox.tsx`, fed into `features/chat-input`'s
 *    `NewChatInput`) wires `onShowAgentEditor: () => {}` /
 *    `onShowPipelineEditor: () => {}` — literal no-ops — instead of real
 *    handlers built from `useEditorMutex`/`useEditPipeline`/`useEditAgent`.
 *    `ChatBox.tsx` itself never mounts `<AgentEditor>` or `<PipelineEditor>`
 *    anywhere in its render tree.
 *  - Fixing this for real means: (1) `ChatBox.tsx` (or a new child it owns)
 *    calls `useEditPipeline`/`usePipelineCreation` (this slice, already
 *    exported) and `useEditAgent`/`useAgentCreation` (`features/agents`,
 *    check that slice's own export status) and `useEditorMutex`
 *    (`processes/chat`); (2) it renders `<PipelineEditor deps={...}>` /
 *    `<AgentEditor deps={...}>` somewhere in its tree, gated on
 *    `isEditingPipeline`/`isEditingAgent`; (3) `buildAgentEditorProps`'s
 *    no-op `onShowAgentEditor`/`onShowPipelineEditor` get replaced with the
 *    real `onEditAgent`/`onEditPipeline` from `useEditorMutex`. ALL of that
 *    is `widgets/chat-box`/`processes/chat` file scope — a different
 *    Wave-2 unit (C6) than this cluster, and per this mission's own
 *    tracking, C6 was already verified+clean in an earlier pass (so this is
 *    a REGRESSION-FROM-SCOPE gap, not something C6 missed by accident —
 *    C6's remit at the time didn't include wiring editors that hadn't
 *    landed yet). Not fixed here: touching `widgets/chat-box`/
 *    `processes/chat` is outside this cluster's file scope, and
 *    `PipelineEditor`/`useEditPipeline`/`usePipelineCreation` themselves
 *    have substantial real test coverage proving they work correctly in
 *    isolation — the gap is purely "nobody mounts them yet".
 *
 * **Real, load-bearing redesign, not a porting shortcut — read before
 * wiring this up.** This app has no Formik (react-hook-form + zod instead —
 * `features/agents/ui/AgentEditor.tsx`'s own doc comment establishes this
 * first, for the byte-for-byte-identical situation on the agent side).
 * `PipelineEditor` follows the EXACT same `deps`-injection shape
 * `AgentEditor` already established for this batch, adapted for the
 * pipeline-specific Configuration/Flow-editor tab split (types in
 * `./PipelineEditorParts.ts`, split out purely for the §3.5 400-line file
 * budget):
 *
 *  - `deps.renderShell` — the chat-owned editor chrome (baseline:
 *    `pages/NewChat/components/BaseEditor.jsx`). `features/pipelines`
 *    cannot import `features/chat` (`no-sideways-features`, no carve-out).
 *  - `deps.renderLlmModelSelector` — baseline:
 *    `pages/NewChat/components/LLMModelSelectorWrapper.jsx`, same
 *    chat-ownership reason.
 *  - `deps.renderConfigurationPanels` — the SIX baseline `features/agent`-owned
 *    panels the baseline's `PipelineConfigurationForm.jsx` composed
 *    (`ApplicationTools`/`AgentInput.WelcomeMessageInput`/
 *    `ConversationStarters`/`ApplicationAdvanceSettings`/
 *    `ApplicationEditorNotes`/`ApplicationInformation`) — verified via
 *    `entities/application-form/ui/ApplicationConfigurationLayout.tsx`'s
 *    own doc comment: "A1 (agent) and A2 (pipeline) each pass their own
 *    panels into the same slots... this is the 'peers, not one owning the
 *    other' relationship." No `features/pipelines`-owned equivalent of
 *    these six panels exists anywhere in this worktree (grepped directly),
 *    and none is in this sub-unit's (A2l) owned-file list — same
 *    "not-yet-landed sibling slot" treatment `AgentEditor.tsx`'s own
 *    `renderConfigurationForm` doc comment gives its identical gap
 *    (verified there via `find src/features/agents -iname
 *    '*ApplicationConfigurationForm*'` — zero hits; `pages/pipelines/
 *    EditPipeline.tsx`'s own doc comment independently hits and discloses
 *    the SAME gap for the standalone-page editor, "A disclosed placeholder
 *    stands in its place"). `ApplicationConfigurationLayout` itself (the
 *    LAYOUT, not the panel bodies) is legally importable from `entities/`
 *    and IS called directly by `PipelineConfigurationTab`
 *    (`./PipelineEditorParts.tsx`) — only the individual panel ReactNodes
 *    are deferred.
 *  - `deps.renderCreateForm` — the baseline's create-mode `CreateAgentForm
 *    entityType="pipeline"` (`PipelineEditor.jsx:517-523`) is
 *    `features/agents`-owned (`no-sideways-features` forbids reaching it
 *    even though it is exported from that slice's `index.ts` — no
 *    carve-out). `values`/`onFieldChange` are owned locally by
 *    `usePipelineEditorCreate` (mirroring `useAgentEditorCreate.ts`'s
 *    identical role) and handed to this not-yet-landed slot.
 *  - `deps.useConversationStartersSync` — baseline:
 *    `useConversationStartersSync` (`features/chat/lib/hooks`). Defaults to
 *    a no-op, same as `AgentEditor.tsx`.
 *  - `deps.onSaveVersion`/`isSavingVersion` — edit-mode save orchestration
 *    (baseline: `SaveApplicationButton`'s internal `useSaveVersion` call)
 *    is NOT owned here, same split `AgentEditor.tsx` already made: this
 *    component owns the READ side (fetch + YAML/flow-graph reconciliation
 *    into the zustand stores, `./usePipelineEditorLifecycle.ts`) but not
 *    the WRITE side, which needs the not-yet-landed configuration-form
 *    slot's own live field values to build a save payload.
 *
 * **Zustand, not Redux — `slices/pipeline.js`/`slices/pipelineEditor.js`
 * are ALREADY faithfully ported, by sibling sub-units, not this file:**
 *  - `slices/pipelineEditor.js` (`{nodes, edges}`) + `slices/pipeline.js`'s
 *    `stateValidationErrors` → `../model/pipelineEditorStore.ts` (unit
 *    A2d, landed).
 *  - `slices/pipeline.js`'s `yamlCode`/`yamlJsonObject`/`initState.yamlCode`
 *    → `../model/pipelineYamlStore.ts` (unit A2n, landed) — its own doc
 *    comment names THIS unit ("whichever sub-unit owns the pipeline-editor
 *    PAGE/composition root") as the intended consumer for the
 *    `initPipelineYaml`/`markYamlCodeSaved`/`resetPipelineYaml` write API it
 *    built but does not itself call. `./usePipelineEditorLifecycle.ts`'s
 *    `usePipelineVersionSync` is that consumer.
 *  - `slices/pipeline.js`'s remaining fields — `orientation` (canvas
 *    layout toggle), `resetFlag` (internal `FlowEditor.jsx` re-sync
 *    signal), `layout_version` (auto-relayout migration version) — have NO
 *    consumer anywhere in this worktree (verified via `grep -rl
 *    "layout_version\|orientation\|resetFlag" src/features/pipelines`: every
 *    hit is either one of the two stores above declining to carry them, or
 *    `useFlowEditorLifecycle.ts`/`FlowEditor`-internal plumbing that takes
 *    them as caller-supplied PROPS rather than reading a store — sibling
 *    unit A2k's own disclosed redesign). This component's freshly-computed
 *    `{nodes, edges}` (via `ParsePipelineHelpers.parseYaml` +
 *    `LayoutHelpers.doLayout`, per this batch's explicit instruction to use
 *    both — see `./usePipelineEditorLifecycle.ts`'s `usePipelineVersionSync`)
 *    are written to `pipelineEditorStore`'s real `setNodes`/`setEdges` —
 *    that store is the one landed zustand store with a `{nodes, edges}`
 *    shape, and calling its own already-built public API from this new call
 *    site is the minimal-invention choice; whichever future sub-unit lands
 *    `FlowEditor.tsx` reads both stores directly (no prop-threading needed
 *    for the STATE, only `EditorPanel`'s own small ref/callback surface —
 *    the whole point of a globally-accessible store over Redux+props).
 *
 * **`EditorPanel` (A2n, `./EditorPanel`) landed in this worktree partway
 * through this sub-unit's own build** (this file initially declared a local
 * `EditorPanelHandle` stand-in per this batch's stated cross-sub-unit
 * landing-order hazard; once the real file appeared, `./PipelineEditorParts.tsx`
 * switched to importing the real `EditorPanel`/`EditorPanelHandle` from it
 * directly). The 4 props passed (`ref`, `setYamlDirty`, `disabled`,
 * `stopRun`) match the baseline's own call site (`PipelineEditor.jsx:
 * 543-548`) as closely as the REAL landed `EditorPanelProps` allows — one
 * real, disclosed deviation: the baseline's `onStopRun` takes an `isChat`
 * boolean (`stopRun={onStopRun}`, where `onStopRun = isChat => {...}`), but
 * the landed `EditorPanelProps.stopRun` is `() => void` (A2n's own
 * redesign — the editor's internal stop control always means "stopped from
 * inside the chat editor"). `PipelineEditorBody` (`./PipelineEditorParts.tsx`)
 * bridges this by wiring `stopRun={() => onStopRun(true)}`, preserving this
 * component's own externally-visible `PipelineEditorHandle.onStopRun(isChat)`
 * contract (still needed for the `stopRunOnNodeStop`/node-triggered-stop
 * branch, `isChat === false`) unchanged.
 *
 * **Dropped, disclosed gaps (same "no unreachable dependency invented"
 * discipline as `AgentEditor.tsx`):**
 *  - `usePipelineAttachmentYamlSync`/`<PipelineAttachmentYamlSync />`
 *    (baseline: `features/pipelines/lib/hooks/
 *    usePipelineAttachmentYamlSync.hooks.js`) read `useFormikContext()` (no
 *    Formik in this app) and the version's live `internal_tools`, which
 *    only the not-yet-landed `renderConfigurationPanels` slot's own form
 *    state holds — same "this component has no reach into it" gap
 *    `AgentEditor.tsx`'s own doc comment documents for `tools`.
 *  - `InstructionsInputRefProvider`/`fileReaderEnhancerRef`
 *    (`app/providers`, baseline lines 19,42,273,413,461) — `features/`
 *    cannot import `app/` (`no-upward-from-features`), and its one real
 *    consumer (`CreateAgentForm`'s instructions field, `showInstructions=
 *    {false}` for pipelines anyway) is `features/agents`-owned and
 *    unreachable regardless.
 *  - GA event tracking (`useTrackEvent`, baseline lines 18,135,429-436) —
 *    no analytics-event SDK exists anywhere in this app, same documented
 *    gap `usePipelineCreation.ts`'s own doc comment already gives.
 */

export interface PipelineEditorHandle {
  readonly onRcvAgentEvent: (event: unknown) => void;
  readonly deleteAllRunNodes: () => void;
  readonly onStopRun: (isChat: boolean) => void;
}

export interface PipelineEditorProps {
  readonly pipeline: PipelineEditorPipelineLike | null | undefined;
  readonly isVisible: boolean;
  readonly isCreateMode?: boolean;
  readonly onClosePipelineEditor?: () => void;
  readonly onPipelineCreated?: (result: ApplicationCreatedResponse) => void;
  readonly onPipelineSaved?: (savedFormData: unknown) => void;
  readonly onPipelineDirtyStateChange?: (isDirty: boolean) => void;
  readonly stopRunOnNodeStop?: (stop: boolean) => void;
  readonly activeParticipantId?: string | number | undefined;
  readonly onAttachmentToolChange?: (pipelineId: string | number | undefined) => void;
  readonly onConversationStartersChange?: (starters: readonly string[]) => void;
  readonly deps: PipelineEditorDeps;
}

/** Split out purely to keep `PipelineEditorInner`'s own branch count under the oxlint complexity budget. */
function resolveEditorTitle(isCreateMode: boolean, pipeline: PipelineEditorPipelineLike | null | undefined): string {
  if (isCreateMode) return t('features.pipelines.pipelineEditor.createTitle', 'Create New Pipeline');
  return pipelineDisplayName(pipeline, t('features.pipelines.pipelineEditor.unnamed', 'Unnamed Pipeline'));
}

/** Split out purely to keep `PipelineEditorInner`'s own branch count under the oxlint complexity budget. */
function resolveValidateProjectId(entityProjectId: string | number | undefined, projectId: string | undefined): string | undefined {
  return typeof entityProjectId === 'string' ? entityProjectId : projectId;
}

/** Split out purely to keep `PipelineEditorInner`'s own branch count under the oxlint complexity budget. */
function hasNothingToRender(pipeline: PipelineEditorPipelineLike | null | undefined, isCreateMode: boolean): boolean {
  return !pipeline && !isCreateMode;
}

/** Split out purely to keep `PipelineEditorInner`'s own branch count under the oxlint complexity budget. */
function resolveEditorSubtitle(isCreateMode: boolean, versionName: string | undefined): string | undefined {
  return isCreateMode ? undefined : versionName;
}

const PipelineEditorInner = (
  {
    pipeline,
    isVisible,
    isCreateMode = false,
    onClosePipelineEditor,
    onPipelineCreated,
    onPipelineSaved,
    onPipelineDirtyStateChange,
    stopRunOnNodeStop,
    activeParticipantId,
    onAttachmentToolChange,
    onConversationStartersChange,
    deps,
  }: PipelineEditorProps,
  ref: Ref<PipelineEditorHandle>,
): ReactNode => {
  const editorPanelRef = useRef<EditorPanelHandle | null>(null);

  const projectId = useSelectedProjectId();
  const hasEditPermission = useHasPermission(projectId, PERMISSIONS.applications.update);

  const [isDirty, setIsDirty] = useState(false);
  const [isYamlDirty, setIsYamlDirty] = useState(false);
  const [activeTab, setActiveTab] = useState(0);

  const isPublic = isPublicPipeline(pipeline);
  const canEditIt = canEditPipeline(isPublic, hasEditPermission);
  const viewMode = pipelineViewMode(canEditIt);

  const pipelineId = getPipelineId(pipeline);
  const versionId = pipeline?.entity_settings?.version_id;
  const entityProjectId = pipeline?.entity_meta?.project_id;
  const isPublishedPipeline = entityProjectId === PUBLIC_PROJECT_ID;

  const handle = usePipelineEditorHandle<EditorPanelHandle>({ editorPanelRef, pipelineId, activeParticipantId, stopRunOnNodeStop });
  useImperativeHandle(ref, () => handle, [handle]);

  const totalDirty = useMemo(() => isDirty || isYamlDirty, [isDirty, isYamlDirty]);

  const onIdentityReset = useCallback(() => {
    setActiveTab(0);
    setIsDirty(false);
    setIsYamlDirty(false);
  }, []);
  usePipelineIdentityReset({ isCreateMode, pipelineEntityId: pipeline?.entity_meta?.id, onReset: onIdentityReset });

  const { versionDetails, fetchError, refetchPrivate } = usePipelineVersionQuery({
    projectId,
    pipelineId,
    versionId,
    isVisible,
    isCreateMode,
    isPublishedPipeline,
  });
  usePipelineVersionSync({ isCreateMode, versionDetails, versionId });

  const create = usePipelineEditorCreate(projectId);

  const { canSaveCreate, handleCreateSubmit, handleDiscard, handleSaveSuccess, handleAttachmentToolChange } = usePipelineEditorActions({
    isCreateMode,
    pipelineEntityId: pipeline?.entity_meta?.id,
    create,
    onPipelineCreated,
    onPipelineSaved,
    onAttachmentToolChange,
    refetchPrivate,
    setIsDirty,
    setIsYamlDirty,
  });

  const handleTabChange = useCallback((_event: SyntheticEvent, newValue: number) => {
    setActiveTab(newValue);
  }, []);

  const handleClose = useCallback(() => {
    onClosePipelineEditor?.();
    deps.onEditorClosed?.();
  }, [onClosePipelineEditor, deps]);

  usePipelineConversationStartersSync(deps.useConversationStartersSync, onConversationStartersChange);

  if (hasNothingToRender(pipeline, isCreateMode)) {
    return null;
  }

  const editorTitle = resolveEditorTitle(isCreateMode, pipeline);
  const editorSubtitle = resolveEditorSubtitle(isCreateMode, versionDetails?.name);
  const validateProjectId = resolveValidateProjectId(entityProjectId, projectId);

  const saveButton = (
    <PipelineEditorSaveButton
      isCreateMode={isCreateMode}
      onCreateSave={() => void handleCreateSubmit()}
      isCreating={create.isCreating}
      canSaveCreate={canSaveCreate}
      isSavingVersion={deps.isSavingVersion}
      deps={deps}
      onSaveSuccess={handleSaveSuccess}
      isDirty={totalDirty}
    />
  );

  const formContent = (
    <PipelineEditorTabs
      isCreateMode={isCreateMode}
      activeTab={activeTab}
      onTabChange={handleTabChange}
    />
  );

  const body = (
    <PipelineEditorBody
      isCreateMode={isCreateMode}
      activeTab={activeTab}
      viewMode={viewMode}
      isPublic={isPublic}
      identity={{ pipelineId, projectId, entityProjectId, validateProjectId, versionId }}
      create={create}
      setIsYamlDirty={setIsYamlDirty}
      onStopRun={handle.onStopRun}
      onAttachmentToolChange={handleAttachmentToolChange}
      editorPanelRef={editorPanelRef}
      deps={deps}
      versionDetails={versionDetails}
    />
  );

  return deps.renderShell({
    isVisible,
    isDirty: totalDirty,
    onClose: handleClose,
    title: editorTitle,
    subtitle: editorSubtitle,
    error: fetchError,
    onDirtyStateChange: onPipelineDirtyStateChange,
    onDiscard: handleDiscard,
    formContent,
    saveButton,
    isPublic: !canEditIt,
    children: body,
  });
};

export const PipelineEditor = forwardRef<PipelineEditorHandle, PipelineEditorProps>(PipelineEditorInner);
