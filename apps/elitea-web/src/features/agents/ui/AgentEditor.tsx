import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import { applicationCreationSchema, ApplicationSaveButton, ApplicationValidator } from '@/entities/application-form';
import type { ApplicationCreatedResponse, LlmSettings } from '@/shared/api/generated/model';
import { ChatParticipantType } from '@/shared/lib/chat';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { useSelectedProjectId } from '../api/useSelectedProjectId';
import {
  agentDisplayName,
  agentId as resolveAgentId,
  agentViewMode,
  canEditAgent,
  canEditModel as resolveCanEditModel,
  isPublicAgent,
  publicLlmOverride,
  resolveValidateProjectId,
  type AgentEditorAgentLike,
} from '../lib/agentEditorViewState';
import { useAgentEditorCreate } from '../lib/useAgentEditorCreate';
import { useHasPermission } from '../lib/useHasPermission';
import { useValidateAgentVersion } from '../lib/useValidateAgentVersion';
import { CreateAgentForm } from './CreateAgentForm';
import { GenerateAgentButton } from './generate-agent-modal/GenerateAgentButton';

/**
 * Ported from `apps/elitea-ui/src/pages/NewChat/AgentEditor.jsx`.
 *
 * **MUST be exported through `src/features/agents/index.ts`** — Wave-2
 * unit C6 needs it cross-feature (this sub-unit's own mission brief).
 *
 * **Real, load-bearing redesign, not a porting shortcut — read before
 * wiring this up.** The baseline is a thin shell around a `<Formik>`
 * ancestor (not itself part of this file) that every child reads via
 * `useFormikContext()`. This app has no Formik dependency
 * (`../model/types.ts`'s own doc comment: react-hook-form + zod instead,
 * "`shared/ui`/`features/` components should not assume a specific
 * form-library context is mounted above them"). This port therefore:
 *
 *  - Owns CREATE-mode form state itself (`../lib/useAgentEditorCreate.ts`)
 *    since `../ui/CreateAgentForm.tsx` (A1c, real, landed) takes
 *    `values`/`onFieldChange` as plain props, not context — AgentEditor is
 *    the natural (and only) owner of that state.
 *  - Does NOT own EDIT-mode form state. The baseline's
 *    `ApplicationConfigurationForm` (fed only `applicationId`/`viewMode`/a
 *    couple of flags, never `values`) has NO port anywhere in this
 *    worktree — verified directly, and independently confirmed by sibling
 *    `../ui/ConfigurationTab.tsx`'s own doc comment ("`find src/features/
 *    agents -iname '*ApplicationConfigurationForm*'` — zero hits... a
 *    sibling A1 sub-unit's own file, not this one's"). `deps.
 *    renderConfigurationForm` is a `(props) => ReactNode` slot with the
 *    same `{applicationId, viewMode}` core `ConfigurationTab.tsx` already
 *    established for this exact not-yet-landed dependency, plus the two
 *    extra fields the baseline's own `ApplicationConfigurationForm` call
 *    site (`AgentEditor.jsx:92-99`) passes (`entityProjectId`,
 *    `onAttachmentToolChange`) that `ConfigurationTab`'s own slot doesn't
 *    need — once the real sibling file lands, a caller passes `(props) =>
 *    <ApplicationConfigurationForm {...props} isChatView />` with zero
 *    change here.
 *  - `versionId` for `ApplicationValidator` comes straight off
 *    `agent.entity_settings.version_id` (no fetch needed — matches the
 *    baseline's own `const versionId = agent?.entity_settings?.version_id`,
 *    `AgentEditor.jsx:137`). `tools` is passed as `undefined` (that
 *    component's own `shouldSkipValidation` treats an empty/absent tools
 *    list as "skip" — a safe, non-crashing default): the version's real
 *    tool list lives inside the not-yet-landed configuration-form slot
 *    above, which this component has no reach into.
 *  - Chat-domain pieces genuinely unreachable from `features/agents`
 *    (`no-sideways-features`: `BaseEditor`/`LLMModelSelectorWrapper` are
 *    `features/chat`-owned; `no-upward-from-features`: `app/providers`'
 *    `InstructionsInputRefProvider` cannot be imported either) are
 *    injected via `deps` — GA event tracking (`useTrackEvent`, baseline
 *    `AgentEditor.jsx:5,123,284-290`) is dropped outright, same
 *    documented-gap treatment as `../model/useAgentCreation.ts`'s own doc
 *    comment (no analytics-event SDK exists anywhere in this app).
 *  - `isDirty` (baseline: `AgentEditor.jsx:126`, plain local state there,
 *    NOT Formik-derived) is `create.isDirty` in CREATE mode (this file owns
 *    that state, so it can answer "has anything changed" for real) and
 *    always `false` in EDIT mode — dirty-tracking for the not-yet-landed
 *    config-form slot's own edits is that slot's own concern, same call
 *    `ConfigurationTab.tsx`'s own doc comment already makes.
 */

export interface AgentEditorShellProps {
  readonly isVisible: boolean;
  readonly isDirty: boolean;
  readonly onClose: () => void;
  readonly title: string;
  readonly subtitle: string | undefined;
  readonly error: unknown;
  readonly onDirtyStateChange?: ((isDirty: boolean) => void) | undefined;
  /** Baseline: `AgentEditor.jsx:270-273`'s `handleDiscard`. Only wired for CREATE mode (resets `useAgentEditorCreate`'s `values` back to empty) — no EDIT-mode form state exists here to discard, so `undefined` rather than a silent no-op. */
  readonly onDiscard?: (() => void) | undefined;
  readonly saveButton: ReactNode;
  readonly isPublic: boolean;
  readonly children: ReactNode;
}

export interface AgentConfigurationFormSlotProps {
  readonly applicationId: string | number | undefined;
  readonly viewMode: 'Owner' | 'Public';
  readonly entityProjectId: string | number | undefined;
  readonly onAttachmentToolChange: (() => void) | undefined;
}

export interface AgentEditorDeps {
  /** The chat-owned editor chrome (baseline: `pages/NewChat/components/BaseEditor.jsx`) — see the module doc comment. */
  readonly renderShell: (props: AgentEditorShellProps) => ReactNode;
  /** The not-yet-landed sibling form — see the module doc comment. Omitted entirely (not rendered) when absent. */
  readonly renderConfigurationForm?: (props: AgentConfigurationFormSlotProps) => ReactNode;
  /**
   * Baseline: `pages/NewChat/components/LLMModelSelectorWrapper.jsx`, edit
   * mode only. `onConversationLlmOverride` is the baseline's own
   * `onPublicLlmOverride` (`AgentEditor.jsx:61-84`) — already gated to
   * `isPublic` by the caller (`AgentEditorBody`), so a real implementation
   * of this slot only needs to wire it into `LLMModelSelectorWrapper`'s own
   * `onPublicLlmOverride`/`onResetToDefaults` props and compute the
   * matching tooltips.
   */
  readonly renderLlmModelSelector?: (props: {
    projectId: string | undefined;
    disabled: boolean;
    isPublic: boolean;
    onConversationLlmOverride?: ((settings: LlmSettings | null) => void) | undefined;
  }) => ReactNode;
  /** Baseline: `useConversationStartersSync` (`features/chat/lib/hooks`). Defaults to a no-op — see the module doc comment. */
  readonly useConversationStartersSync?: (onChange: ((starters: readonly string[]) => void) | undefined) => void;
  /** Edit-mode save — owned by whoever supplies `renderConfigurationForm`'s real live state (this component does not own edit-mode values). */
  readonly onSaveVersion?: () => void;
  readonly isSavingVersion?: boolean;
  /** Baseline: `useRefetchAgentVersionDetailsOnClose`, simplified to a plain callback — see the module doc comment. */
  readonly onEditorClosed?: () => void;
}

export interface AgentEditorProps {
  readonly agent: AgentEditorAgentLike | null | undefined;
  readonly versionName?: string | undefined;
  readonly isVisible: boolean;
  readonly isCreateMode?: boolean;
  readonly onCloseAgentEditor?: () => void;
  readonly onAgentCreated?: (result: ApplicationCreatedResponse) => void;
  readonly onAgentDirtyStateChange?: (isDirty: boolean) => void;
  readonly onConversationStartersChange?: (starters: readonly string[]) => void;
  /** Baseline: `AgentEditor.jsx:297-303`'s `handleAttachmentToolChange`, which forwards the agent id (`agent?.id`) to this callback. */
  readonly onAttachmentToolChange?: (agentId: string | number | undefined) => void;
  readonly onAssociationWarning?: (message: string) => void;
  /**
   * Baseline: `onConversationLlmOverride` (`AgentEditor.jsx:121`) — enables
   * per-conversation LLM-model overrides for PUBLISHED PUBLIC agents (a
   * viewer with no edit permission can still pick a model for their own
   * conversation when the caller opts in by supplying this). Only takes
   * effect when the agent being edited `isPublicAgent` — see
   * `deps.renderLlmModelSelector`'s own doc comment for how it reaches the
   * real model selector.
   */
  readonly onConversationLlmOverride?: (settings: LlmSettings | null) => void;
  readonly deps: AgentEditorDeps;
}

function noopConversationStartersSync(_onChange: ((starters: readonly string[]) => void) | undefined): void {
  // No-op default — see `AgentEditorDeps.useConversationStartersSync`'s doc comment.
}

function noopSave(): void {}

/** `theAgentId`/`versionId` arrive as `string | number | undefined`; `ApplicationValidator` wants a real `number`. Extracted purely to keep `AgentEditor` under the complexity budget. */
function asValidatorId(value: string | number | undefined): number | undefined {
  const numeric = Number(value);
  return Number.isFinite(numeric) ? numeric : undefined;
}

/** `isCreateMode ? value : fallback` — this component only owns CREATE-mode state (module doc comment), so several values are only real in that mode. Extracted purely to keep `AgentEditor` under the complexity budget, same reason as `asValidatorId` above. */
function whenCreateMode<T>(isCreateMode: boolean, value: T, fallback: T): T {
  return isCreateMode ? value : fallback;
}

interface SaveButtonSlotProps {
  readonly isCreateMode: boolean;
  readonly onCreateSave: () => void;
  readonly isCreating: boolean;
  readonly canSaveCreate: boolean;
  readonly onSaveVersion: (() => void) | undefined;
  readonly isSavingVersion: boolean | undefined;
}

/** The create-vs-edit `ApplicationSaveButton` branch, extracted purely to keep `AgentEditor` under the complexity budget. */
function AgentEditorSaveButton({
  isCreateMode,
  onCreateSave,
  isCreating,
  canSaveCreate,
  onSaveVersion,
  isSavingVersion,
}: SaveButtonSlotProps): ReactNode {
  if (isCreateMode) {
    return (
      <ApplicationSaveButton
        onSave={onCreateSave}
        isSaving={isCreating}
        disabled={!canSaveCreate}
      />
    );
  }
  return (
    <ApplicationSaveButton
      onSave={onSaveVersion ?? noopSave}
      isSaving={isSavingVersion ?? false}
      disabled={!onSaveVersion}
    />
  );
}

/** The derived agent/view state `AgentEditorBody` needs — grouped into one prop to stay under the §3.5 12-prop budget. */
export interface EditorBodyAgentState {
  readonly isCreateMode: boolean;
  readonly theAgentId: string | number | undefined;
  readonly validateProjectId: string | undefined;
  readonly versionId: string | number | undefined;
  readonly canEditIt: boolean;
  /** `canEditIt || (isPublic && onConversationLlmOverride is supplied)` — baseline's `canEditModel`, `AgentEditor.jsx:62`. */
  readonly canEditModel: boolean;
  readonly isPublic: boolean;
  readonly viewMode: 'Owner' | 'Public';
  readonly projectId: string | undefined;
  readonly entityProjectId: string | number | undefined;
}

interface EditorBodyProps {
  readonly state: EditorBodyAgentState;
  readonly onAttachmentToolChange: (() => void) | undefined;
  readonly onAssociationWarning: ((message: string) => void) | undefined;
  /** Already gated to `isPublic` by the caller — see `EditorBodyAgentState.canEditModel`'s doc comment. */
  readonly onConversationLlmOverride: ((settings: LlmSettings | null) => void) | undefined;
  readonly create: ReturnType<typeof useAgentEditorCreate>;
  readonly handleAgentCreated: (result: ApplicationCreatedResponse) => void;
  readonly deps: AgentEditorDeps;
}

/** The validator + LLM-selector-slot + create-or-config-form body, extracted purely to keep `AgentEditor` under the complexity budget. */
function AgentEditorBody({ state, onAttachmentToolChange, onAssociationWarning, onConversationLlmOverride, create, handleAgentCreated, deps }: EditorBodyProps): ReactNode {
  const { isCreateMode, theAgentId, validateProjectId, versionId, canEditModel, isPublic, viewMode, projectId, entityProjectId } = state;
  return (
    <>
      <ApplicationValidator
        applicationId={asValidatorId(theAgentId)}
        projectId={validateProjectId}
        versionId={asValidatorId(versionId)}
        tools={undefined}
        isCreateMode={isCreateMode}
        useValidate={useValidateAgentVersion}
      />
      {!isCreateMode &&
        deps.renderLlmModelSelector?.({ projectId, disabled: !canEditModel, isPublic, onConversationLlmOverride })}
      {isCreateMode ? (
        <CreateAgentForm
          values={create.values}
          onFieldChange={create.onFieldChange}
          disabled={create.isCreating}
          showOutputContract
          generateAgentButtonSlot={
            <GenerateAgentButton
              onAgentCreated={handleAgentCreated}
              onAssociationWarning={onAssociationWarning}
            />
          }
        />
      ) : (
        deps.renderConfigurationForm?.({ applicationId: theAgentId, viewMode, entityProjectId, onAttachmentToolChange })
      )}
    </>
  );
}

export function AgentEditor({
  agent,
  versionName,
  isVisible,
  isCreateMode = false,
  onCloseAgentEditor,
  onAgentCreated,
  onAgentDirtyStateChange,
  onConversationStartersChange,
  onAttachmentToolChange,
  onAssociationWarning,
  onConversationLlmOverride,
  deps,
}: AgentEditorProps): ReactNode {
  const projectId = useSelectedProjectId();
  const hasEditPermission = useHasPermission(projectId, PERMISSIONS.applications.update);
  const useStartersSync = deps.useConversationStartersSync ?? noopConversationStartersSync;
  useStartersSync(onConversationStartersChange);

  const isPublic = isPublicAgent(agent);
  const canEditIt = canEditAgent(isPublic, hasEditPermission);
  // See `publicLlmOverride`/`canEditModel`'s own doc comments (`../lib/agentEditorViewState.ts`) for the baseline lines these restore.
  const conversationLlmOverride = publicLlmOverride(isPublic, onConversationLlmOverride);
  const canEditModel = resolveCanEditModel(canEditIt, conversationLlmOverride !== undefined);
  const viewMode = agentViewMode(canEditIt);
  const theAgentId = resolveAgentId(agent);
  const versionId = agent?.entity_settings?.version_id;
  const entityProjectId = agent?.entity_meta?.project_id;
  // See `resolveValidateProjectId`'s own doc comment (`../lib/agentEditorViewState.ts`) for the baseline line this restores.
  const validateProjectId = resolveValidateProjectId(entityProjectId, projectId);

  const create = useAgentEditorCreate(projectId);
  const isDirty = whenCreateMode(isCreateMode, create.isDirty, false);

  const handleClose = useCallback(() => {
    onCloseAgentEditor?.();
    deps.onEditorClosed?.();
  }, [onCloseAgentEditor, deps]);

  const handleAgentCreated = useCallback(
    (result: ApplicationCreatedResponse) => {
      onAgentCreated?.({ participantType: ChatParticipantType.Applications, ...result } as ApplicationCreatedResponse);
    },
    [onAgentCreated],
  );

  const handleCreateSubmit = useCallback(async () => {
    const result = await create.submit();
    if (result) handleAgentCreated(result);
  }, [create, handleAgentCreated]);

  // Baseline: `AgentEditor.jsx:270-273`'s `handleDiscard` — see `AgentEditorShellProps.onDiscard`'s own doc comment for why only CREATE mode gets a real one.
  const handleDiscard = useCallback(() => {
    create.reset();
  }, [create]);

  // Baseline: `AgentEditor.jsx:297-303`'s `handleAttachmentToolChange` — forwards the resolved agent id. The refetch half has no equivalent here: this component doesn't own the edit-mode version-details query (module doc comment) — that belongs to whichever real component eventually fills `renderConfigurationForm`'s slot, same as `deps.onEditorClosed` already replacing `useRefetchAgentVersionDetailsOnClose`.
  const handleAttachmentToolChange = useCallback(() => {
    onAttachmentToolChange?.(theAgentId);
  }, [onAttachmentToolChange, theAgentId]);

  const canSaveCreate = useMemo(() => applicationCreationSchema.safeParse(create.values).success, [create.values]);

  if (!agent && !isCreateMode) return null;

  const title = isCreateMode ? 'Create New Agent' : agentDisplayName(agent, 'Unnamed Agent');
  const subtitle = isCreateMode ? undefined : versionName;

  const saveButton = (
    <AgentEditorSaveButton
      isCreateMode={isCreateMode}
      onCreateSave={() => void handleCreateSubmit()}
      isCreating={create.isCreating}
      canSaveCreate={canSaveCreate}
      onSaveVersion={deps.onSaveVersion}
      isSavingVersion={deps.isSavingVersion}
    />
  );

  const bodyState: EditorBodyAgentState = {
    isCreateMode,
    theAgentId,
    validateProjectId,
    versionId,
    canEditIt,
    canEditModel,
    isPublic,
    viewMode,
    projectId,
    entityProjectId,
  };

  const body = (
    <AgentEditorBody
      state={bodyState}
      onAttachmentToolChange={handleAttachmentToolChange}
      onAssociationWarning={onAssociationWarning}
      onConversationLlmOverride={conversationLlmOverride}
      create={create}
      handleAgentCreated={handleAgentCreated}
      deps={deps}
    />
  );

  return deps.renderShell({
    isVisible,
    isDirty,
    onClose: handleClose,
    title,
    subtitle,
    // Mode-appropriate, matching the baseline's own `isPublishedAgent ? publicError : privateError`
    // (both `skip`ped in create mode, `AgentEditor.jsx:162-164`): `create.error`
    // is this component's OWN mutation-error state, real only for CREATE mode.
    error: whenCreateMode(isCreateMode, create.error, undefined),
    onDirtyStateChange: onAgentDirtyStateChange,
    onDiscard: whenCreateMode(isCreateMode, handleDiscard, undefined),
    saveButton,
    isPublic: !canEditIt,
    children: body,
  });
}
