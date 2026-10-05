import type { ComponentType, ReactNode } from 'react';
import { useCallback, useRef } from 'react';

import Box from '@mui/material/Box';
import FullscreenExitOutlinedIcon from '@mui/icons-material/FullscreenExitOutlined';
import FullscreenOutlinedIcon from '@mui/icons-material/FullscreenOutlined';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import type { Message } from '@/entities/message';
import { t } from '@/shared/i18n';

import { useIndexHistory } from '../../lib/hooks/useIndexHistory.hooks';
import type { IndexChatMessage } from '../../lib/helpers/indexChat.helpers';

/**
 * The two message shapes `ChatMessageList` can be fed: LIVE chat rows
 * (`IndexChatMessage`, built by `indexChat.helpers.ts`'s
 * `generateChatMessageBasedOnResponse`/`generateWelcomeMessage` from raw
 * socket events — this is what `useToolkitChat`'s `chatHistory` actually
 * is) and PERSISTED history rows (`entities/message`'s `Message`, read
 * back from a stored conversation via `useIndexHistory`'s
 * `historyMessages`). The baseline hands both to the same untyped JS
 * component interchangeably; this union is the typed equivalent.
 */
export type ChatDisplayMessage = IndexChatMessage | Message;

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexDetails/IndexChat.jsx` (unit A4a) — the right-hand test-chat panel.
 *
 * DISCLOSED DI: the baseline renders `LLMModelSelector` (`widgets/
 * llm-model-selector`) and `ChatButton.ClearChatButton`/`ChatMessageList`
 * (`features/chat`). Neither exists anywhere in this app yet — grepped:
 * no `widgets/llm-model-selector` or `features/chat` directory in this
 * worktree. Both are large, generic, cross-domain UI (a full LLM settings
 * picker; a markdown/tool-action/streaming message renderer used
 * identically by agents/pipelines/toolkits chat surfaces alike) — building
 * competing local copies would duplicate a FUTURE Wave-2 unit's actual
 * job, not "a thin piece", and risks drifting once the real ones land.
 * Injected as component props instead (same DI treaty as `IndexConfig.tsx`'s
 * `ToolFormField` and `IndexActions.tsx`'s `useToolkitSchemas`) — this file
 * (and its test) stay real and green today, and the real components drop
 * in with zero call-site changes once their unit lands.
 *
 * `FullScreenToggle`/`ContentContainer`/`ChatBodyContainer` (baseline:
 * `components/Chat/FullScreenToggle.jsx`, `pages/Common/Components/
 * StyledComponents.jsx`, `components/Chat/StyledComponents.jsx`) are small,
 * self-contained layout primitives (an MUI `IconButton` pair; two
 * `styled(Box)` wrappers) with no cross-domain complexity — ported locally
 * below rather than injected, including the real CSS those two
 * `styled(Box)` wrappers carry (border, radius, background, scroll
 * behavior), not just their layout-only subset.
 */

export interface LLMModelSelectorProps {
  readonly selectedModel: unknown;
  readonly onSelectModel: (model: unknown) => void;
  readonly models: readonly unknown[];
  readonly llmSettings: Record<string, unknown> | undefined;
  readonly onSetLLMSettings: (settings: Record<string, unknown>) => void;
}

export interface ChatMessageListProps {
  readonly chat_history: readonly ChatDisplayMessage[];
  readonly activeConversation: unknown;
  readonly isLoading: boolean;
  readonly isStreaming: boolean;
  readonly isLoadingMore: boolean;
  readonly interaction_uuid: string;
  readonly onCopyToClipboard: (id: string) => void;
}

export interface IndexChatProps {
  readonly selectedModel: unknown;
  readonly onSelectModel: (model: unknown) => void;
  readonly modelList: readonly unknown[];
  readonly llmSettings: Record<string, unknown> | undefined;
  readonly onSetLLMSettings: (settings: Record<string, unknown>) => void;
  readonly isFullScreenChat: boolean;
  readonly toggleFullScreenChat: (value: boolean) => void;
  readonly clearChat: () => void;
  readonly chatHistory: readonly ChatDisplayMessage[];
  readonly conversation: unknown;
  readonly LLMModelSelector: ComponentType<LLMModelSelectorProps>;
  readonly ChatMessageList: ComponentType<ChatMessageListProps>;
  readonly ClearChatButton: ComponentType<{ onClear: () => void }>;
}

function FullScreenToggle(props: { isFullScreenChat: boolean; setIsFullScreenChat: (value: boolean) => void }): ReactNode {
  const { isFullScreenChat, setIsFullScreenChat } = props;
  return isFullScreenChat ? (
    <Tooltip title={t('features.toolkits.indexChat.exitFullscreen', 'Exit fullscreen mode')}>
      <IconButton onClick={() => setIsFullScreenChat(false)}>
        <FullscreenExitOutlinedIcon sx={{ width: '1rem', height: '1rem' }} />
      </IconButton>
    </Tooltip>
  ) : (
    <Tooltip title={t('features.toolkits.indexChat.fullscreen', 'Fullscreen mode')}>
      <IconButton onClick={() => setIsFullScreenChat(true)}>
        <FullscreenOutlinedIcon sx={{ width: '1rem', height: '1rem' }} />
      </IconButton>
    </Tooltip>
  );
}

// Port of `pages/Common/Components/StyledComponents.jsx`'s `ContentContainer`
// (`boxSizing: border-box` + the lg+ hidden-scrollbar `overflowY: scroll`)
// plus the `height="100%"` MUI system prop the baseline passed at its own
// `<ContentContainer height="100%">` call site.
const contentContainerSx: SxProps<Theme> = (theme) => ({
  height: '100%',
  display: 'flex',
  flexDirection: 'column',
  boxSizing: 'border-box',
  [theme.breakpoints.up('lg')]: {
    overflowY: 'scroll',
    msOverflowStyle: 'none',
    scrollbarWidth: 'none',
    '::-webkit-scrollbar': { display: 'none' },
  },
});

// Port of `components/Chat/StyledComponents.jsx`'s `ChatBodyContainer` — the
// bordered/background-colored "card" the real app renders around the
// message list. `flex`/`minHeight`/`overflowY` diverge intentionally from
// the baseline's `flex: '1 0 0'` + `overflow: hidden` (there, the injected
// `ChatMessageList` owns its own internal scroll region; here it is DI'd —
// see this file's own doc comment — so this wrapper scrolls itself instead
// of assuming the injected component will).
const chatBodyContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  alignSelf: 'stretch',
  position: 'relative',
  borderRadius: (theme) => theme.vars.shape.radiusLg,
  border: (theme) => `1px solid ${theme.vars.palette.border.lines}`,
  background: (theme) => theme.vars.palette.background.eliteaDefault,
  boxSizing: 'border-box',
  flex: '1 1 auto',
  minHeight: 0,
  overflowY: 'auto',
};

function getMessageItemContent(item: unknown): string {
  if (!item || typeof item !== 'object') return '';
  // Matches the baseline's `item.content || item.item_details?.content ||
  // ''` (`IndexChat.jsx:44`): the real wire shape (`entities/message`'s
  // `MessageItemWire`) never puts `content` directly on a message item —
  // only inside `item_details.content` — so falling through to
  // `item_details?.content` here is REQUIRED, not a defensive extra, for
  // every History-tab message that carries `messageItems`.
  const typed = item as { content?: unknown; item_details?: { content?: unknown } };
  if (typeof typed.content === 'string' && typed.content) return typed.content;
  const detailsContent = typed.item_details?.content;
  return typeof detailsContent === 'string' ? detailsContent : '';
}

function getMessageContentForCopy(message: ChatDisplayMessage | undefined): string {
  if (!message) return '';
  if ('exception' in message && message.exception) return JSON.stringify(message.exception);
  if ('messageItems' in message && message.messageItems?.length) {
    return message.messageItems.map(getMessageItemContent).filter(Boolean).join('\n');
  }
  return message.content ?? '';
}

export function IndexChat(props: IndexChatProps): ReactNode {
  const {
    selectedModel,
    onSelectModel,
    modelList,
    llmSettings,
    onSetLLMSettings,
    isFullScreenChat,
    toggleFullScreenChat,
    clearChat,
    chatHistory,
    conversation,
    LLMModelSelector,
    ChatMessageList,
    ClearChatButton,
  } = props;

  const questionItemRef = useRef<HTMLDivElement>(null);

  const { isHistoryMode, historyMessages, historyConversation, isHistoryLoading } = useIndexHistory();

  const onCopyToClipboard = useCallback(
    (id: string) => {
      const messages = isHistoryMode ? historyMessages : chatHistory;
      const message = messages.find((item) => item.id === id);
      void navigator.clipboard.writeText(getMessageContentForCopy(message)).catch(() => undefined);
    },
    [chatHistory, historyMessages, isHistoryMode],
  );

  return (
    <Box sx={{ flex: '1 1 auto', minWidth: 0, overflow: 'hidden' }}>
      <Box sx={contentContainerSx}>
        <Box sx={{ display: 'flex', flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', width: '100%', minWidth: 0, gap: '0.5rem', marginBottom: '.875rem' }}>
          <Box sx={{ minWidth: 0, flex: '1 1 0' }}>
            {isHistoryMode ? (
              <Box />
            ) : (
              <LLMModelSelector
                selectedModel={selectedModel}
                onSelectModel={onSelectModel}
                models={modelList}
                llmSettings={llmSettings}
                onSetLLMSettings={onSetLLMSettings}
              />
            )}
          </Box>
          <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', gap: '0.5rem', flexShrink: 0 }}>
            <FullScreenToggle
              isFullScreenChat={isFullScreenChat}
              setIsFullScreenChat={toggleFullScreenChat}
            />
            {!isHistoryMode && <ClearChatButton onClear={clearChat} />}
          </Box>
        </Box>

        <Box sx={chatBodyContainerSx}>
          <ChatMessageList
            chat_history={isHistoryMode ? historyMessages : chatHistory}
            activeConversation={isHistoryMode ? historyConversation : conversation}
            isLoading={isHistoryMode ? isHistoryLoading : false}
            isStreaming={false}
            isLoadingMore={false}
            interaction_uuid="toolkit-test"
            onCopyToClipboard={onCopyToClipboard}
          />
        </Box>
      </Box>
      <div ref={questionItemRef} />
    </Box>
  );
}
