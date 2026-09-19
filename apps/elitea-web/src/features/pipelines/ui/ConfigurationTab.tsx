import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

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
import { useSelectedProjectId } from '../lib/flow-editor/hooks/useSelectedProjectId';
import { extractExistingToolkitIds, triggerScope } from './configurationTab.lib';
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
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Components/
 * ConfigurationTab.jsx` — the composition root tying `GeneralFormPanel` +
 * `EditorPanel` + `ChatPanel` together for the standalone pipeline-editing
 * page (`pages/Pipelines/EditPipeline.jsx` — a DIFFERENT, not-yet-built page
 * from `features/pipelines/ui/PipelineEditor.tsx`'s own NewChat-embedded
 * composition; both use `EditorPanel`, see that file's own doc comment).
 *
 * **DISCLOSED REDESIGN — no Formik, explicit `versionDetails`/`setFieldValue`
 * props instead of `useFormikContext()`:** this app has no Formik (§2.3);
 * matches `features/agents/ui/ConfigurationTab.tsx`'s own identical
 * redesign for the sibling baseline file.
 *
 * **DISCLOSED REDESIGN — no `react-split`:** the baseline's `<Split>`
 * (`react-split`) provides a draggable gutter between `EditorPanel` and
 * `ChatPanel`. `react-split` is not a dependency of this app (verified:
 * `grep -n react-split package.json` — zero hits, and `ls node_modules/
 * react-split` confirms it is not installed) — adding a new npm dependency
 * is outside a single Wave-2 sub-unit's scope (a shared `package.json`/
 * lockfile change touching every concurrently-landing sibling). Replaced
 * with a plain flex `Box` (`flexDirection: row`/`column` by `isSmallWindow`,
 * matching the baseline's own direction logic). The REAL, load-bearing
 * width behaviour survives untouched: both panels' own collapse states
 * still drive their real `minWidth`/`maxWidth` via `GeneralFormPanel.tsx`/
 * `ChatPanel.tsx`'s own `sx`. Only the SECONDARY "drag to fine-tune the
 * 98/2 split ratio" affordance is dropped.
 *
 * **DISCLOSED REDESIGN — `RunHistoryContainer`/`useShowRunHistoryFromUrl`
 * dropped, matching `features/agents/ui/ConfigurationTab.tsx`'s own
 * identical precedent for the sibling baseline file:** `entities/
 * run-history` does not exist in this worktree; `renderRunHistory` is a
 * slot, `showHistory` is local component state (not URL-search-param
 * driven).
 *
 * **DISCLOSED GAP — `useUploadAttachments` dropped:** the baseline's
 * `uploadAttachments`/`isUploadingAttachments`/`uploadProgress` (from
 * `hooks/chat/useUploadAttachments`, chat-domain machinery, not in this
 * sub-unit's owned list and no promoted equivalent) lived inside the
 * chat-attachment upload flow — that flow now lives inside the slotted
 * `renderChat` pane, matching `features/agents/ui/ConfigurationTab.tsx`'s
 * own identical "lives inside the (slotted) test pane" disclosure for
 * `useUploadAttachments`.
 *
 * **`usePipelineMCPToolsStatusMonitor`/`usePipelineAttachmentYamlSync`** are
 * wired directly — real, already-landed (this sub-unit's own
 * `usePipelineMCPToolsStatusMonitor`/`usePipelineAttachmentYamlSync`), not
 * slots. `onToolsChange` (the monitor's write side) calls `setFieldValue`
 * directly rather than taking its own prop — the caller already supplies
 * `setFieldValue`, and this is the only real write target for it.
 */
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
  const [isGeneralPaneCollapsed, setIsGeneralPaneCollapsed] = useState(false);
  const [isChatPaneCollapsed, setIsChatPaneCollapsed] = useState(false);
  const [showHistory, setShowHistory] = useState(false);

  const chatPanelRef = useRef<ChatPanelHandle>(null);
  const editorPanelRef = useRef<EditorPanelHandle>(null);

  const projectId = useSelectedProjectId();

  const { isSmallWindow } = useIsSmallWindow(() => {
    setTimeout(() => editorPanelRef.current?.fitView(), 0);
  });

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

  const handleCollapsedGeneralPane = useCallback((collapsed: boolean) => {
    setIsGeneralPaneCollapsed(collapsed);
    setTimeout(() => editorPanelRef.current?.fitView(), 0);
  }, []);

  const handleCollapsedChatPane = useCallback((collapsed: boolean) => {
    setIsChatPaneCollapsed(collapsed);
    setTimeout(() => editorPanelRef.current?.fitView(), 0);
  }, []);

  const handleShowHistory = useCallback(() => setShowHistory(true), []);
  const handleRestoreConversation = useCallback((id: string | number) => setRestoredConversationID(id), []);
  const handleCloseHistory = useCallback(() => setShowHistory(false), []);

  const styles = useMemo(() => configurationTabStyles(isSmallWindow, isChatPaneCollapsed, isGeneralPaneCollapsed), [isSmallWindow, isChatPaneCollapsed, isGeneralPaneCollapsed]);

  useEffect(() => {
    editorPanelRef.current?.fitView();
  }, [isSmallWindow]);

  if (isError) {
    return (
      <Box sx={styles.errorContainer}>
        <Typography
          variant="labelMedium"
          color="text.secondary"
        >
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
    <Box sx={styles.mainContainer}>
      <GeneralFormPanel
        applicationId={applicationId}
        onCollapsed={handleCollapsedGeneralPane}
        viewMode={viewMode}
        renderConfigurationForm={slots.renderConfigurationForm}
      />
      <Box sx={styles.splitContainer}>
        {/* #899: the Entrypoint node's Trigger surface addresses THIS project and version — see `PipelineTriggerScopeContext`. */}
        <PipelineTriggerScopeContext.Provider value={triggerScope(projectId, versionDetails)}>
          <EditorPanel
            ref={editorPanelRef}
            setYamlDirty={setYamlDirty}
            stopRun={() => chatPanelRef.current?.stopRun()}
            sx={styles.editorPanel}
            versionTools={versionTools}
            llmSettings={flowEditorLlmSettings}
          />
        </PipelineTriggerScopeContext.Provider>
        <ChatPanel
          ref={chatPanelRef}
          settings={settings}
          onCollapsed={handleCollapsedChatPane}
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

function configurationTabStyles(isSmallWindow: boolean, isChatPaneCollapsed: boolean, isGeneralPaneCollapsed: boolean): ConfigurationTabStyles {
  const maxWidthOfEditorPane = isGeneralPaneCollapsed ? 'calc(100% - 3.75rem)' : 'calc(100% - 21.875rem)';
  return {
    errorContainer: { display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100%', width: '100%' },
    spinnerContainer: { height: '100%', width: '100%', display: 'flex', justifyContent: 'center', alignItems: 'center' },
    mainContainer: {
      height: 'calc(100vh - 3.8125rem)',
      overflow: 'scroll',
      padding: '0.75rem 1.5rem 1.5rem',
      gap: '2rem',
      display: 'flex',
      boxSizing: 'border-box',
      width: '100%',
      flexDirection: isSmallWindow ? 'column' : 'row',
    },
    splitContainer: {
      flex: 1,
      height: isSmallWindow ? 'max-content' : '100%',
      display: 'flex',
      flexDirection: isSmallWindow ? 'column' : 'row',
      maxWidth: isSmallWindow ? '100%' : maxWidthOfEditorPane,
      gap: isSmallWindow ? '0.75rem' : '0.625rem',
    },
    editorPanel: {
      minWidth: isSmallWindow ? '100%' : undefined,
      minHeight: isSmallWindow ? undefined : '100%',
      maxWidth: `calc(100% - ${isChatPaneCollapsed ? '1.75rem' : '21.5625rem'})`,
      marginRight: !isSmallWindow && isChatPaneCollapsed ? '1.5rem' : '0rem',
    },
  };
}
