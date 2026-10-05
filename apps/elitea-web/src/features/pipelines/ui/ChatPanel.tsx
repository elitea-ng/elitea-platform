import type { ReactNode, Ref } from 'react';
import { forwardRef, useCallback, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { ViewRunHistoryButton } from '@/shared/ui/ViewRunHistoryButton';

import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import { useIsPipelineYamlCodeDirty } from '../lib/hooks/useIsPipelineYamlCodeDirty';
import type { ChatConversation } from '../lib/hooks/pipelineChat.types';

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/Components/ChatPanel.jsx`.
 *
 * **DISCLOSED REDESIGN — slot-based test-chat content, matching the exact
 * precedent `features/agents/ui/ConfigurationTab.tsx`'s own `renderTestPane`
 * doc comment establishes for the identical baseline shape:**
 *  - `ChatBox`/`ChatButton.ClearChatButton` (baseline: `@/[fsd]/features/
 *    chat/ui`) — `no-sideways-features` forbids `features/pipelines`
 *    importing a `features/chat` slice (no carve-on), and that slice does
 *    not exist anywhere in this worktree (verified: `ls src/features/chat`
 *    — no such directory). `renderChat`/`renderClearChatButton` are slots.
 *  - `ContextBudgetUI.ContextBudgetInfo` (baseline: `@/[fsd]/widgets/
 *    context-budget`) — `features/` may not import `widgets/` at all
 *    (`no-upward-from-features`, R-L1), regardless of whether that widget
 *    exists. `renderContextBudget` is a slot.
 *  - `ViewRunHistoryButton` (`@/shared/ui/button`) IS wired directly — real,
 *    landed, layer-legal (`shared/ui` is freely importable from `features/`).
 *
 * `isPipelineDirty` (baseline: `useSelector(state => state.pipeline)`'s
 * `yamlCode !== initState.yamlCode` comparison) is recomputed here via this
 * sub-unit's own `useIsPipelineYamlCodeDirty` hook (real, exact port of the
 * same comparison — see that hook's own doc comment) rather than a
 * `useSelector` read.
 */
export interface ChatPanelSlotProps {
  readonly settings: Readonly<Record<string, unknown>>;
  readonly restore?: { readonly conversationId: string | undefined; readonly onComplete: () => void } | undefined;
  readonly disableChat: boolean;
  readonly ref: Ref<ChatBoxSlotHandle> | undefined;
}

/** The imperative surface `ChatPanel`'s own `stopRun`/clear-button wiring needs from whatever real chat component `renderChat` eventually mounts (baseline: `ChatBox`'s own `ref.stopAll`/`ref.onClear`, `ChatPanel.jsx:101-103,161-166`). */
export interface ChatBoxSlotHandle {
  readonly stopAll: () => void;
  readonly onClear: () => void;
}

export interface ChatPanelProps {
  readonly restore?: ChatPanelSlotProps['restore'];
  readonly settings: Readonly<Record<string, unknown>> & { readonly isStreaming?: boolean; readonly activeConversation?: ChatConversation | null };
  readonly display?: string | undefined;
  readonly onCollapsed?: ((collapsed: boolean) => void) | undefined;
  readonly setActiveConversation: (
    update: ChatConversation | null | ((prev: ChatConversation | null) => ChatConversation | null),
  ) => void;
  readonly hasRunsInProgress?: (() => boolean) | undefined;
  readonly onShowHistory?: (() => void) | undefined;
  readonly renderChat: (props: ChatPanelSlotProps) => ReactNode;
  readonly renderClearChatButton?: ((props: { disabled: boolean; onClear: () => void }) => ReactNode) | undefined;
  readonly renderContextBudget?:
    | ((props: {
        conversationId: string | number;
        contextStrategy: Readonly<Record<string, unknown>>;
        setActiveConversation: ChatPanelProps['setActiveConversation'];
        conversationInstructions: unknown;
      }) => ReactNode)
    | undefined;
}

export interface ChatPanelHandle {
  readonly stopRun: () => void;
}

const chatContainerSx = (isSmallWindow: boolean, collapsed: boolean, display: string | undefined): SxProps<Theme> => ({
  minWidth: isSmallWindow ? '100%' : collapsed ? '1.75rem' : '20rem',
  width: isSmallWindow ? '100%' : collapsed ? '1.75rem' : undefined,
  maxWidth: '100%',
  position: 'relative',
  height: '100%',
  display: display ?? 'flex',
  flexDirection: 'row',
  boxSizing: 'border-box',
  gap: '0.75rem',
});
const collapseButtonSx: SxProps<Theme> = { padding: '0', marginLeft: '0', position: 'absolute', top: '0.25rem', left: '0', zIndex: 100 };
const chatContentSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', height: '100%', width: '100%', maxWidth: '100%', flex: 1, gap: '0.75rem', boxSizing: 'border-box' };
const topBarSx = (collapsed: boolean): SxProps<Theme> => ({
  display: !collapsed ? 'flex' : 'none',
  alignItems: 'center',
  justifyContent: 'space-between',
  gap: '0.5rem',
  width: '100%',
  paddingLeft: '2.25rem',
});
const clearButtonContainerSx = (isSmallWindow: boolean, hasActiveConversationId: boolean): SxProps<Theme> => ({
  display: 'flex',
  flex: 1,
  justifyContent: 'flex-end',
  marginRight: !isSmallWindow && hasActiveConversationId ? '0.25rem' : '0',
  gap: '0.5rem',
});

const WELCOME_MESSAGE_ID = 'welcome_message_id';

/** `ChatPanel.jsx:118-122`'s `shouldDisableClear` — extracted to a pure function to keep the component's own cyclomatic complexity under this codebase's gate. */
function computeShouldDisableClear(activeConversation: ChatConversation | null | undefined, isStreaming: boolean | undefined): boolean {
  const history = activeConversation?.chat_history ?? [];
  if (!history.length) return true;
  if (isStreaming) return true;
  return history.length === 1 && history[0]?.id === WELCOME_MESSAGE_ID;
}

interface ContextBudgetSlotProps {
  readonly conversationId: string | number;
  readonly contextStrategy: Readonly<Record<string, unknown>>;
  readonly setActiveConversation: ChatPanelProps['setActiveConversation'];
  readonly conversationInstructions: unknown;
}

/** Builds `renderContextBudget`'s props from `activeConversation`, or `undefined` when there is no conversation id yet — extracted to keep the component's own cyclomatic complexity under this codebase's gate. */
function buildContextBudgetSlotProps(
  activeConversation: ChatConversation | null | undefined,
  setActiveConversation: ChatPanelProps['setActiveConversation'],
): ContextBudgetSlotProps | undefined {
  if (activeConversation?.id === undefined) return undefined;
  return {
    conversationId: activeConversation.id,
    contextStrategy: activeConversation.meta?.context_strategy ?? {},
    setActiveConversation,
    conversationInstructions: activeConversation.instructions,
  };
}

export const ChatPanel = forwardRef<ChatPanelHandle, ChatPanelProps>(function ChatPanel(props, ref): ReactNode {
  const { restore, settings, display, onCollapsed, setActiveConversation, hasRunsInProgress, onShowHistory, renderChat, renderClearChatButton, renderContextBudget } = props;
  const { isStreaming, activeConversation } = settings;
  const boxRef = useRef<ChatBoxSlotHandle | null>(null);
  const [collapsed, setCollapsed] = useState(false);
  const { isSmallWindow } = useIsSmallWindow();
  const isPipelineDirty = useIsPipelineYamlCodeDirty();

  useImperativeHandle(ref, () => ({ stopRun: () => boxRef.current?.stopAll() }), []);

  const onClickCollapsed = useCallback(() => {
    setCollapsed((prev) => {
      onCollapsed?.(!prev);
      return !prev;
    });
  }, [onCollapsed]);

  useEffect(() => {
    if (isSmallWindow) {
      setCollapsed(false);
      onCollapsed?.(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isSmallWindow]);

  const shouldDisableClear = computeShouldDisableClear(activeConversation, isStreaming);

  const onClear = useCallback(() => {
    // Baseline `ChatPanel.jsx`'s `onClear` handler — stop any in-progress flow-editor
    // run before clearing chat history, so a live run isn't left orphaned mid-flight.
    if (hasRunsInProgress?.()) boxRef.current?.stopAll();
    boxRef.current?.onClear();
  }, [hasRunsInProgress]);

  const disableChat = Boolean(settings['disableChat']) || isPipelineDirty;

  const slotProps = useMemo<ChatPanelSlotProps>(
    () => ({ settings, disableChat, ref: boxRef, restore }),
    [settings, disableChat, restore],
  );
  const contextBudgetSlotProps = buildContextBudgetSlotProps(activeConversation, setActiveConversation);

  return (
    <Box sx={chatContainerSx(isSmallWindow, collapsed, display)}>
      {!isSmallWindow && (
        <IconButton
          sx={collapseButtonSx}
          onClick={onClickCollapsed}
          // #135: same critical `button-name` violation as
          // `GeneralFormPanel.tsx`'s own collapse button — see its comment.
          aria-label={
            collapsed
              ? t('features.pipelines.chatPanel.expand', 'Expand the chat panel')
              : t('features.pipelines.chatPanel.collapse', 'Collapse the chat panel')
          }
        >
          {collapsed ? <KeyboardDoubleArrowLeftIcon fontSize="small" /> : <KeyboardDoubleArrowRightIcon fontSize="small" />}
        </IconButton>
      )}
      <Box sx={chatContentSx}>
        <Box sx={topBarSx(collapsed)}>
          {contextBudgetSlotProps && renderContextBudget?.(contextBudgetSlotProps)}
          <Box sx={clearButtonContainerSx(isSmallWindow, activeConversation?.id !== undefined)}>
            {renderClearChatButton?.({ disabled: shouldDisableClear, onClear })}
            {onShowHistory && <ViewRunHistoryButton onShowHistory={onShowHistory} />}
          </Box>
        </Box>
        {!collapsed && renderChat(slotProps)}
      </Box>
    </Box>
  );
});
