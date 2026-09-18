import type { ReactNode, SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

import { ApplicationConfigurationLayout, ApplicationSaveButton, ApplicationValidator } from '@/entities/application-form';
import type { ApplicationVersionDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';
import { GearIcon } from '@/shared/ui/icons/gear-icon';

import { EditorPanel, type EditorPanelHandle } from './EditorPanel';
import { useFlowEditorVersionInputs } from '../lib/hooks/useFlowEditorVersionInputs';
import { useValidatePipelineVersion } from '../lib/useValidatePipelineVersion';

export type { EditorPanelHandle } from './EditorPanel';
import type { usePipelineEditorCreate } from '../lib/usePipelineEditorCreate';
import type { PipelineDraftValues, PipelineFieldChange } from '../model/types';

/**
 * `PipelineEditor.tsx`'s presentational sub-components — split into this
 * sibling file purely to keep `PipelineEditor.tsx` under the §3.5 400-line
 * file budget and its own top-level function under the cyclomatic-
 * complexity budget (12). Every one of these was previously inline in
 * `PipelineEditor.jsx`'s single render tree (`PipelineEditorContent`, the
 * Configuration/Flow-editor `<Tabs>`, the create/edit `ApplicationSaveButton`
 * branch, the validator + tab-body composition) — none is a real, separate
 * old-app file, so none is independently cited beyond the line ranges
 * already given in `PipelineEditor.tsx`'s own module doc comment.
 */

const PUBLIC_MODEL_TOOLTIP = 'Model configuration is locked for Public agents';
const PUBLIC_SETTINGS_TOOLTIP = 'Model settings are locked for Public agents';

/** The chat-owned editor chrome's own prop contract (baseline: `pages/NewChat/components/BaseEditor.jsx`) — see `PipelineEditor.tsx`'s module doc comment for why this is injected via `deps.renderShell` rather than imported directly. */
export interface PipelineEditorShellProps {
  readonly isVisible: boolean;
  readonly isDirty: boolean;
  readonly onClose: () => void;
  readonly title: string;
  readonly subtitle: string | undefined;
  readonly error: unknown;
  readonly onDirtyStateChange?: ((isDirty: boolean) => void) | undefined;
  readonly onDiscard: () => void;
  readonly formContent: ReactNode;
  readonly saveButton: ReactNode;
  readonly isPublic: boolean;
  readonly children: ReactNode;
}

interface PipelineConfigurationPanelSlotProps {
  readonly applicationId: string | number | undefined;
  readonly viewMode: 'Owner' | 'Public';
  readonly entityProjectId: string | number | undefined;
  readonly onAttachmentToolChange: (() => void) | undefined;
}

/** The six baseline `features/agent`-owned panels `ApplicationConfigurationLayout` slots — see `PipelineEditor.tsx`'s module doc comment. `tools` is the layout's one required slot; the rest match the baseline's `isChatView` visibility (`ApplicationConfigurationLayout.tsx`'s own doc comment). */
interface PipelineConfigurationPanels {
  readonly tools: ReactNode;
  readonly welcomeMessage?: ReactNode;
  readonly conversationStarters?: ReactNode;
  readonly advanceSettings?: ReactNode;
  readonly editorNotes?: ReactNode;
  readonly information?: ReactNode;
}

export interface PipelineCreateFormSlotProps {
  readonly values: PipelineDraftValues;
  readonly onFieldChange: PipelineFieldChange;
}

interface PipelineLlmModelSelectorSlotProps {
  readonly projectId: string | undefined;
  readonly disabled: boolean;
  readonly modelTooltip: string | undefined;
  readonly settingsTooltip: string | undefined;
}

export interface PipelineEditorDeps {
  readonly renderShell: (props: PipelineEditorShellProps) => ReactNode;
  readonly renderConfigurationPanels?: (props: PipelineConfigurationPanelSlotProps) => PipelineConfigurationPanels;
  readonly renderCreateForm?: (props: PipelineCreateFormSlotProps) => ReactNode;
  readonly renderLlmModelSelector?: (props: PipelineLlmModelSelectorSlotProps) => ReactNode;
  readonly useConversationStartersSync?: (onChange: ((starters: readonly string[]) => void) | undefined) => void;
  /**
   * Edit-mode save trigger. Takes an `onSuccess` callback (matching the
   * baseline's own `<SaveApplicationButton onSuccess={handleSaveSuccess}
   * />` injection, `PipelineEditor.jsx:505`) that the caller (`PipelineEditor.tsx`)
   * calls with the saved payload to reset its own dirty flags and forward
   * `onPipelineSaved` — the caller supplying this dep owns the actual save
   * mutation (it needs the not-yet-landed configuration-form slot's live
   * field values) but reports back through the same callback shape.
   */
  readonly onSaveVersion?: (onSuccess: (savedFormData: unknown) => void) => void;
  readonly isSavingVersion?: boolean;
  /** `true` once the same caller-owned Configuration form has an unsaved edit — folded into `PipelineEditor.tsx`'s `totalDirty` alongside `isDirty`/`isYamlDirty`, neither of which ever sees one (`elitea_issues #2223/#2664`'s Save gate). */
  readonly isConfigurationDirty?: boolean;
  readonly onEditorClosed?: () => void;
}

function noopSave(): void {}

/** `pipelineId`/`versionId` arrive as `string | number | undefined`; `ApplicationValidator` wants a real `number`. */
function asValidatorId(value: string | number | undefined): number | undefined {
  const numeric = Number(value);
  return Number.isFinite(numeric) ? numeric : undefined;
}

interface SaveButtonSlotProps {
  readonly isCreateMode: boolean;
  readonly onCreateSave: () => void;
  readonly isCreating: boolean;
  readonly canSaveCreate: boolean;
  readonly isSavingVersion: boolean | undefined;
  readonly deps: PipelineEditorDeps;
  readonly onSaveSuccess: (savedFormData: unknown) => void;
  /**
   * `true` once the open pipeline actually differs from its last
   * loaded/saved snapshot (`PipelineEditor.tsx`'s `totalDirty` —
   * `isDirty || isYamlDirty`).
   *
   * `elitea_issues #2223 / #2664`: in EDIT mode this save button used to be
   * gated only on `!onSaveVersion` — enabled the instant the Canvas editor
   * opened (a save handler is wired from the first render), with no regard
   * to whether the user had actually changed anything, and staying enabled
   * across a bare Configuration<->Flow-editor tab switch. `AgentEditor.tsx`'s
   * standalone `pages/agents/EditApplication.tsx` page already carries the
   * equivalent fix (`canSaveApplication(isValid, isSaving, isDirty)`,
   * elitea_issues #5107/#5987) for the full agent edit page; this Canvas
   * save button (used for both pipeline and agent participants opened from
   * chat) had not received it. Only the PIPELINE half is fixed here — it is
   * this package's own owned file; the agent Canvas editor
   * (`features/agents/ui/AgentEditor.tsx`) shares the identical gap and is
   * out of this package's scope.
   */
  readonly isDirty: boolean;
}

/** The create-vs-edit `ApplicationSaveButton` branch — mirrors `AgentEditor.tsx`'s own `AgentEditorSaveButton`. Resolves `deps.onSaveVersion`'s presence/binding itself (rather than the caller doing it) purely to keep `PipelineEditor`'s own complexity under budget. */
export function PipelineEditorSaveButton({ isCreateMode, onCreateSave, isCreating, canSaveCreate, isSavingVersion, deps, onSaveSuccess, isDirty }: SaveButtonSlotProps): ReactNode {
  if (isCreateMode) {
    return (
      <ApplicationSaveButton
        onSave={onCreateSave}
        isSaving={isCreating}
        disabled={!canSaveCreate}
        testId="pipeline-save-button"
      />
    );
  }
  const onSaveVersion = deps.onSaveVersion;
  return (
    <ApplicationSaveButton
      onSave={onSaveVersion ? () => onSaveVersion(onSaveSuccess) : noopSave}
      isSaving={isSavingVersion ?? false}
      disabled={!onSaveVersion || !isDirty}
      testId="pipeline-save-button"
    />
  );
}

interface ConfigurationTabProps {
  readonly viewMode: 'Owner' | 'Public';
  readonly isPublic: boolean;
  readonly pipelineId: string | number | undefined;
  readonly projectId: string | undefined;
  readonly entityProjectId: string | number | undefined;
  readonly onAttachmentToolChange: (() => void) | undefined;
  readonly deps: PipelineEditorDeps;
}

/** `PipelineEditor.jsx`'s `PipelineEditorContent` (Configuration-tab body: LLM selector slot + `ApplicationConfigurationLayout` fed the six not-yet-landed panel slots). */
function PipelineConfigurationTab({ viewMode, isPublic, pipelineId, projectId, entityProjectId, onAttachmentToolChange, deps }: ConfigurationTabProps): ReactNode {
  const panels = deps.renderConfigurationPanels?.({
    applicationId: pipelineId,
    viewMode,
    entityProjectId,
    onAttachmentToolChange,
  });

  return (
    <Box>
      {deps.renderLlmModelSelector?.({
        projectId,
        disabled: viewMode !== 'Owner',
        modelTooltip: isPublic ? PUBLIC_MODEL_TOOLTIP : undefined,
        settingsTooltip: isPublic ? PUBLIC_SETTINGS_TOOLTIP : undefined,
      })}
      <ApplicationConfigurationLayout
        viewMode={viewMode}
        isChatView
        tools={panels?.tools ?? null}
        welcomeMessage={panels?.welcomeMessage}
        conversationStarters={panels?.conversationStarters}
        advanceSettings={panels?.advanceSettings}
        editorNotes={panels?.editorNotes}
        information={panels?.information}
      />
    </Box>
  );
}

interface PipelineEditorTabsProps {
  readonly isCreateMode: boolean;
  readonly activeTab: number;
  readonly onTabChange: (event: SyntheticEvent, newValue: number) => void;
}

/** The Configuration/Flow-editor tab bar (`PipelineEditor.jsx:474-500`) — `null` in create mode (baseline: no tab bar until the pipeline exists). Takes `isCreateMode` itself (rather than the caller branching) purely to keep `PipelineEditor`'s own complexity under budget. */
export function PipelineEditorTabs({ isCreateMode, activeTab, onTabChange }: PipelineEditorTabsProps): ReactNode {
  if (isCreateMode) return null;
  return (
    <Tabs
      value={activeTab}
      onChange={onTabChange}
      aria-label={t('features.pipelines.pipelineEditor.tabsAriaLabel', 'pipeline editor tabs')}
    >
      <Tab
        icon={<GearIcon />}
        label={t('features.pipelines.pipelineEditor.configurationTab', 'Configuration')}
        iconPosition="start"
      />
      <Tab
        icon={<FlowIcon />}
        label={t('features.pipelines.pipelineEditor.flowEditorTab', 'Flow editor')}
        iconPosition="start"
      />
    </Tabs>
  );
}

/** The pipeline/version identifiers `PipelineEditorBody` and its children thread through to the validator, the Configuration tab, and (eventually) a save payload — grouped into one object purely to keep `PipelineEditorBody`'s own prop count under the §3.5 budget (12), same "group into one option object" convention `BasicAccordion`/`InputBase` already established in this codebase. */
interface PipelineEditorIdentity {
  readonly pipelineId: string | number | undefined;
  readonly projectId: string | undefined;
  readonly entityProjectId: string | number | undefined;
  readonly validateProjectId: string | undefined;
  readonly versionId: string | number | undefined;
}

export interface PipelineEditorBodyProps {
  readonly isCreateMode: boolean;
  readonly activeTab: number;
  readonly viewMode: 'Owner' | 'Public';
  readonly isPublic: boolean;
  readonly identity: PipelineEditorIdentity;
  readonly create: ReturnType<typeof usePipelineEditorCreate>;
  readonly setIsYamlDirty: (dirty: boolean) => void;
  /** Matches `PipelineEditorHandle.onStopRun`'s `(isChat: boolean) => void` signature — this component supplies `true` when wiring `EditorPanel`'s own `stopRun: () => void` prop (baseline: the editor's internal stop button always means "stop initiated from inside the chat editor", the same case the baseline's `onStopRun(true)` branch covers). */
  readonly onStopRun: (isChat: boolean) => void;
  readonly onAttachmentToolChange: () => void;
  readonly editorPanelRef: { current: EditorPanelHandle | null };
  readonly deps: PipelineEditorDeps;
  /**
   * `usePipelineEditorLifecycle.ts`'s own `usePipelineVersionQuery` result,
   * threaded down from `PipelineEditor.tsx` so this component's own
   * `<EditorPanel>` call site can supply the `versionTools`/`llmSettings`
   * `FlowWrapper.tsx`'s module doc comment names as real but unreachable
   * from here — see `../lib/flowEditorVersionInputs.helpers.ts` for the
   * wire -> `FlowWrapperProps` mapping (same one `ConfigurationTab.tsx`
   * uses for its own, sibling `<EditorPanel>` call site).
   */
  readonly versionDetails: ApplicationVersionDetail | undefined;
}

/** The validator + create-form-or-configuration-tab + flow-editor-tab body. */
export function PipelineEditorBody({
  isCreateMode,
  activeTab,
  viewMode,
  isPublic,
  identity,
  create,
  setIsYamlDirty,
  onStopRun,
  onAttachmentToolChange,
  editorPanelRef,
  deps,
  versionDetails,
}: PipelineEditorBodyProps): ReactNode {
  const { pipelineId, projectId, entityProjectId, validateProjectId, versionId } = identity;
  const { versionTools, llmSettings } = useFlowEditorVersionInputs(versionDetails);
  return (
    <>
      <ApplicationValidator
        applicationId={asValidatorId(pipelineId)}
        projectId={validateProjectId}
        versionId={asValidatorId(versionId)}
        tools={undefined}
        isCreateMode={isCreateMode}
        useValidate={useValidatePipelineVersion}
      />

      {isCreateMode && deps.renderCreateForm?.({ values: create.values, onFieldChange: create.onFieldChange })}

      {!isCreateMode && activeTab === 0 && (
        <PipelineConfigurationTab
          viewMode={viewMode}
          isPublic={isPublic}
          pipelineId={pipelineId}
          projectId={projectId}
          entityProjectId={entityProjectId}
          onAttachmentToolChange={onAttachmentToolChange}
          deps={deps}
        />
      )}

      {!isCreateMode && activeTab === 1 && (
        <EditorPanel
          ref={editorPanelRef}
          setYamlDirty={setIsYamlDirty}
          disabled={viewMode !== 'Owner'}
          stopRun={() => onStopRun(true)}
          versionTools={versionTools}
          llmSettings={llmSettings}
        />
      )}

      {isCreateMode && activeTab === 1 && <Box>{t('features.pipelines.pipelineEditor.saveToAccessFlowEditor', 'Save the pipeline to access the flow editor.')}</Box>}
    </>
  );
}
