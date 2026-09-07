import type { ComponentType, ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ToolListError } from '@/shared/ui/ToolListError';

import { EditViewTabsEnum, IndexStatuses, IndexViewsEnum, IndexesToolsEnum } from '../../lib/constants/indexDetails.constants';
import { adjustIndexDataSchema, getMockToolkitIndexConversation, type IndexChatMessage } from '../../lib/helpers/indexChat.helpers';
import { toDisplayString } from '../../lib/helpers/displayString.local';
import { useIndexNameValidation } from '../../lib/hooks/useIndexNameValidation.hooks';
import type { IndexRow } from '../../model/indexesStore';

import type { EditToolDetail, IndexActionsProps, UseToolkitSchemasResult } from './IndexActions';
import { IndexActions } from './IndexActions';
import type { CredentialsSelectSlotProps } from './IndexScheduleModal';
import type { ChatMessageListProps, LLMModelSelectorProps } from './IndexChat';
import { IndexChat } from './IndexChat';
import type { IndexConfigToolsConfig, ToolFormFieldProps } from './IndexConfig';
import { IndexNameWrapper } from './IndexNameWrapper';
import { IndexViewToggler } from './IndexViewToggler';
import { IndexViews } from './IndexViews';
import {
  computeDefaultConfigValues,
  computeDisableHistoryTabReason,
  computeDisableRunTabReason,
  computeIndexConfigWrapperSx,
  validateToolkitForm,
  useIndexDetailsTabSync,
  TOOLKIT_CHAT_MODE_CREATE_INDEX,
} from './IndexDetails.helpers';
import type { SelectedToolSchemaRead, UseToolkitChatParams, UseToolkitChatResult } from './IndexDetails.helpers';

/** Re-exported for backward compatibility — `IndexesContainer.test.tsx`/`IndexDetails.test.tsx` (and any other consumer) import these two types from this module path; the underlying definitions moved to `IndexDetails.helpers.ts` (see that file's own doc comment) purely to keep this file under the 400-line budget. */
export type { UseToolkitChatParams, UseToolkitChatResult };

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexDetails/index.jsx` (unit A4a) — the per-index detail panel: header
 * (name + actions), the tab-switched config/history/run body, and the
 * test-chat panel, all wired to one `useToolkitChat` session.
 *
 * DISCLOSED DI, three independent constraints (each already established by
 * a sibling file in this sub-unit — see their own doc comments for the
 * full reasoning, summarised here):
 *
 *  1. `useToolkitChat` (`features/toolkits/lib/hooks`, unit A4b) — this
 *     hook is 400+ real lines with its own substantial dependency tree
 *     (socket context, LLM settings, conversation mutations, `useToast`)
 *     that is A4b's ownership, not A4a's, to build or duplicate. Injected
 *     as a hook prop (`useToolkitChat`) with the params/return contract
 *     read off the real baseline-ported source
 *     (`features/toolkits/lib/hooks/useToolkitChat.hooks.ts`), so it drops
 *     in with zero call-site changes.
 *  2. `useGetSelectedToolSchema` (`hooks/toolkit/useGetSelectedToolSchema.js`
 *     in the baseline — itself built on A4b's `useGetCurrentToolkitSchemas`
 *     plus a toolkit-available-tools query neither owned by A4a) —
 *     injected as `useSelectedToolSchema`.
 *  3. `useMcpAuthModal`/`McpAuthModal` (`features/mcps`, unit A5) —
 *     `no-sideways-features` forbids this slice importing another
 *     `features/*` slice, full stop (not a landing-order issue, a
 *     permanent layer rule) — injected as `onMcpAuthRequired`/
 *     `mcpAuthModal` (state-and-render owned by the caller).
 *
 * `ToolkitChatHelpers.validateToolkitForm` (baseline:
 * `features/toolkits/lib/helpers/toolkitChat.helpers.js`) is NOT injected
 * — it is a 16-line pure function with zero imports of its own (verified
 * by reading it in full), so it is ported directly below as a local,
 * non-exported helper rather than adding a fourth DI seam for something
 * this trivial and self-contained.
 */

export interface IndexDetailsProps {
  readonly index: IndexRow;
  readonly view: string;
  readonly traceNewIndex: (id: string | null, metadata: Record<string, unknown>) => void;
  readonly refetchIndexesList: () => void;
  readonly handleDeleteIndex: () => void;
  readonly isIndexDeleting?: boolean | undefined;
  readonly selectedIndexTools: readonly string[];
  readonly toolkitId: string;
  readonly editToolDetail?: EditToolDetail | null | undefined;
  readonly values: Record<string, unknown>;

  readonly useToolkitChat: (params: UseToolkitChatParams) => UseToolkitChatResult;
  /** #440: a RESULT OBJECT, not the bare schema — see `SelectedToolSchemaRead`. A lost read must not look like a tool that takes no arguments. */
  readonly useSelectedToolSchema: (params: { toolkitType: string; toolOptionType: string | null }) => SelectedToolSchemaRead;
  readonly useToolkitSchemas: (params: { isMCP: boolean }) => UseToolkitSchemasResult;
  readonly ToolFormField: ComponentType<ToolFormFieldProps>;
  readonly LLMModelSelector: ComponentType<LLMModelSelectorProps>;
  readonly ChatMessageList: ComponentType<ChatMessageListProps>;
  readonly ClearChatButton: ComponentType<{ onClear: () => void }>;
  readonly onMcpAuthRequired?: ((message: unknown) => void) | undefined;
  readonly mcpAuthModal?: ReactNode;
  readonly currentUserId?: string | number | undefined;
  readonly userPermissions?: readonly string[] | undefined;
  readonly currentProjectName?: string | undefined;
  readonly isPrivateProject?: boolean | undefined;
  readonly renderCredentialsSelect?: ((props: CredentialsSelectSlotProps) => ReactNode) | undefined;
}

const wrapperSx: SxProps<Theme> = { flexGrow: 1, maxWidth: 'calc(100% - 16.25rem)' };
const detailsHeaderSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  padding: '1rem 0rem 1rem 2rem',
  maxHeight: '3.75rem',
  borderBottom: (theme) => `0.0625rem solid ${theme.vars.palette.divider}`,
};
const mainContentSx: SxProps<Theme> = { display: 'flex', flex: '1 1 auto', height: 'calc(100% - 3.75rem)', padding: '1.5rem 0rem', boxSizing: 'border-box' };

export function IndexDetails(props: IndexDetailsProps): ReactNode {
  const {
    index,
    view,
    traceNewIndex,
    refetchIndexesList,
    handleDeleteIndex,
    isIndexDeleting,
    selectedIndexTools,
    toolkitId,
    editToolDetail,
    values,
    useToolkitChat,
    useSelectedToolSchema,
    useToolkitSchemas,
    ToolFormField,
    LLMModelSelector,
    ChatMessageList,
    ClearChatButton,
    onMcpAuthRequired,
    mcpAuthModal,
    currentUserId,
    userPermissions,
    currentProjectName,
    isPrivateProject,
    renderCredentialsSelect,
  } = props;

  const configInitialized = useRef(false);

  const isCreateView = view === IndexViewsEnum.create;

  const [toolInputVariables, setToolInputVariables] = useState<Record<string, unknown>>({});
  const { clearIndexNameError, indexNameError, updateIndexNameError, isIndexNameValid } = useIndexNameValidation();

  const disableRunTabReason = useMemo(() => computeDisableRunTabReason(index, selectedIndexTools), [index, selectedIndexTools]);
  const disableHistoryTabReason = useMemo(() => computeDisableHistoryTabReason(index), [index]);

  const defaultActiveEditTab = disableRunTabReason ? EditViewTabsEnum.configuration : EditViewTabsEnum.run;
  const defaultRunTool = isCreateView ? IndexesToolsEnum.indexData : IndexesToolsEnum.searchIndexData;

  const [activeEditTab, setActiveEditTab] = useState<string>(defaultActiveEditTab);
  const [selectedRunTool, setSelectedRunTool] = useState<string>(defaultRunTool);

  const toolSchemaName = activeEditTab === EditViewTabsEnum.configuration || isCreateView ? IndexesToolsEnum.indexData : selectedRunTool;

  const { toolSchema: indexDataSchema, isError: toolSchemaReadFailed, refetch: retryToolSchemaRead } = useSelectedToolSchema({ toolkitType: toDisplayString(values['type']), toolOptionType: toolSchemaName });

  const adjustedIndexDataSchema = useMemo(() => {
    const adjustment =
      view === IndexViewsEnum.edit
        ? { index_name: { hidden: true, default: index.metadata['collection'] }, query: { clipboard: true } }
        : { index_name: indexNameError ? { error: indexNameError } : {}, query: { clipboard: true } };

    return adjustIndexDataSchema(indexDataSchema, adjustment);
  }, [indexNameError, view, index, indexDataSchema]);

  const isValidForm = useMemo(() => {
    if (values['type'] === 'custom') return true;
    if (!adjustedIndexDataSchema?.properties) return false;
    return validateToolkitForm(adjustedIndexDataSchema, toolInputVariables);
  }, [toolInputVariables, adjustedIndexDataSchema, values]);

  const initializeDefaultConfigValues = useCallback(
    (reset = false) => {
      configInitialized.current = true;

      const { defaultValues, hasDefaults } = computeDefaultConfigValues({
        properties: adjustedIndexDataSchema?.properties ?? {},
        toolInputVariables,
        reset,
        useIndexConfigValues: view === IndexViewsEnum.edit && activeEditTab === EditViewTabsEnum.configuration,
        indexConfigValues: index.metadata['index_configuration'] as Record<string, unknown> | undefined,
      });

      if (hasDefaults) {
        setToolInputVariables((prev) => ({ ...(reset ? {} : prev), ...defaultValues }));
      }
    },
    [adjustedIndexDataSchema?.properties, toolInputVariables, view, activeEditTab, index],
  );

  const {
    chatHistory,
    isIndexing,
    isRunning,
    isStoppingIndexing,
    isFullScreenChat,
    handleClearChat,
    handleIndexData,
    handleRunTool,
    llmSettings,
    modelList,
    onCancelIndexing,
    onSelectModel,
    onSetLLMSettings,
    selectedModel,
    toggleFullScreenChat,
    stopRunOnIndexChange,
    handleClearActiveConversation,
  } = useToolkitChat({
    cancelIndexingCallback: (value) => {
      setActiveEditTab(value);
      traceNewIndex(index.id === 'new_index' ? null : index.id, { state: IndexStatuses.cancelled });
    },
    index,
    isValidForm,
    refetchIndexesList,
    runTool: activeEditTab === EditViewTabsEnum.configuration ? null : selectedRunTool,
    toolkitId,
    toolInputVariables,
    traceNewIndex,
    values,
    modes: isCreateView ? [TOOLKIT_CHAT_MODE_CREATE_INDEX] : [],
    onMcpAuthRequired,
  });

  useEffect(() => {
    clearIndexNameError();
    setSelectedRunTool(defaultRunTool);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [defaultRunTool]);

  useEffect(() => {
    if (isRunning) stopRunOnIndexChange();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [index.id]);

  useIndexDetailsTabSync({
    indexId: index.id,
    indexState: index.metadata['state'],
    view,
    selectedRunTool,
    activeEditTab,
    defaultActiveEditTab,
    disableRunTabReason,
    setActiveEditTab,
    handleClearActiveConversation,
    handleClearChat,
    initializeDefaultConfigValues,
  });

  const discardConfigChanges = useCallback(() => initializeDefaultConfigValues(true), [initializeDefaultConfigValues]);

  const onChangeInputVariables = useCallback(
    (value: Record<string, unknown>) => {
      const nextIndexName = value['index_name'];
      if (typeof nextIndexName === 'string' && !isIndexNameValid(nextIndexName)) updateIndexNameError(nextIndexName);
      else clearIndexNameError();

      setToolInputVariables(value);
    },
    [clearIndexNameError, isIndexNameValid, updateIndexNameError],
  );

  const toolConfigProps: IndexConfigToolsConfig = useMemo(
    () => ({
      selectedRunTool,
      onChangeTool: (tool) => setSelectedRunTool(tool.value ?? ''),
      handleRunTool,
      selectedIndexTools,
    }),
    [selectedRunTool, selectedIndexTools, handleRunTool],
  );

  const actionsProps: Omit<IndexActionsProps, 'useToolkitSchemas'> = {
    activeView: activeEditTab,
    index,
    view,
    toolkitId,
    onDiscard: discardConfigChanges,
    isValidForm,
    indexData: handleIndexData,
    isIndexingData: isIndexing,
    isRunningTool: isRunning,
    isIndexDeleting,
    handleDeleteIndex,
    selectedIndexTools,
    onCancelIndexing,
    isStoppingIndexing,
    editToolDetail,
    currentUserId,
    userPermissions,
    currentProjectName,
    isPrivateProject,
    renderCredentialsSelect,
  };

  return (
    <Box sx={wrapperSx}>
      <Box sx={detailsHeaderSx}>
        <IndexNameWrapper index={index} />
        <IndexActions
          {...actionsProps}
          useToolkitSchemas={useToolkitSchemas}
        />
      </Box>

      <Box sx={mainContentSx}>
        <Box sx={computeIndexConfigWrapperSx(isFullScreenChat)}>
          {/* #440: a lost schema read is its own state — without it the form below renders no fields, which is what a tool with no arguments looks like. */}
          {toolSchemaReadFailed && <ToolListError onRetry={retryToolSchemaRead} testId="index-tool-schema-error" message={t('features.toolkits.indexDetails.toolSchemaError', 'The tool settings did not load. Try again.')} />}
          {isCreateView ? (
            <IndexViews
              activeView={IndexViewsEnum.create}
              schema={adjustedIndexDataSchema}
              configInitialized={configInitialized}
              initializeDefaultConfigValues={initializeDefaultConfigValues}
              toolInputVariables={toolInputVariables}
              onChangeInputVariables={onChangeInputVariables}
              isValidForm={isValidForm}
              isRunningTool={isRunning}
              index={index}
              ToolFormField={ToolFormField}
            />
          ) : (
            <>
              <IndexViewToggler
                activeTab={activeEditTab}
                onChangeTab={(_event, value) => {
                  if (value) setActiveEditTab(value);
                }}
                disableRunTabReason={disableRunTabReason}
                disableHistoryTabReason={disableHistoryTabReason}
              />
              <IndexViews
                activeView={activeEditTab}
                schema={adjustedIndexDataSchema}
                toolsConfig={toolConfigProps}
                configInitialized={configInitialized}
                initializeDefaultConfigValues={initializeDefaultConfigValues}
                toolInputVariables={toolInputVariables}
                onChangeInputVariables={onChangeInputVariables}
                // Sibling of the `isCreateView` branch above (both omitted, this button was permanently disabled for every EXISTING index).
                isValidForm={isValidForm}
                isRunningTool={isRunning}
                index={index}
                ToolFormField={ToolFormField}
              />
            </>
          )}
        </Box>
        <IndexChat
          selectedModel={selectedModel}
          onSelectModel={onSelectModel}
          modelList={modelList}
          llmSettings={llmSettings}
          onSetLLMSettings={onSetLLMSettings}
          isFullScreenChat={isFullScreenChat}
          toggleFullScreenChat={toggleFullScreenChat}
          clearChat={handleClearChat}
          chatHistory={chatHistory}
          // `getMockToolkitIndexConversation` only ever wraps LIVE chat
          // rows (the baseline's own call site — never persisted/history
          // rows, which take the separate `historyConversation` path
          // inside `IndexChat.tsx`). `chatHistory`'s prop type is widened
          // to `ChatDisplayMessage` only because `ChatMessageListProps`
          // must also accept the persisted shape; at this specific call
          // site it is always what `useToolkitChat` actually produces.
          conversation={getMockToolkitIndexConversation(chatHistory as readonly IndexChatMessage[])}
          LLMModelSelector={LLMModelSelector}
          ChatMessageList={ChatMessageList}
          ClearChatButton={ClearChatButton}
        />
      </Box>

      {mcpAuthModal}
    </Box>
  );
}
