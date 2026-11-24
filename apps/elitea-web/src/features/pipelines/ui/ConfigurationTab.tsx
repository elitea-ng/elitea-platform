import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { useEditorPaneRoom } from '../lib/hooks/useEditorPaneRoom';
import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import { usePipelineAttachmentYamlSync } from '../lib/hooks/usePipelineAttachmentYamlSync.hooks';
import { usePipelineChat } from '../lib/hooks/usePipelineChat.hooks';
import type {
  ChatConversationAdapter,
  ChatPipelineVersionDetails,
  UsePipelineChatResult,
} from '../lib/hooks/usePipelineChat.hooks';
import { usePipelineMCPToolsStatusMonitor } from '../lib/hooks/usePipelineMCPToolsStatusMonitor';
import type { PipelineMcpToolLike } from '../lib/hooks/usePipelineMCPToolsStatusMonitor';
import { PipelineTriggerScopeContext } from '../lib/flow-editor/flowEditorContext';
import { getUnsavedTestReason } from '../lib/flow-editor/helpers/pipelineReadiness.helpers';
import { useSelectedProjectId } from '../lib/flow-editor/hooks/useSelectedProjectId';
import { extractExistingToolkitIds, mainOverflow, triggerScope } from './configurationTab.lib';
import { DEFAULT_MAX_TOKENS, DEFAULT_TEMPERATURE } from '../lib/flowEditorVersionInputs.helpers';
import { useFlowEditorVersionInputs } from '../lib/hooks/useFlowEditorVersionInputs';
import { ChatPanel } from './ChatPanel';
import type { ChatPanelHandle, ChatPanelProps } from './ChatPanel';
import { EditorPanel } from './EditorPanel';
import type { EditorPanelHandle } from './EditorPanel';
import { GeneralFormPanel } from './GeneralFormPanel';
import type { GeneralFormPanelProps } from './GeneralFormPanel';

// Baseline `@/[fsd]/shared/lib/constants/llmSettings.constants.js` — not ported to `shared/lib/`
// in this worktree (verified: `grep -rn DEFAULT_MAX_TOKENS src/shared` — zero hits).
// `DEFAULT_MAX_TOKENS`/`DEFAULT_TEMPERATURE` now live in `../lib/flowEditorVersionInputs.helpers.ts`.
const DEFAULT_REASONING_EFFORT = 'medium';

const EMPTY_ARRAY: readonly never[] = [];

interface UseConfigurationTabSettingsArgs {
  readonly chat: UsePipelineChatResult;
  readonly versionDetails: ChatPipelineVersionDetails | undefined;
  readonly applicationId: string | number | undefined;
  readonly interaction_uuid: string;
  readonly unsavedLLMSettings: Readonly<Record<string, unknown>> | undefined;
  readonly setUnsavedLLMSettings: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly onStopRun: () => void;
  readonly onRcvAgentEvent: (event: unknown) => void;
  readonly deleteAllRunNodes: () => void;
}

/** Builds `ChatPanel`'s `settings` prop from `usePipelineChat`'s result plus the version's own fields — extracted to keep `ConfigurationTab`'s own cyclomatic complexity under this codebase's gate; matches `ConfigurationTab.jsx`'s own `settings` `useMemo` (lines 148-227). */
function useConfigurationTabSettings(args: UseConfigurationTabSettingsArgs): Readonly<Record<string, unknown>> {
  const { chat, versionDetails, applicationId, interaction_uuid, unsavedLLMSettings, setUnsavedLLMSettings, onStopRun, onRcvAgentEvent, deleteAllRunNodes } = args;
  const { llm_settings = {}, tools, id: currentVersionId, type = 'chat', conversation_starters } = versionDetails ?? {};
  const { model_name, model_project_id, max_tokens = DEFAULT_MAX_TOKENS, temperature = DEFAULT_TEMPERATURE, reasoning_effort = DEFAULT_REASONING_EFFORT } = llm_settings;

  const memoizedLlmSettings = useMemo(
    () => ({ model_name, model_project_id, temperature, max_tokens, reasoning_effort }),
    [model_name, model_project_id, temperature, max_tokens, reasoning_effort],
  );
  const conversationStarters = conversation_starters ?? EMPTY_ARRAY;
  const existingToolkitIds = useMemo(() => extractExistingToolkitIds(tools), [tools]);

  // A plain object literal, NOT wrapped in `useMemo` — this codebase's own `hook-deps` budget
  // (§3.5, enforced by `scripts/check-budgets.mjs`) caps a `useMemo`'s dependency array at 8
  // entries; this object has 13 real, independently-reactive inputs (matching the baseline's own
  // `ConfigurationTab.jsx` 25-entry `settings` `useMemo` deps array, lines 190-226 — the ORIGINAL
  // was already past what a hand-audited dependency array can track correctly). `chat`/
  // `memoizedLlmSettings`/`existingToolkitIds` are already independently memoised above (and
  // `usePipelineChat`'s own return, `ConfigurationTab`'s own `useCallback`s) — this object is
  // cheap to rebuild (no work beyond the property list itself), so recomputing it every render
  // trades a negligible allocation for a genuinely correct, budget-compliant dependency story
  // instead of a hand-maintained 13-entry array that would immediately breach the same budget.
  return {
    ...chat,
    llmSettings: memoizedLlmSettings,
    type,
    conversationStarters,
    isFullScreenChat: false,
    isAgentsPage: true,
    tools,
    currentVersionId,
    application_id: applicationId,
    disableChat: !applicationId,
    onStopRun,
    onRcvAgentEvent,
    deleteAllRunNodes,
    interaction_uuid,
    existingToolkitIds,
    unsavedLLMSettings,
    setUnsavedLLMSettings,
  };
}

export interface ConfigurationTabRunHistoryRenderProps {
  readonly applicationId: string | number | undefined;
  readonly versions: readonly unknown[];
  readonly onRestoreConversation: (conversationId: string | number) => void;
  readonly onClose: () => void;
}

export interface ConfigurationTabSlots {
  /** The left panel's field-editing content — see `GeneralFormPanel.tsx`'s own doc comment for why this is a slot. */
  readonly renderConfigurationForm: GeneralFormPanelProps['renderConfigurationForm'];
  /** The right panel's live test-chat content — see `ChatPanel.tsx`'s own doc comment for why this is a slot. */
  readonly renderChat: ChatPanelProps['renderChat'];
  readonly renderClearChatButton?: ChatPanelProps['renderClearChatButton'];
  readonly renderContextBudget?: ChatPanelProps['renderContextBudget'];
  /** The full-width run-history view, swapped in for the two-panel layout while open — mirrors `features/agents/ui/ConfigurationTab.tsx`'s own `renderRunHistory` (same real dependency gap: no `entities/run-history` exists in this worktree). */
  readonly renderRunHistory?: (props: ConfigurationTabRunHistoryRenderProps) => ReactNode;
}

/** @public */
export interface ConfigurationTabProps {
  readonly isFetching: boolean;
  readonly isError: boolean;
  readonly applicationId: string | number | undefined;
  readonly pipelineName: string | undefined;
  readonly versionDetails: ChatPipelineVersionDetails | undefined;
  readonly versions?: readonly unknown[] | undefined;
  readonly setFieldValue: (field: string, value: unknown) => void;
  readonly setYamlDirty: (dirty: boolean) => void;
  /** Grouped into one option object — §3.5's prop-count budget, same "group into one option object" move `AddNodeMenu.tsx`'s sibling files document for this codebase. */
  readonly unsavedLLMSettings?: Readonly<Record<string, unknown>> | undefined;
  readonly setUnsavedLLMSettings?: ((settings: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly adapter: ChatConversationAdapter;
  readonly viewMode?: GeneralFormPanelProps['viewMode'];
  readonly slots: ConfigurationTabSlots;
}

/**
 * Ported from `EliteaUI` `widgets/pipeline-editor/ui/PipelineEditorWorkspace.jsx`.
 * Composes `GeneralFormPanel` (auto-collapses on first node interaction) +
 * `EditorPanel` (Flow/YAML + `"Test Pipeline"` toolbar button) + on-demand resizable `ChatPanel`.
 */
function useGeneralAutoCollapse(isSmallWindow: boolean, foldGeneral: boolean, onFitView: () => void) {
  const [isGeneralPaneCollapsed, setIsGeneralPaneCollapsed] = useState(false);
  const hasAutoCollapsedRef = useRef(false);

  const handleCollapsedGeneralPane = useCallback(
    (collapsed: boolean) => {
      setIsGeneralPaneCollapsed(collapsed);
      onFitView();
    },
    [onFitView],
  );

  const handleCanvasNodeInteraction = useCallback(() => {
    if (hasAutoCollapsedRef.current || isSmallWindow || isGeneralPaneCollapsed) return;
    hasAutoCollapsedRef.current = true;
    setIsGeneralPaneCollapsed(true);
    onFitView();
  }, [isGeneralPaneCollapsed, isSmallWindow, onFitView]);

  const effectiveCollapsed = isSmallWindow ? false : (isGeneralPaneCollapsed || foldGeneral);
  return { effectiveCollapsed, setIsGeneralPaneCollapsed, handleCollapsedGeneralPane, handleCanvasNodeInteraction };
}

function resolveUnsavedTestReason(applicationId: string | number | undefined): string {
  return applicationId === undefined ? getUnsavedTestReason() : '';
}

function buildRestoreProp(restoredConversationID: string | number | null, onComplete: () => void) {
  return {
    conversationId: restoredConversationID == null ? undefined : String(restoredConversationID),
    onComplete,
  };
}

export function ConfigurationTab(props: ConfigurationTabProps): ReactNode {
  const {
    isFetching,
    isError,
    applicationId,
    pipelineName,
    versionDetails,
    versions = [],
    setFieldValue,
    setYamlDirty,
    unsavedLLMSettings,
    setUnsavedLLMSettings,
    adapter,
    viewMode,
    slots,
  } = props;

  const [restoredConversationID, setRestoredConversationID] = useState<string | number | null>(null);
  const [isTestPanelOpen, setIsTestPanelOpen] = useState(false);
  const [showHistory, setShowHistory] = useState(false);

  const splitContainerRef = useRef<HTMLDivElement | null>(null);
  const chatPanelRef = useRef<ChatPanelHandle>(null);
  const editorPanelRef = useRef<EditorPanelHandle>(null);

  const projectId = useSelectedProjectId();
  const room = useEditorPaneRoom();

  const handleFitView = useCallback(() => {
    setTimeout(() => editorPanelRef.current?.fitView(), 0);
  }, []);

  const { isSmallWindow } = useIsSmallWindow(handleFitView);
  const generalPane = useGeneralAutoCollapse(isSmallWindow, room.foldGeneral, handleFitView);

  const onRestoreConversationComplete = useCallback(() => {
    setRestoredConversationID(null);
    setShowHistory(false);
  }, []);

  const handleDeleteAllRunNodes = useCallback(() => {
    editorPanelRef.current?.deleteAllRunNodes();
  }, []);

  const hasAttachments = Boolean(versionDetails?.meta?.internal_tools?.includes('attachments'));
  usePipelineAttachmentYamlSync(hasAttachments);

  const handleMcpToolsChange = useCallback(
    (nextTools: readonly (PipelineMcpToolLike & { readonly online?: boolean })[]) => setFieldValue('version_details.tools', nextTools),
    [setFieldValue],
  );
  usePipelineMCPToolsStatusMonitor({ tools: versionDetails?.tools as readonly PipelineMcpToolLike[] | undefined, projectId, onToolsChange: handleMcpToolsChange });

  const interaction_uuid = useMemo(() => `pipeline_${String(applicationId)}_${Date.now()}`, [applicationId]);

  const chat: UsePipelineChatResult = usePipelineChat({
    pipelineId: applicationId,
    pipelineName,
    pipelineVersionDetails: versionDetails,
    projectId,
    setFieldValue,
    adapter,
    deleteAllRunNodes: handleDeleteAllRunNodes,
    restoredConversationID,
    onRestoreConversationComplete,
  });

  const onStopRun = useCallback(() => editorPanelRef.current?.onStopRun(), []);
  const onRcvAgentEvent = useCallback((event: unknown) => editorPanelRef.current?.onRcvAgentEvent(event), []);
  const handleHasRunsInProgress = useCallback(() => editorPanelRef.current?.hasRunsInProgress() ?? false, []);

  const settings = useConfigurationTabSettings({
    chat,
    versionDetails,
    applicationId,
    interaction_uuid,
    unsavedLLMSettings,
    setUnsavedLLMSettings,
    onStopRun,
    onRcvAgentEvent,
    deleteAllRunNodes: handleDeleteAllRunNodes,
  });

  // `FlowWrapper.tsx`'s own module doc comment names this component as the real, missing source.
  const { versionTools, llmSettings: flowEditorLlmSettings } = useFlowEditorVersionInputs(versionDetails);

  const handleOpenTestPanel = useCallback(() => setIsTestPanelOpen(true), []);
  const handleCloseTestPanel = useCallback(() => setIsTestPanelOpen(false), []);

  const handleShowHistory = useCallback(() => setShowHistory(true), []);
  const handleRestoreConversation = useCallback((id: string | number) => {
    setRestoredConversationID(id);
    setShowHistory(false);
    setIsTestPanelOpen(true);
  }, []);
  const handleCloseHistory = useCallback(() => setShowHistory(false), []);

  const styles = useMemo(() => configurationTabStyles(isSmallWindow, generalPane.effectiveCollapsed), [isSmallWindow, generalPane.effectiveCollapsed]);
  const testDisabledReason = resolveUnsavedTestReason(applicationId);
  const testPanelOptions = useMemo(
    () => ({ isOpen: isTestPanelOpen, onClose: handleCloseTestPanel, workspaceRef: splitContainerRef, onResized: handleFitView }),
    [isTestPanelOpen, handleCloseTestPanel, handleFitView],
  );

  useEffect(() => {
    editorPanelRef.current?.fitView();
  }, [isSmallWindow]);

  if (isError) {
    return (
      <Box sx={styles.errorContainer}>
        <Typography variant="labelMedium" color="text.secondary">
          {t('features.pipelines.configurationTab.loadError', 'Failed to load data! Please try refreshing the page.')}
        </Typography>
      </Box>
    );
  }

  if (isFetching) {
    return (
      <Box sx={styles.spinnerContainer}>
        <CircularProgress />
      </Box>
    );
  }

  if (showHistory && slots.renderRunHistory) {
    return (
      <>
        {slots.renderRunHistory({
          applicationId,
          versions,
          onRestoreConversation: handleRestoreConversation,
          onClose: handleCloseHistory,
        })}
      </>
    );
  }

  return (
    <Box ref={room.measureRef} sx={styles.mainContainer}>
      <GeneralFormPanel
        applicationId={applicationId}
        onCollapsed={generalPane.handleCollapsedGeneralPane}
        collapsed={generalPane.effectiveCollapsed}
        onCollapsedChange={generalPane.setIsGeneralPaneCollapsed}
        autoFolded={room.foldGeneral}
        viewMode={viewMode}
        renderConfigurationForm={slots.renderConfigurationForm}
      />
      <Box ref={splitContainerRef} sx={styles.splitContainer}>
        {/* #899: the Entrypoint node's Trigger surface addresses THIS project and version — see `PipelineTriggerScopeContext`. */}
        <PipelineTriggerScopeContext.Provider value={triggerScope(projectId, versionDetails)}>
          <EditorPanel
            ref={editorPanelRef}
            setYamlDirty={setYamlDirty}
            stopRun={() => chatPanelRef.current?.stopRun()}
            sx={styles.editorPanel}
            versionTools={versionTools}
            llmSettings={flowEditorLlmSettings}
            onTestPipeline={handleOpenTestPanel}
            testDisabledReason={testDisabledReason}
            isTestPanelOpen={isTestPanelOpen}
            onCanvasNodeInteraction={generalPane.handleCanvasNodeInteraction}
          />
        </PipelineTriggerScopeContext.Provider>
        <ChatPanel
          ref={chatPanelRef}
          settings={settings}
          restore={buildRestoreProp(restoredConversationID, onRestoreConversationComplete)}
          panel={testPanelOptions}
          setActiveConversation={chat.setActiveConversation}
          hasRunsInProgress={handleHasRunsInProgress}
          onShowHistory={applicationId !== undefined ? handleShowHistory : undefined}
          renderChat={slots.renderChat}
          renderClearChatButton={slots.renderClearChatButton}
          renderContextBudget={slots.renderContextBudget}
        />
      </Box>
    </Box>
  );
}

interface ConfigurationTabStyles {
  readonly errorContainer: SxProps<Theme>;
  readonly spinnerContainer: SxProps<Theme>;
  readonly mainContainer: SxProps<Theme>;
  readonly splitContainer: SxProps<Theme>;
  readonly editorPanel: SxProps<Theme>;
}

function editorPanelStyles(isSmallWindow: boolean): SxProps<Theme> {
  if (isSmallWindow) return { minWidth: '100%', minHeight: '32rem', flex: '0 0 auto', maxWidth: '100%', marginRight: '0rem' };
  return {
    flex: 1,
    minWidth: 0,
    minHeight: '100%',
    maxWidth: '100%',
  };
}

function splitContainerStyles(isSmallWindow: boolean, isGeneralPaneCollapsed: boolean): SxProps<Theme> {
  if (isSmallWindow) {
    return { flex: '0 0 auto', height: 'auto', width: '100%', display: 'flex', flexDirection: 'column', maxWidth: '100%', gap: '0.75rem' };
  }
  const maxWidth = isGeneralPaneCollapsed ? 'calc(100% - 3.75rem)' : 'calc(100% - 21.875rem)';
  return { flex: 1, minWidth: 0, height: '100%', display: 'flex', flexDirection: 'row', maxWidth, gap: '0.75rem' };
}

function configurationTabStyles(isSmallWindow: boolean, isGeneralPaneCollapsed: boolean): ConfigurationTabStyles {
  return {
    errorContainer: { display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100%', width: '100%' },
    spinnerContainer: { height: '100%', width: '100%', display: 'flex', justifyContent: 'center', alignItems: 'center' },
    mainContainer: {
      height: '100%',
      overflow: mainOverflow(isSmallWindow),
      padding: '0.75rem 1.5rem 1.5rem',
      gap: '2rem',
      display: 'flex',
      boxSizing: 'border-box',
      width: '100%',
      flexDirection: isSmallWindow ? 'column' : 'row',
    },
    splitContainer: splitContainerStyles(isSmallWindow, isGeneralPaneCollapsed),
    editorPanel: editorPanelStyles(isSmallWindow),
  };
}
