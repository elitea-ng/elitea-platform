/**
 * The DeepWiki chat drawer — the composition root for the wiki conversation.
 *
 * WHY A WIDGET. `features/wiki-chat` is headless: it owns the conversation and
 * nothing that renders. This layer is where the pieces meet, and it is the ONLY
 * layer allowed to reach into `features/chat-messages` — `no-sideways-features`
 * exempts that slice FROM the rule, not TO it, so a feature cannot import it and
 * a widget can. (This first pass reuses `shared/ui/Markdown` and needs nothing
 * from that slice yet; the permission is the reason the drawer lives here.)
 *
 * IT OWNS NO CONVERSATION STATE. Everything below is derived from the
 * controller. A second copy here — a `messages` mirror, a cached `isLoading` —
 * is how the drawer and the stream come to disagree about whether a turn is
 * still running.
 */
import { memo, useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useQuery } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import CloseIcon from '@mui/icons-material/Close';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { ResizableDrawer } from '@/shared/ui/ResizableDrawer';
import { isThinkingBlock, useWikiChat } from '@/features/wiki-chat';

import { pollWikiChat, startWikiChat, type WikiChatTarget } from '../api/wikiChatApi';
import { listWikiConversations, loadWikiTranscript } from '../api/wikiHistoryApi';
import {
  createWikiChatStorage,
  createWikiConversationKey,
  forgetLocalWikiMessages,
  readLocalWikiMessages,
} from '../lib/chatStorage';
import { ResearchTodosPanel } from './ResearchTodosPanel';
import { WikiChatComposer } from './WikiChatComposer';
import { WikiChatMessages } from './WikiChatMessages';

/**
 * Resize bounds in CSS pixels: ResizableDrawer measures the pointer, so its
 * arithmetic is px by nature (the same reason sidebarWidth.ts exports a px
 * value beside its rem one). 480px is the 30rem the app's other right-hand
 * surfaces open at.
 */
const DEFAULT_WIDTH = 480;
const MIN_WIDTH = 352;
const MAX_WIDTH = 800;

export interface WikiChatDrawerProps {
  readonly open: boolean;
  readonly onClose: () => void;
  readonly target: WikiChatTarget;
  /**
   * The open wiki version's page ids, offered as attachable context. They come
   * from the manifest the page already has open, so the picker can only offer
   * what the provider will accept for that version.
   */
  readonly contextPages?: readonly string[];
  /** A fresh identifier. Injected so a test can make ids predictable. */
  readonly newId?: () => string;
}

export const WikiChatDrawer = memo(function WikiChatDrawer({
  open,
  onClose,
  target,
  contextPages,
  newId,
}: WikiChatDrawerProps) {
  /**
   * The attachment lives HERE, not in the conversation state.
   *
   * It is a property of the next question, not of a turn already sent, and the
   * controller is deliberately headless — threading a selection through it
   * would put a rendering concern into a slice that has none. The consequence
   * is that `regenerate` re-asks with whatever is attached NOW, which is what
   * the control on screen says it will do.
   */
  const [contextPaths, setContextPaths] = useState<readonly string[]>([]);
  // Stable for the life of the drawer, so the conversation key is minted once
  // and the controller's id generator does not change identity per render.
  const mintId = useMemo(() => newId ?? (() => crypto.randomUUID()), [newId]);
  const storage = useMemo(
    () => createWikiChatStorage(target.projectId, target.toolkitId),
    [target.projectId, target.toolkitId],
  );
  // The handle this browser sends with every question so the server files the
  // turn in the right conversation. It reads and writes `localStorage`
  // lazily rather than living in state: minting must not re-render, and a
  // drawer that is never opened must never mint at all.
  const conversationKey = useMemo(
    () => createWikiConversationKey(target.projectId, target.toolkitId, mintId),
    // mintId is stable for the life of the drawer — see the memo below it.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see above
    [target.projectId, target.toolkitId],
  );

  // The LEGACY conversation, read once. A browser that kept one before the
  // server did shows it rather than an empty drawer; it is replaced the
  // moment the server's own transcript arrives, and never uploaded.
  const localMessages = useMemo(
    () => readLocalWikiMessages(target.projectId, target.toolkitId),
    [target.projectId, target.toolkitId],
  );

  // The selection is folded into the target rather than into the controller's
  // input, so `features/wiki-chat` needs no knowledge of attachments at all.
  const attaching = useMemo(
    () => ({ ...target, contextPaths }),
    [target, contextPaths],
  );

  const onContextPathsChange = useCallback((selected: readonly string[]) => {
    setContextPaths(selected);
  }, []);

  const chat = useWikiChat({
    // `attaching`, not `target`: a question asked with pages attached is the
    // same question, and it is recorded like any other. The conversation key
    // rides beside the body, so what the transcript stores is the question the
    // user typed while the attachment travels to the provider.
    invoke: (input) => startWikiChat(attaching, input, conversationKey.read()),
    initialMessages: localMessages,
    // The tool name is carried on the state's own pending turn rather than
    // recomputed from the mode: the mode can move while a turn is in flight,
    // and polling the wrong tool's path returns a 404 that reads as a lost
    // invocation.
    poll: (invocationId) =>
      pollWikiChat(target, chat.state.pendingCapability === 'research' ? 'deep_research' : 'ask', invocationId),
    storage,
    newId: mintId,
  });

  // Opening restores the capability the LAST ANSWER was produced with, which is
  // not necessarily the toggle's last position — see `capabilityOnOpen`.
  useEffect(() => {
    if (open) chat.restoreCapability();
    // Only on the open transition. Depending on the controller would re-run it
    // after every frame and fight the user's own toggle.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see above
  }, [open]);

  /*
   * THE SERVER'S COPY OF THIS CONVERSATION.
   *
   * Fetched only while the drawer is OPEN — a wiki page renders the drawer
   * closed, and a transcript nobody is looking at is a request per page view
   * for nothing. `staleTime: Infinity` is the other half of that: the answer
   * to "what did we say" changes only when THIS drawer says something, and it
   * already knows when it did, so a refetch on focus would replace the live
   * transcript with the server's slightly older one.
   */
  const [historyEpoch, setHistoryEpoch] = useState(0);
  const history = useQuery({
    queryKey: ['deepwiki', 'wiki-chat-history', target.projectId, target.toolkitId, historyEpoch],
    enabled: open,
    staleTime: Infinity,
    queryFn: async () => {
      const conversations = await listWikiConversations(target.projectId, target.toolkitId);
      const { key, minted } = conversationKey.resolve();
      let current = conversations.find((conversation) => conversation.chatKey === key);

      /*
       * A BROWSER THAT HAS NEVER BEEN HERE ADOPTS THE USER'S LATEST
       * CONVERSATION, and that is the feature rather than a convenience: on a
       * second device the drawer mints a key of its own, so without this it
       * would find no match and open empty — history stored on the server that
       * only the browser that wrote it can see is history nobody moved.
       *
       * Only when the key was MINTED by this very call. A browser that already
       * had one has already answered the question, and adopting over it would
       * resurrect the conversation the user just cleared.
       */
      if (!current && minted) {
        const adopted = conversations.find((conversation) => conversation.chatKey !== undefined);
        if (adopted?.chatKey !== undefined) {
          conversationKey.adopt(adopted.chatKey);
          current = adopted;
        }
      }

      return {
        // Whether this toolkit has ANY stored conversation, which is what
        // decides the fate of the local one below.
        stored: conversations.length > 0,
        messages: current ? await loadWikiTranscript(target.projectId, current.id) : [],
      };
    },
  });

  /*
   * Hand the loaded transcript to the controller, and retire the local one.
   *
   * ONCE PER LOAD, hence the ref: `hydrate` refuses a running turn, so a hook
   * that re-ran on every render would keep replacing a finished transcript
   * with the same one and lose nothing — but it would also undo a turn the
   * moment it settled. Keying the ref on the query's data identity is what
   * makes "a new load happened" the trigger rather than "a render happened".
   *
   * The local conversation is forgotten only when the server HAS one for this
   * toolkit. A user whose questions predate this feature keeps them on screen
   * until they ask something new, and then gets the server's copy instead —
   * nothing is deleted before it has been replaced.
   */
  const hydrated = useRef<unknown>(null);
  useEffect(() => {
    if (!history.data || hydrated.current === history.data) return;
    hydrated.current = history.data;
    if (history.data.stored) {
      chat.hydrate(history.data.messages);
      forgetLocalWikiMessages(target.projectId, target.toolkitId);
    }
    // The controller is rebuilt every render; depending on it would re-run
    // this on every frame. The guard above is what makes it run once.
    // eslint-disable-next-line react-hooks/exhaustive-deps -- see above
  }, [history.data, target.projectId, target.toolkitId]);

  /*
   * "Clear" starts a NEW conversation; it does not erase the old one.
   *
   * That is the change server-side history forces, and it is the better
   * behaviour: the previous conversation stays stored and readable, and the
   * next question opens a fresh one rather than appending to a chat the user
   * has visibly finished with.
   */
  const startNewConversation = () => {
    conversationKey.renew();
    hydrated.current = null;
    forgetLocalWikiMessages(target.projectId, target.toolkitId);
    chat.clear();
    // A new epoch, not a new key in the query key: the reload must re-ask the
    // listing (the conversation just left behind is now one of its rows) and
    // must do it exactly once.
    setHistoryEpoch((epoch) => epoch + 1);
  };

  const canRegenerate = useMemo(
    () => chat.state.messages.some((message) => !isThinkingBlock(message) && message.role === 'user'),
    [chat.state.messages],
  );

  return (
    <ResizableDrawer
      open={open}
      onClose={onClose}
      initialWidth={DEFAULT_WIDTH}
      minWidth={MIN_WIDTH}
      maxWidth={MAX_WIDTH}
      aria-label={t('widgets.deepwiki.chat.title', 'Wiki chat')}
      data-testid="wiki-chat-drawer"
    >
      <Stack
        sx={{ flexDirection: 'row', alignItems: 'center', p: 1.5, borderBottom: 1, borderColor: 'divider' }}
      >
        <Typography variant="headingSmall" sx={{ flex: 1 }}>
          {t('widgets.deepwiki.chat.title', 'Wiki chat')}
        </Typography>
        <IconButton size="small" onClick={onClose} aria-label={t('widgets.deepwiki.chat.close', 'Close')}>
          <CloseIcon fontSize="small" />
        </IconButton>
      </Stack>

      <ResearchTodosPanel todos={chat.state.todos} />

      {chat.state.messages.length === 0 && chat.state.streamingText === '' ? (
        <Box sx={{ p: 2, flex: 1 }}>
          <Typography variant="bodyMedium" color="text.secondary">
            {t(
              'widgets.deepwiki.chat.empty',
              'Ask a question about this repository, or switch to Research for a longer investigation.',
            )}
          </Typography>
        </Box>
      ) : (
        <WikiChatMessages
          messages={chat.state.messages}
          streamingText={chat.state.streamingText}
        />
      )}

      {/* The error is shown ALONGSIDE the failed turn, not instead of it. The
          turn already carries the message; this banner is what makes a failure
          visible when the list is scrolled away from it. */}
      {chat.state.error ? (
        <Alert severity="error" variant="outlined" sx={{ mx: 1.5 }} data-testid="wiki-chat-error">
          {chat.state.error}
        </Alert>
      ) : null}

      <WikiChatComposer
        mode={chat.state.mode}
        onModeChange={chat.setMode}
        onSend={chat.send}
        onRegenerate={chat.regenerate}
        onClear={startNewConversation}
        isLoading={chat.state.isLoading}
        canRegenerate={canRegenerate}
        contextPages={contextPages ?? []}
        contextPaths={contextPaths}
        onContextPathsChange={onContextPathsChange}
      />
    </ResizableDrawer>
  );
});
