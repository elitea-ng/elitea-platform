import type { MouseEvent, ReactNode, Ref, RefObject } from 'react';
import { forwardRef, useCallback, useImperativeHandle, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import CloseIcon from '@mui/icons-material/Close';
import Divider from '@mui/material/Divider';
import IconButton from '@mui/material/IconButton';
import KeyboardDoubleArrowLeftIcon from '@mui/icons-material/KeyboardDoubleArrowLeft';
import KeyboardDoubleArrowRightIcon from '@mui/icons-material/KeyboardDoubleArrowRight';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { ViewRunHistoryButton } from '@/shared/ui/ViewRunHistoryButton';

import {
  chatContentSx,
  closeButtonSx,
  closeDividerSx,
  collapseButtonSx,
  controlsContainerSx,
  legacyContainerSx,
  onDemandContainerSx,
  resizeHandleSx,
  topBarSx,
} from './ChatPanel.styles';

import { usePaneFold } from '../lib/hooks/useEditorPaneRoom';
import { useIsSmallWindow } from '../lib/hooks/useIsSmallWindow';
import { useIsPipelineYamlCodeDirty } from '../lib/hooks/useIsPipelineYamlCodeDirty';
import { useTestPanelWidth } from '../lib/hooks/useTestPanelWidth';
import type { ChatConversation } from '../lib/hooks/pipelineChat.types';

/**
 * Ported from `EliteaUI` `features/pipelines/test-panel/ui/TestPanel.jsx` and
 * `widgets/entity-test-panel/ui/EntityTestPanelShell.jsx`.
 *
 * Supports the Wave U3 on-demand resizable TestPanel (`panel` prop: closed by default,
 * stays mounted while hidden so live runs survive toggling, resizable left edge, header
 * Run History / Clear / Close controls) while preserving legacy standalone collapse mode
 * when `panel` is omitted.
 */
export interface ChatPanelSlotProps {
  readonly settings: Readonly<Record<string, unknown>>;
  readonly restore?: { readonly conversationId: string | undefined; readonly onComplete: () => void } | undefined;
  readonly disableChat: boolean;
  readonly ref: Ref<ChatBoxSlotHandle> | undefined;
}

export interface ChatBoxSlotHandle {
  readonly stopAll: () => void;
  readonly onClear: () => void;
}

export interface TestPanelShellOptions {
  readonly isOpen: boolean;
  readonly onClose: () => void;
  readonly workspaceRef?: RefObject<HTMLDivElement | null> | undefined;
  readonly onResized?: (() => void) | undefined;
}

export interface ChatPanelProps {
  readonly restore?: ChatPanelSlotProps['restore'];
  readonly settings: Readonly<Record<string, unknown>> & { readonly isStreaming?: boolean; readonly activeConversation?: ChatConversation | null };
  readonly display?: string | undefined;
  readonly onCollapsed?: ((collapsed: boolean) => void) | undefined;
  /** Starts folded while the canvas lacks room (`useEditorPaneRoom`); ignored when `panel` is given. */
  readonly autoFolded?: boolean | undefined;
  /** On-demand resizable TestPanel shell options (`isOpen`, `onClose`, `workspaceRef`, `onResized`). */
  readonly panel?: TestPanelShellOptions | undefined;
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

const WELCOME_MESSAGE_ID = 'welcome_message_id';

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

function TestPanelResizeHandle({ onResizeStart }: { readonly onResizeStart: (event: MouseEvent) => void }): ReactNode {
  return (
    <Box
      component="hr"
      aria-orientation="vertical"
      aria-label={t('features.pipelines.chatPanel.resize', 'Resize test panel')}
      data-testid="pipeline-test-panel-resize-handle"
      onMouseDown={onResizeStart}
      sx={resizeHandleSx}
    />
  );
}

function TestPanelCloseButton({ onClose }: { readonly onClose: () => void }): ReactNode {
  return (
    <>
      <Divider
        orientation="vertical"
        flexItem
        sx={closeDividerSx}
      />
      <Tooltip
        title={t('features.pipelines.chatPanel.close', 'Close')}
        placement="top"
      >
        <Box component="span">
          <IconButton
            size="small"
            onClick={onClose}
            aria-label={t('features.pipelines.chatPanel.closePanel', 'Close test panel')}
            data-testid="pipeline-test-panel-close"
            sx={closeButtonSx}
          >
            <CloseIcon fontSize="small" />
          </IconButton>
        </Box>
      </Tooltip>
    </>
  );
}

function LegacyCollapseToggle({ collapsed, onToggle }: { readonly collapsed: boolean; readonly onToggle: () => void }): ReactNode {
  return (
    <IconButton
      sx={collapseButtonSx}
      onClick={onToggle}
      aria-label={
        collapsed
          ? t('features.pipelines.chatPanel.expand', 'Expand the chat panel')
          : t('features.pipelines.chatPanel.collapse', 'Collapse the chat panel')
      }
    >
      {collapsed ? <KeyboardDoubleArrowLeftIcon fontSize="small" /> : <KeyboardDoubleArrowRightIcon fontSize="small" />}
    </IconButton>
  );
}

interface ChatPanelHeaderProps {
  readonly isSmallWindow: boolean;
  readonly isVisible: boolean;
  readonly isOnDemand: boolean;
  readonly hasActiveConversationId: boolean;
  readonly contextBudgetNode: ReactNode;
  readonly clearButtonNode: ReactNode;
  readonly onShowHistory: (() => void) | undefined;
  readonly onClose: (() => void) | undefined;
}

function ChatPanelHeader({
  isSmallWindow,
  isVisible,
  isOnDemand,
  hasActiveConversationId,
  contextBudgetNode,
  clearButtonNode,
  onShowHistory,
  onClose,
}: ChatPanelHeaderProps): ReactNode {
  return (
    <Box sx={topBarSx(isSmallWindow, !isVisible, isOnDemand)}>
      {contextBudgetNode}
      <Box
        data-testid="pipeline-test-panel-controls"
        sx={controlsContainerSx(isSmallWindow, hasActiveConversationId)}
      >
        {clearButtonNode}
        {onShowHistory && <ViewRunHistoryButton onShowHistory={onShowHistory} />}
        {onClose && <TestPanelCloseButton onClose={onClose} />}
      </Box>
    </Box>
  );
}

interface ChatPanelEdgeControlProps {
  readonly isSmallWindow: boolean;
  readonly isOnDemand: boolean;
  readonly collapsed: boolean;
  readonly onResizeStart: (event: MouseEvent) => void;
  readonly onToggleCollapsed: () => void;
}

function ChatPanelEdgeControl({
  isSmallWindow,
  isOnDemand,
  collapsed,
  onResizeStart,
  onToggleCollapsed,
}: ChatPanelEdgeControlProps): ReactNode {
  if (isSmallWindow) return null;
  if (isOnDemand) return <TestPanelResizeHandle onResizeStart={onResizeStart} />;
  return <LegacyCollapseToggle collapsed={collapsed} onToggle={onToggleCollapsed} />;
}

function resolvePanelLayout(
  panel: TestPanelShellOptions | undefined,
  isSmallWindow: boolean,
  collapsed: boolean,
  width: number,
  display: string | undefined,
  containerRef: RefObject<HTMLDivElement | null>,
) {
  const isOnDemand = panel !== undefined;
  return {
    isOnDemand,
    isVisible: isOnDemand ? panel.isOpen : !collapsed,
    shouldRenderChat: isOnDemand || !collapsed,
    rootRef: isOnDemand ? containerRef : undefined,
    ariaHidden: isOnDemand ? !panel.isOpen : undefined,
    rootSx: isOnDemand
      ? onDemandContainerSx(isSmallWindow, panel.isOpen, width)
      : legacyContainerSx(isSmallWindow, collapsed, display),
  };
}

export const ChatPanel = forwardRef<ChatPanelHandle, ChatPanelProps>(function ChatPanel(props, ref): ReactNode {
  const {
    restore,
    settings,
    display,
    onCollapsed,
    autoFolded = false,
    panel,
    setActiveConversation,
    hasRunsInProgress,
    onShowHistory,
    renderChat,
    renderClearChatButton,
    renderContextBudget,
  } = props;
  const { isStreaming, activeConversation } = settings;
  const boxRef = useRef<ChatBoxSlotHandle | null>(null);
  const { isSmallWindow } = useIsSmallWindow();
  const { collapsed, toggle: onClickCollapsed } = usePaneFold(autoFolded, isSmallWindow, onCollapsed);
  const { containerRef, width, handleResizeStart } = useTestPanelWidth({
    workspaceRef: panel?.workspaceRef,
    onResized: panel?.onResized,
  });
  const isPipelineDirty = useIsPipelineYamlCodeDirty();

  useImperativeHandle(ref, () => ({ stopRun: () => boxRef.current?.stopAll() }), []);

  const shouldDisableClear = computeShouldDisableClear(activeConversation, isStreaming);

  const onClear = useCallback(() => {
    if (hasRunsInProgress?.()) boxRef.current?.stopAll();
    boxRef.current?.onClear();
  }, [hasRunsInProgress]);

  const disableChat = Boolean(settings['disableChat']) || isPipelineDirty;
  const slotProps = useMemo<ChatPanelSlotProps>(
    () => ({ settings, disableChat, ref: boxRef, restore }),
    [settings, disableChat, restore],
  );
  const contextBudgetSlotProps = buildContextBudgetSlotProps(activeConversation, setActiveConversation);
  const layout = resolvePanelLayout(panel, isSmallWindow, collapsed, width, display, containerRef);

  return (
    <Box
      ref={layout.rootRef}
      aria-hidden={layout.ariaHidden}
      data-testid="pipeline-test-panel"
      sx={layout.rootSx}
    >
      <ChatPanelEdgeControl
        isSmallWindow={isSmallWindow}
        isOnDemand={layout.isOnDemand}
        collapsed={collapsed}
        onResizeStart={handleResizeStart}
        onToggleCollapsed={onClickCollapsed}
      />
      <Box data-testid="pipeline-chat-content" sx={chatContentSx(isSmallWindow)}>
        <ChatPanelHeader
          isSmallWindow={isSmallWindow}
          isVisible={layout.isVisible}
          isOnDemand={layout.isOnDemand}
          hasActiveConversationId={activeConversation?.id !== undefined}
          contextBudgetNode={contextBudgetSlotProps ? renderContextBudget?.(contextBudgetSlotProps) : null}
          clearButtonNode={renderClearChatButton?.({ disabled: shouldDisableClear, onClear })}
          onShowHistory={onShowHistory}
          onClose={panel?.onClose}
        />
        {layout.shouldRenderChat && renderChat(slotProps)}
      </Box>
    </Box>
  );
});
