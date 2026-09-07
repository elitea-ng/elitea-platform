/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/
 * useToolkitChat.hooks.js` (426 lines, Wave-2 unit A4b) — the toolkit
 * test/index chat panel's state machine: run/cancel a tool, stream the
 * response into `chatHistory`, and manage the model/LLM-settings picker.
 * Response wiring (the SSE execution stream + the `chat_predict` socket
 * fallback and its room lifecycle) is split into
 * `./useToolkitChatSocket.hooks.ts` — see that file's own doc comment; this
 * file covers everything else.
 *
 * TRANSPORT (issue #93): run dispatch lives in
 * `./useToolkitChatDispatch.hooks.ts` — REST start first, socket.io
 * `chat_predict` emit as the fallback.
 *
 * FIVE real, disclosed gaps (each verified by grep, not assumed — see
 * `./useToolkitChat.types.ts`'s per-field doc comments for the individual
 * citation): `isAuthCheckSession`, `modelList`/`defaultModel`,
 * `createConversation`, `addParticipant`, `stopIndexing`, and
 * `buildMessagePayload` are INJECTED PARAMETERS instead of internal calls,
 * because no real generated-endpoint/context/cross-slice equivalent exists
 * for any of them in this worktree:
 *  - `src/shared/api/generated/chat/chat.ts` (the ONLY generated "chat"
 *    endpoint file) exports exactly `useWebchatSync`/`useGetChatConfig` —
 *    no conversation-create, add-participant, or indexing-stop endpoint
 *    exists anywhere under `shared/api/generated/**`.
 *  - No `ListModels` endpoint exists (mission brief's own disclosed gap,
 *    independently reconfirmed here).
 *  - `useToolkitSocketContext` (`[fsd]/app/providers`) has no new-app
 *    home: `app/` is composition-root-only (spec §3.2), and no sibling A4
 *    context provider is confirmed landed in this worktree.
 *  - `generateMessagePayload` (`common/messagePayloadUtils.js`) is
 *    chat-domain: it pulls in `features/mcp`'s
 *    `McpAuthHelpers.getAllTokens()` (sideways-forbidden even once that
 *    slice lands) and chat-LLM-settings filtering outside this slice's
 *    ownership.
 *
 * `useIndexHistory`/`convertConversationToChatHistory` are NOT injected —
 * the toolkits/indexes sub-unit (A4a) landed in this shared worktree
 * mid-session; `../../indexes/lib/hooks/useIndexHistory.hooks.ts` now
 * exists and is intra-slice (same `features/toolkits/` slice, free
 * regardless of landing order — R-L3 only restricts crossing INTO a
 * DIFFERENT slice), so it is imported directly below rather than kept as a
 * speculative injection point.
 *
 * Everything else — the actual state machine (`chatHistory`/`isRunning`/
 * `selectedModel`/`llmSettings`, `run`/`executeRunTool`/
 * `createToolkitConversation`/`onCancelIndexing`) — is a faithful, fully-
 * wired port using this app's REAL infrastructure:
 *  - `crypto.randomUUID()` replaces the baseline's `uuid` package
 *    (`v4 as uuidv4`) — `uuid` is not a `package.json` dependency in this
 *    app (grepped; only present transitively in `package-lock.json`), and
 *    every OTHER call site that needs a client-generated id already uses
 *    `crypto.randomUUID()` instead (`features/mcps/model/useMcpAuthCheck.ts`,
 *    `features/pipelines/model/useAIContentGenerationStreaming.ts`,
 *    `shared/api/auth/popup.ts`) — same substitution, not a one-off choice.
 *  - `createToolkitConversationWithParticipant`/`findToolkitParticipant`/
 *    `DEFAULT_LLM_SETTINGS` (`../helpers/toolkitConversation.helpers.ts`)
 *    are this sub-unit's own already-ported files, reused intra-slice.
 *  - `IndexesToolsEnum`/`EditViewTabsEnum`/`IndexStatuses`
 *    (`../../indexes/lib/constants/indexDetails.constants`),
 *    `generateMockMessageTemplate`/`generateWelcomeMessage`
 *    (`../../indexes/lib/helpers/indexChat.helpers`), and `useIndexHistory`
 *    (`../../indexes/lib/hooks/useIndexHistory.hooks`) are intra-slice
 *    siblings owned by the toolkits/indexes sub-unit (A4a).
 *
 * `generateLLMSettings(model)` (`[fsd]/shared/lib/utils/llmSettings.utils.js`)
 * — model-specific LLM-settings defaulting — has no port of its OWN FILE
 * anywhere in this app (independently reconfirmed by two OTHER
 * already-landed files: `entities/application-form/model/initialValues.ts`
 * and `features/agents/ui/generate-agent-modal/useAgentDraftApproval.ts`,
 * both citing the exact same gap for their own callers). The function
 * itself, however, IS re-implemented locally — `../helpers/
 * toolkitConversation.helpers.ts`'s `generateLlmSettings` — and used here on
 * both the initial `llmSettings` state and every `onSelectModel` call,
 * matching the baseline's own `useState(() => generateLLMSettings(defaultModel))`
 * / `onSelectModel`'s `setLLmSettings(generateLLMSettings(model))` exactly
 * (real fix, not `DEFAULT_LLM_SETTINGS`'s previous static fallback on every
 * selection — see that helper file's own module doc comment for the full
 * `top_k`/`reasoning_effort` parity rationale).
 *
 * `ToolkitsHelpers.prettifyToolkitConversation` is deliberately NOT applied
 * to the recovered history (`historyMessages`, below) — the SAME decision
 * `useIndexHistory.hooks.ts`'s own doc comment already makes for the
 * identical reason: it operates on the baseline's snake_case
 * `message_items` field, and the normalized `Message` shape renamed that to
 * camelCase `messageItems`; calling it here would risk the same silent
 * no-op/throw that file already declined to risk.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import type { TestToolkitToolOutcome } from '../../api/toolkitTestRun';
import { EditViewTabsEnum, IndexesToolsEnum, IndexStatuses } from '../../indexes/lib/constants/indexDetails.constants';
import { generateWelcomeMessage } from '../../indexes/lib/helpers/indexChat.helpers';
import { canStartToolkitRun } from '../../indexes/lib/helpers/indexExecution.helpers';
import { useIndexHistory } from '../../indexes/lib/hooks/useIndexHistory.hooks';
import { ToolkitChatModesEnum } from '../constants/toolkitChat.constants';
import { buildTestToolChatMessage, describeTestToolRefusal } from '../helpers/testToolOutcome.helpers';
import type { CreatedConversation } from '../helpers/toolkitConversation.helpers';
import { createToolkitConversationWithParticipant, generateLlmSettings } from '../helpers/toolkitConversation.helpers';
import { buildRunToolErrorMessage } from '../helpers/toolRunError.helpers';
import { useSelectedProjectId } from './useSelectedProjectId';
import { useToolkitRunDispatch } from './useToolkitChatDispatch.hooks';
import { useToolkitChatSocket } from './useToolkitChatSocket.hooks';
import type { ToolkitChatIndexLike, ToolkitChatLlmSettings, ToolkitChatMessage, ToolkitChatModel, UseToolkitChatParams, UseToolkitChatResult } from './useToolkitChat.types';

/** `run()`'s "which input variables apply" resolution, split out of the hook body to stay under the §3.5 complexity budget. */
function resolveRunInputVariables(
  toolInputVariables: Readonly<Record<string, unknown>> | undefined,
  index: ToolkitChatIndexLike | undefined,
  isCreateIndexMode: boolean,
  indexing: boolean,
): Readonly<Record<string, unknown>> {
  if (!isCreateIndexMode && indexing && index) {
    return index.metadata.index_configuration ?? {};
  }
  return toolInputVariables ?? {};
}

/** Whether an in-progress index's conversation should be recovered on mount — split out of the hook body to stay under the §3.5 complexity budget. */
function computeShouldRecoverHistory(isCreateIndexMode: boolean, isIndexing: boolean, index: ToolkitChatIndexLike | undefined): boolean {
  return !isCreateIndexMode && isIndexing && Boolean(index?.metadata.conversation_id);
}

export function useToolkitChat(params: UseToolkitChatParams): UseToolkitChatResult {
  const {
    toolkitId,
    runTool,
    isValidForm,
    toolInputVariables,
    index,
    traceNewIndex,
    refetchIndexesList,
    cancelIndexingCallback,
    values,
    modes,
    onMcpAuthRequired,
    isAuthCheckSession = false,
    modelList,
    defaultModel,
    createConversation,
    addParticipant,
    stopIndexing,
    buildMessagePayload,
    onSuccess,
    onError,
  } = params;

  const runningToolRef = useRef<string | null>(null);
  const projectId = useSelectedProjectId();

  const isTestToolsMode = useMemo(() => modes.includes(ToolkitChatModesEnum.testTools), [modes]);
  const isCreateIndexMode = useMemo(() => modes.includes(ToolkitChatModesEnum.createIndex), [modes]);

  const [selectedModel, setSelectedModel] = useState<ToolkitChatModel | null>(defaultModel);
  const [llmSettings, setLLmSettings] = useState<ToolkitChatLlmSettings>(() => generateLlmSettings(defaultModel));

  const [chatHistory, setChatHistory] = useState<ToolkitChatMessage[]>([generateWelcomeMessage(runTool, isTestToolsMode)]);
  const [isFullScreenChat, toggleFullScreenChat] = useState(false);
  const [activeConversation, setActiveConversation] = useState<CreatedConversation | null>(null);

  const [isRunning, setIsRunning] = useState(false);
  const [isStoppingIndexing, setIsStoppingIndexing] = useState(false);
  /** The REST `task_id` whose event stream `./useToolkitChatSocket.hooks.ts` follows (issue #93). `undefined` = socket fallback, and also the post-settle state, so the stream is closed instead of left polling. */
  const [executionId, setExecutionId] = useState<string | undefined>(undefined);
  const isIndexing = useMemo(() => index?.metadata.state === IndexStatuses.progress, [index]);

  const recoveryConversationId = index?.metadata.conversation_id;
  const { needGenerateProgressingIndexHistory, setProgressingIndexHistoryRecovered, historyMessages } = useIndexHistory({
    shouldRecover: computeShouldRecoverHistory(isCreateIndexMode, isIndexing, index),
    // `exactOptionalPropertyTypes` requires the conditional-spread pattern —
    // an absent `conversation_id` must produce an ABSENT `conversationId`
    // key, never a present key holding `undefined`.
    ...(recoveryConversationId !== undefined ? { conversationId: recoveryConversationId } : {}),
  });

  useEffect(() => {
    if (!needGenerateProgressingIndexHistory) return;
    setChatHistory([...historyMessages]);
    setProgressingIndexHistoryRecovered(true);
    setIsRunning(true);
  }, [needGenerateProgressingIndexHistory, historyMessages, setProgressingIndexHistoryRecovered]);

  const onSetLLMSettings = useCallback((settings: Partial<ToolkitChatLlmSettings>) => {
    setLLmSettings((prev) => ({ ...prev, ...settings }));
  }, []);

  const onSelectModel = useCallback((model: ToolkitChatModel) => {
    setSelectedModel(model);
    setLLmSettings(generateLlmSettings(model));
  }, []);

  const onRunFinish = useCallback(
    (state: string) => {
      // Close the execution stream first: whichever transport reported the
      // finish, there is nothing left to receive on it.
      setExecutionId(undefined);
      if (isTestToolsMode) {
        setIsRunning(false);
        return;
      }
      setTimeout(() => {
        if (runningToolRef.current && runningToolRef.current !== IndexesToolsEnum.indexData) return;
        traceNewIndex?.(index?.id ?? null, { state });
        refetchIndexesList();
      }, 500);
      setIsRunning(false);
    },
    [refetchIndexesList, isTestToolsMode, index?.id, traceNewIndex],
  );

  const onStartTask = useCallback(
    (taskId: string | undefined) => {
      if (isTestToolsMode) return;
      traceNewIndex?.(index?.id ?? null, { task_id: taskId });
    },
    [index?.id, isTestToolsMode, traceNewIndex],
  );
  /** The dispatch hook's "fifth case" (REST test-tool, every tool but `index_data`) reports here: a settled run becomes a transcript message, a refusal becomes a Snackbar — see `../helpers/testToolOutcome.helpers.ts`. */
  const onTestToolOutcome = useCallback(
    (outcome: TestToolkitToolOutcome) => {
      const tool = runningToolRef.current ?? runTool;
      const message = buildTestToolChatMessage(tool, outcome);
      if (message) {
        setChatHistory((prev) => [...prev, message]);
      } else {
        onError?.(describeTestToolRefusal(tool, outcome) ?? '');
      }
      onRunFinish(outcome.kind === 'ok' ? 'finished' : outcome.kind);
    },
    [onError, onRunFinish, runTool],
  );

  /** SSE-first run dispatch with the socket.io fallback, and the no-transport report — see `./useToolkitChatDispatch.hooks.ts`. */
  const { startToolRun, runSocketFallback } = useToolkitRunDispatch({
    projectId,
    toolkitId,
    selectedModel,
    llmSettings,
    buildMessagePayload,
    onStartTask,
    setExecutionId,
    onError,
    onTestToolOutcome,
  });

  useToolkitChatSocket({
    isAuthCheckSession,
    onMcpAuthRequired,
    onRunFinish,
    onStartTask,
    setChatHistory,
    activeConversationId: activeConversation?.id,
    activeConversationUuid: activeConversation?.uuid,
    projectId,
    roomEnabled: isIndexing || isRunning,
    executionId,
    onStreamError: runSocketFallback,
  });

  useEffect(() => {
    if (defaultModel && selectedModel === null) setSelectedModel(defaultModel);
  }, [defaultModel, selectedModel]);

  const createToolkitConversation = useCallback(
    async (input: { readonly indexName: string | undefined; readonly configuration: Readonly<Record<string, unknown>>; readonly tool: string }) => {
      try {
        const conversation = await createToolkitConversationWithParticipant({
          createConversation,
          addParticipant,
          toolkitId,
          projectId,
          values,
          llmSettings,
          selectedModel,
          meta: {
            ...(input.indexName ? { index_name: input.indexName } : {}),
            configuration: input.configuration,
            operation_type: input.tool,
          },
        });

        if (conversation) setActiveConversation(conversation);
        return conversation;
      } catch {
        setIsRunning(false);
        return null;
      }
    },
    [createConversation, addParticipant, toolkitId, projectId, values, llmSettings, selectedModel],
  );

  /** The "no active conversation yet, or a fresh non-test-tools run" branch of `executeRunTool`, split out to stay under the §3.5 complexity budget. */
  const startNewToolkitConversation = useCallback(
    async (relevantInputVariables: Readonly<Record<string, unknown>>, tool: string) => {
      setProgressingIndexHistoryRecovered(true);
      setChatHistory([]);
      const conversation = await createToolkitConversation({
        indexName: relevantInputVariables['index_name'] as string | undefined,
        configuration: relevantInputVariables,
        tool,
      });
      if (conversation) traceNewIndex?.(index?.id ?? null, { conversation_id: conversation.id });
      return conversation;
    },
    [setProgressingIndexHistoryRecovered, createToolkitConversation, traceNewIndex, index?.id],
  );

  const executeRunTool = useCallback(
    async (input: { readonly relevantInputVariables: Readonly<Record<string, unknown>>; readonly indexing: boolean; readonly tool: string }) => {
      const { relevantInputVariables, indexing, tool } = input;
      try {
        const needsNewConversation = !activeConversation || (indexing && !modes.includes(ToolkitChatModesEnum.testTools));
        const currentConversation = needsNewConversation ? await startNewToolkitConversation(relevantInputVariables, tool) : activeConversation;

        await startToolRun(currentConversation, tool, relevantInputVariables);
      } catch (error) {
        setIsRunning(false);
        if (indexing) {
          traceNewIndex?.(index?.id ?? null, { collection: relevantInputVariables['index_name'], state: IndexStatuses.fail });
        }
        setChatHistory((prev) => [...prev, buildRunToolErrorMessage(tool, error)]);
      }
    },
    [activeConversation, modes, startNewToolkitConversation, startToolRun, traceNewIndex, index?.id],
  );

  const run = useCallback(
    (tool: string = IndexesToolsEnum.indexData) => {
      const indexing = tool === IndexesToolsEnum.indexData;
      const canProceed = canStartToolkitRun({ indexing, isCreateIndexMode, isValidForm, isRunning, isIndexing });
      const relevantInputVariables = resolveRunInputVariables(toolInputVariables, index, isCreateIndexMode, indexing);

      if (!canProceed) return;

      setIsRunning(true);
      runningToolRef.current = tool;

      if (indexing) {
        traceNewIndex?.(index?.id ?? null, { collection: relevantInputVariables['index_name'], state: IndexStatuses.progress, created_on: Date.now() / 1000 });
      }

      void executeRunTool({ relevantInputVariables, indexing, tool });
    },
    [isCreateIndexMode, isValidForm, isRunning, isIndexing, toolInputVariables, index, traceNewIndex, executeRunTool],
  );

  const onCancelIndexing = useCallback(async () => {
    if (!index) return;
    setIsStoppingIndexing(true);
    try {
      await stopIndexing({ projectId, toolkitId, indexName: index.metadata.collection, taskId: index.metadata.task_id });
      onSuccess?.('Indexing stopped successfully');
      setIsRunning(false);
      cancelIndexingCallback?.(EditViewTabsEnum.configuration);
    } catch {
      onError?.('Failed to stop indexing');
    } finally {
      setIsStoppingIndexing(false);
    }
  }, [index, projectId, cancelIndexingCallback, stopIndexing, onError, onSuccess, toolkitId]);

  const handleIndexData = useCallback(() => run(), [run]);
  const handleRunTool = useCallback(() => run(runTool), [run, runTool]);

  const handleClearChat = useCallback(() => {
    setChatHistory([generateWelcomeMessage(runTool, isTestToolsMode)]);
    setProgressingIndexHistoryRecovered(false);
  }, [runTool, setProgressingIndexHistoryRecovered, isTestToolsMode]);

  const handleClearActiveConversation = useCallback(() => {
    setActiveConversation(null);
    setProgressingIndexHistoryRecovered(false);
  }, [setProgressingIndexHistoryRecovered]);

  const stopRunOnIndexChange = useCallback(() => {
    setIsRunning(false);
    setProgressingIndexHistoryRecovered(false);
  }, [setProgressingIndexHistoryRecovered]);

  return {
    activeConversation,
    chatHistory,
    isIndexing,
    isFullScreenChat,
    isRunning,
    isStoppingIndexing,
    handleClearActiveConversation,
    handleClearChat,
    handleIndexData,
    handleRunTool,
    llmSettings,
    modelList,
    onCancelIndexing: () => void onCancelIndexing(),
    onSelectModel,
    onSetLLMSettings,
    selectedModel,
    stopRunOnIndexChange,
    toggleFullScreenChat,
  };
}
