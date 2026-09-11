import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import FullscreenExitOutlinedIcon from '@mui/icons-material/FullscreenExitOutlined';
import FullscreenOutlinedIcon from '@mui/icons-material/FullscreenOutlined';
import Box from '@mui/material/Box';
import Grid from '@mui/material/Grid';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { ToolTypes } from '@/entities/toolkit';
import { t } from '@/shared/i18n';
import { ViewRunHistoryButton } from '@/shared/ui/ViewRunHistoryButton';

import { IndexesToolsEnum } from '../../indexes/lib/constants/indexDetails.constants';
import { adjustIndexDataSchema, getMockToolkitIndexConversation } from '../../indexes/lib/helpers/indexChat.helpers';
import type { IndexChatMessage } from '../../indexes/lib/helpers/indexChat.helpers';
import { useIndexNameValidation } from '../../indexes/lib/hooks/useIndexNameValidation.hooks';
import { ToolkitChatModesEnum } from '../../lib/constants/toolkitChat.constants';
import { validateToolkitForm } from '../../lib/helpers/toolkitChat.helpers';
import type { ToolFormSchema } from '../../lib/helpers/toolkitChat.helpers';
import { useToolkitChat } from '../../lib/hooks/useToolkitChat.hooks';
import type { ToolkitChatModel } from '../../lib/hooks/useToolkitChat.types';

import { getMessageContentForCopy, resolveDefaultValue } from './TestTools.helpers';
import type { TestToolsProps } from './TestTools.types';
import type { McpToolOption } from './useGetSelectedToolSchema';
import { useGetSelectedToolSchema } from './useGetSelectedToolSchema';
import { TestToolSettings } from './TestToolSettings';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/toolkits/ui/test-tools/
 * TestTools.jsx` (319 lines, Wave-2 unit A4f) — the "run a tool against a
 * live toolkit" panel: a live chat transcript (left) plus the tool-picker/
 * argument-form/Run-Tool settings panel (right, `./TestToolSettings.tsx`).
 * `TestToolsProps`/`TestToolsChatUI`/`TestToolsChatSession` live in
 * `./TestTools.types.ts`; `getMessageContentForCopy`/`resolveDefaultValue`
 * live in `./TestTools.helpers.ts` — both split out purely for the §3.5
 * 400-line budget (see their own module doc comments).
 *
 * DISCLOSED DEVIATIONS, each forced by a real, verified constraint:
 *
 *  1. **No ambient Formik context** — `useFormikContext().values` becomes
 *     an explicit `values` prop (established convention across this
 *     session's A1/A2/A4 sub-units).
 *
 *  2. **`useMcpAuthModal`/`McpAuthModal` (`features/mcp`/`features/mcps`,
 *     unit A5) cannot be imported** — `no-sideways-features` forbids
 *     `features/toolkits` importing `features/mcps`, no carve-out
 *     (`.dependency-cruiser.cjs`, confirmed by direct reading). The mission
 *     brief names this dependency `useInternalMcpPatStatus`; no such symbol
 *     exists anywhere in the old app or this app (grepped both in full) —
 *     the REAL dependency at this call site is `useMcpAuthModal`/
 *     `McpAuthModal`, confirmed by reading the baseline file directly.
 *     Rather than duplicating that hook's ~250-line OAuth-metadata/
 *     token-storage-key state machine plus its own full-OAuth-form
 *     `McpAuthModal` UI a SECOND time inside a 2-file "kept small for real
 *     cohesion" sub-unit, `onMcpAuthRequired`/`mcpAuthModal` are explicit
 *     props — the SAME "caller-supplied" DI treaty this exact slice's own
 *     `IndexDetails.tsx` (unit A4a) already established for the identical
 *     constraint (byte-for-byte same prop names). `onMcpAuthRequired`
 *     threads straight into `useToolkitChat`'s own same-named param (A4b's
 *     own disclosed injection point); `mcpAuthModal` renders verbatim where
 *     the baseline rendered `<McpAuthModal {...getModalProps()} />`.
 *
 *  3. **`LLMModelSelector`/`ChatMessageList`/`ClearChatButton` are
 *     injected** — `widgets/llm-model-selector` and `features/chat` do not
 *     exist anywhere in this worktree (grepped) and neither is legally
 *     importable from `features/toolkits` even once they do
 *     (upward/sideways-forbidden respectively). Same DI treaty, same prop
 *     names, as this slice's own `IndexChat.tsx` (unit A4a) —
 *     `LLMModelSelectorProps`/`ChatMessageListProps` are reused directly
 *     from that file (intra-slice, R-L3-legal). `askingQuestionId`/
 *     `lastResponseMinHeight`/`questionItemRef` (baseline: always
 *     `""`/`0`/a FRESH `useRef()` created inline every render, i.e. never
 *     actually accumulates a DOM node) are dropped for the same reason
 *     `IndexChat.tsx`'s own `ChatMessageListProps` already omits them.
 *
 *  4. **`useGetSelectedToolSchema`/`useChatCopyToClipboard`/
 *     `FullScreenToggle`/`ContentContainer`/`ChatBodyContainer`** are
 *     small, self-contained pieces — ported locally (the first as
 *     `./useGetSelectedToolSchema.ts`; the rest inlined below as
 *     `chatContainerSx`/`chatBodyContainerSx`, carrying over
 *     `ContentContainer`'s (`apps/elitea-ui/src/pages/Common/Components/
 *     StyledComponents.jsx`) and `ChatBodyContainer`'s (`apps/elitea-ui/src/
 *     components/Chat/StyledComponents.jsx:28-43`) own base CSS — not just
 *     the baseline's `sx` overrides — so the chat panel keeps the real
 *     bordered/rounded/background-tinted "card" chrome and `lg`+ hidden-
 *     scrollbar behavior) rather than injected, matching `IndexChat.tsx`'s
 *     own identical classification of the same layout primitives.
 *
 *  5. **`ToolkitChatHelpers.validateToolkitForm`/`useToolkitChat` are real,
 *     direct intra-slice imports, NOT injected** — unlike `IndexDetails.tsx`
 *     (which injected both because A4b had not yet landed at ITS authoring
 *     time), both are CONFIRMED landed here now — R-L3 intra-slice imports
 *     are free regardless of landing order.
 *
 *  6. **`useToolkitChat`'s own five disclosed missing-endpoint gaps**
 *     (`modelList`/`defaultModel`, `createConversation`, `addParticipant`,
 *     `stopIndexing`, `buildMessagePayload` — see that hook's own module
 *     doc comment) propagate one level up as this component's `chatSession`
 *     prop group.
 *
 *  7. **`runTool: selectedTool` is bridged to `useToolkitChat`'s
 *     `runTool: string` (non-nullable)** via `selectedTool ?? ''`. The
 *     baseline's untyped JS hook tolerated `null`; A4b's typed
 *     `UseToolkitChatParams.runTool` is `string`. `handleRunTool` is never
 *     reachable with an empty `runTool` in practice — the Run Tool button
 *     is already `disabled` whenever `isValidForm` is `false`, which is
 *     unconditionally `false` while `!selectedTool` — a type-level bridge,
 *     not a behaviour change.
 *
 *  8. **`useIndexNameValidation()` takes `serverIndexes` as an explicit,
 *     optional prop** (defaulting to the hook's own `[]`), rather than
 *     always calling it with zero arguments like `IndexDetails.tsx` (A4a)
 *     does — this panel exposes the same "server-side index rows" seam so
 *     a caller with a real `useIndexesListQuery(...).data` can wire it
 *     through for full "index_data" test-run fidelity.
 *
 * Everything else — the tool-schema selection/adjustment memo, the
 * `isValidForm` computation, the default-config-value initializer, the
 * two-column `Grid` layout, and the fullscreen/clear-chat/run-history
 * control row — is a faithful, byte-for-byte port of this specific file's
 * own logic.
 */

function FullScreenToggle(props: { readonly isFullScreenChat: boolean; readonly setIsFullScreenChat: (value: boolean) => void }): ReactNode {
  const { isFullScreenChat, setIsFullScreenChat } = props;
  return isFullScreenChat ? (
    <Tooltip title={t('features.toolkits.testTools.exitFullscreen', 'Exit fullscreen mode')}>
      <IconButton onClick={() => setIsFullScreenChat(false)}>
        <FullscreenExitOutlinedIcon fontSize="small" />
      </IconButton>
    </Tooltip>
  ) : (
    <Tooltip title={t('features.toolkits.testTools.fullscreen', 'Fullscreen mode')}>
      <IconButton onClick={() => setIsFullScreenChat(true)}>
        <FullscreenOutlinedIcon fontSize="small" />
      </IconButton>
    </Tooltip>
  );
}

export function TestTools(props: TestToolsProps): ReactNode {
  const { showAdvancedSettings, isFullScreenChat, setIsFullScreenChat, toolkitId, onShowHistory, values, serverIndexes, chatUI, chatSession, onMcpAuthRequired, mcpAuthModal } = props;
  const { LLMModelSelector, ChatMessageList, ClearChatButton } = chatUI;
  const { modelList, defaultModel, createConversation, addParticipant, stopIndexing, buildMessagePayload, onSuccess, onError } = chatSession;

  const initializedToolRef = useRef<string | null>(null);

  const { clearIndexNameError, updateIndexNameError, isIndexNameValid, indexNameError } = useIndexNameValidation(serverIndexes);

  const [selectedTool, setSelectedTool] = useState<string | null>(null);
  const [toolInputVariables, setToolInputVariables] = useState<Record<string, unknown>>({});

  // #440: `isError`/`refetch` come with the schema now. The static tier is a server read, so a lost read used to draw an argument form with no fields — the screen a tool that takes no arguments draws.
  const { toolSchema, isError: toolSchemaReadFailed, refetch: retryToolSchemaRead } = useGetSelectedToolSchema({ toolkitId, toolkitType: values.type, toolOptionType: selectedTool, availableMcpTools: values.settings?.['available_mcp_tools'] as readonly McpToolOption[] | undefined });

  const selectedToolSchema = useMemo(() => {
    if (selectedTool === IndexesToolsEnum.indexData) {
      return adjustIndexDataSchema(toolSchema, { index_name: indexNameError ? { error: indexNameError } : {} });
    }
    return toolSchema;
  }, [selectedTool, indexNameError, toolSchema]);

  const isValidForm = useMemo(() => {
    if (values.type === ToolTypes.custom.value) return true;
    if (!selectedTool || !selectedToolSchema?.properties) return false;
    // `exactOptionalPropertyTypes`-only mismatch: `JsonSchemaLike.required` is
    // `readonly string[] | undefined` (explicit-undefined-allowed), `ToolFormSchema.required`
    // is optional-absent-or-`readonly string[]` — both `undefined` and "absent" mean the same
    // thing to `validateToolkitForm`'s own `schema.required ?? []`, so this cast is safe.
    return validateToolkitForm(selectedToolSchema as ToolFormSchema, toolInputVariables);
  }, [selectedTool, toolInputVariables, selectedToolSchema, values.type]);

  const { chatHistory, handleRunTool, handleClearChat, isRunning, onSelectModel, onSetLLMSettings, selectedModel, llmSettings } = useToolkitChat({
    runTool: selectedTool ?? '',
    toolInputVariables,
    toolkitId,
    isValidForm,
    index: undefined,
    traceNewIndex: undefined,
    // The baseline never invokes `refetchIndexesList` in test-tools mode
    // (`onRunFinish`'s indexing branch is guarded by `isTestToolsMode`) —
    // a no-op here is faithful, not a stub.
    refetchIndexesList: () => undefined,
    cancelIndexingCallback: undefined,
    values,
    modes: [ToolkitChatModesEnum.testTools],
    onMcpAuthRequired,
    modelList,
    defaultModel,
    createConversation,
    addParticipant,
    stopIndexing,
    buildMessagePayload,
    onSuccess,
    onError,
  });

  const onCopyToClipboard = useCallback(
    (id: string) => {
      const message = chatHistory.find((item) => item.id === id);
      void navigator.clipboard.writeText(getMessageContentForCopy(message)).catch(() => undefined);
    },
    [chatHistory],
  );

  // `LLMModelSelectorProps` (the injected-component contract, shared with
  // `IndexChat.tsx`) types its callbacks over `unknown`/`Record<string,
  // unknown>` — the DI seam has no way to know the real
  // `ToolkitChatModel`/`ToolkitChatLlmSettings` shapes `useToolkitChat`
  // actually uses. This adapter narrows back at the one call site that
  // knows both types; the injected `LLMModelSelector` only ever calls it
  // with a value it received (via `models`) from this same `useToolkitChat`
  // session, so the cast is sound by construction.
  const onSelectModelForSelector = useCallback((model: unknown) => onSelectModel(model as ToolkitChatModel), [onSelectModel]);

  const onChangeInputVariables = useCallback((inputVariables: Readonly<Record<string, unknown>>) => {
    setToolInputVariables(inputVariables);
  }, []);

  const onChangeTool = useCallback((value: string | null) => {
    setSelectedTool(value || null);
    setToolInputVariables({});
  }, []);

  const initializeDefaultConfigValues = useCallback(() => {
    if (!selectedToolSchema?.properties || !selectedTool || initializedToolRef.current === selectedTool) return;
    initializedToolRef.current = selectedTool;

    const defaultValues: Record<string, unknown> = {};
    let hasDefaults = false;

    for (const [key, property] of Object.entries(selectedToolSchema.properties)) {
      const currentValue = toolInputVariables[key];
      if (currentValue !== undefined && currentValue !== '' && typeof currentValue !== 'function') continue;

      const defaultValue = resolveDefaultValue(property);
      if (defaultValue !== undefined) {
        defaultValues[key] = defaultValue;
        hasDefaults = true;
      }
    }

    if (hasDefaults) onChangeInputVariables({ ...toolInputVariables, ...defaultValues });
  }, [onChangeInputVariables, selectedTool, selectedToolSchema?.properties, toolInputVariables]);

  useEffect(() => {
    clearIndexNameError();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- baseline only clears on `selectedTool` change
  }, [selectedTool]);

  useEffect(() => {
    initializeDefaultConfigValues();
  }, [initializeDefaultConfigValues]);

  return (
    <>
      <Grid
        size={{ md: 12, lg: 8 }}
        sx={chatGridSx}
      >
        <Box sx={chatContainerSx}>
          <Box sx={chatContentSx}>
            <Box sx={buildChatControlsSx(showAdvancedSettings)}>
              <Box sx={controlButtonsSx}>
                <FullScreenToggle
                  isFullScreenChat={isFullScreenChat}
                  setIsFullScreenChat={setIsFullScreenChat}
                />
                <ClearChatButton onClear={handleClearChat} />
                {onShowHistory && <ViewRunHistoryButton onShowHistory={onShowHistory} />}
              </Box>
            </Box>

            <Box sx={chatBodyContainerSx}>
              <ChatMessageList
                chat_history={chatHistory}
                activeConversation={getMockToolkitIndexConversation(chatHistory as readonly IndexChatMessage[])}
                isLoading={false}
                isStreaming={false}
                isLoadingMore={false}
                interaction_uuid="toolkit-test"
                onCopyToClipboard={onCopyToClipboard}
              />
            </Box>
          </Box>
        </Box>
      </Grid>
      <Grid
        size={{ md: 12, lg: 4 }}
        container
        sx={settingsGridSx}
      >
        <TestToolSettings
          toolkitId={toolkitId}
          selectedTool={selectedTool}
          onChangeTool={onChangeTool}
          toolInputVariables={toolInputVariables}
          onChangeInputVariables={onChangeInputVariables}
          onRunTool={handleRunTool}
          isRunning={isRunning}
          isValidForm={isValidForm}
          selectedToolSchema={selectedToolSchema}
          values={values}
          llm={{ selectedModel, onSelectModel: onSelectModelForSelector, models: modelList, llmSettings, onSetLLMSettings }}
          LLMModelSelector={LLMModelSelector}
          indexNameValidation={{ clearIndexNameError, updateIndexNameError, isIndexNameValid, indexNameError }}
          toolSchemaRead={{ isError: toolSchemaReadFailed, onRetry: retryToolSchemaRead }}
        />
      </Grid>

      {mcpAuthModal}
    </>
  );
}

const chatGridSx: SxProps<Theme> = ({ breakpoints }) => ({
  height: '100%',
  [breakpoints.down('lg')]: {
    height: '100vh',
    minHeight: '120vh',
    marginBottom: '1.5rem',
  },
});

// Baseline: `<ContentContainer sx={styles.chatContainer}>` (`apps/elitea-ui/
// src/pages/Common/Components/StyledComponents.jsx`) — `ContentContainer`'s
// own base CSS (boxSizing:'border-box' plus, on `lg`+, a scrollable-but-
// scrollbar-hidden `overflowY`) is restored here, not just the `sx` override.
export const chatContainerSx: SxProps<Theme> = ({ breakpoints }) => ({
  height: '100%',
  boxSizing: 'border-box',
  [breakpoints.up('lg')]: {
    overflowY: 'scroll',
    msOverflowStyle: 'none',
    scrollbarWidth: 'none',
    '&::-webkit-scrollbar': { display: 'none' },
  },
});

const chatContentSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  height: '100%',
  maxHeight: '100%',
  gap: '.875rem',
  overflow: 'auto',
};

// Plain object literals (NOT `SxProps<Theme>`-annotated) so spreading them
// below stays `typescript/no-misused-spread`-clean — that rule flags
// spreading anything whose STATIC type includes a function variant, which
// an explicit `SxProps<Theme>` annotation always does (`SxProps` is a union
// that includes `(theme: Theme) => SystemStyleObject<Theme>`).
const chatControlsBase = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  justifyContent: 'flex-end',
  width: '100%',
} as const;

function buildChatControlsSx(showAdvancedSettings: boolean): SxProps<Theme> {
  return ({ breakpoints }: Theme) => ({
    ...chatControlsBase,
    [breakpoints.down('lg')]: { marginBottom: showAdvancedSettings ? '0rem' : '1.5rem' },
  });
}

const controlButtonsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '.75rem' };

// Baseline: `<ChatBodyContainer sx={...}>` (`apps/elitea-ui/src/components/
// Chat/StyledComponents.jsx:28-43`) — the styled component's own base CSS
// (the bordered/rounded/background-tinted "card" chrome) is restored here;
// `breakpoints.up/down('lg')` below are the `sx` overrides layered on top.
export const chatBodyContainerSx: SxProps<Theme> = ({ breakpoints, vars }) => ({
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  flex: '1 0 0',
  flexGrow: 1,
  alignSelf: 'stretch',
  position: 'relative',
  borderRadius: vars.shape.radiusLg,
  border: `.0625rem solid ${vars.palette.border.lines}`,
  background: vars.palette.background.eliteaDefault,
  boxSizing: 'border-box',
  overflow: 'hidden',
  [breakpoints.up('lg')]: { height: 'calc(100vh - 10rem)', overflow: 'hidden' },
  [breakpoints.down('lg')]: { height: '100vh', minHeight: '100vh', marginBottom: '1.5rem', overflow: 'hidden' },
});

const settingsGridSx: SxProps<Theme> = ({ breakpoints }) => ({
  maxHeight: '100%',
  [breakpoints.down('lg')]: { height: '100vh', minHeight: '100vh', paddingBottom: '1.5rem' },
});
