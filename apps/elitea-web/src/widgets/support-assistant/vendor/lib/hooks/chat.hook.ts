import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useApi } from './api.hook';
import { useSupportAssistantContext } from './supportContext.hook';
import { useSupportStream } from './stream.hook';
import type { TConversationListItem, TMessage, TRawConversation, TSocketMessage } from '../types';
import type { TSupportAttachmentRef } from '../../api';
import { applyPredictFrame, generateUUID, parseConversationMessages } from '../utils';

type TUseChatProps = {
  welcomeMessage: string;
  /**
   * The hidden support project.
   *
   * It is accepted and NOT READ. Its only consumer was the socket room join
   * (`chat_enter_room` carried `project_id`), which is gone with the transport;
   * the SSE stream is named by an `events_url` the server builds itself, so the
   * client never has to know the project. It stays on the props because the
   * config endpoint still reports it and a future surface (a "view this in the
   * full chat" link, say) would need it — and because removing it would make the
   * config field look unused end to end when it is not.
   */
  supportProjectId: number | null;
  initialHistory: TConversationListItem[];
  initialConversation: TRawConversation | null;
  isInitLoading: boolean;
};

export const useChat = (props: TUseChatProps) => {
  const { welcomeMessage, initialHistory, initialConversation, isInitLoading } = props;

  const hasInitializedRef = useRef(false);

  const api = useApi();
  const supportAssistantContext = useSupportAssistantContext();

  const createWelcomeMessages = useCallback(
    (): TMessage[] =>
      welcomeMessage
        ? [{ id: 'welcome', role: 'assistant' as const, content: welcomeMessage, timestamp: Date.now() }]
        : [],
    [welcomeMessage],
  );

  const [messages, setMessages] = useState<TMessage[]>([]);
  const [inputText, setInputText] = useState('');
  const [currentConversationId, setCurrentConversationId] = useState<string | null>(null);
  const [history, setHistory] = useState<TConversationListItem[]>([]);
  const [isSwitchingConversation, setIsSwitchingConversation] = useState(false);

  const isLoading = useMemo(
    () => isInitLoading || isSwitchingConversation,
    [isInitLoading, isSwitchingConversation],
  );

  const isStreaming = useMemo(() => messages.some(m => m.isStreaming || m.isAnimating), [messages]);

  const handleAnimationComplete = useCallback((messageId: string) => {
    setMessages(prev => prev.map(m => (m.id === messageId ? { ...m, isAnimating: false } : m)));
  }, []);

  /*
   * TRANSPORT.
   *
   * The published widget joins a socket ROOM per conversation and emits
   * `support_predict`; this platform has no socket.io, so a turn is started over
   * REST and its frames arrive on the execution's SSE stream. See
   * `stream.hook.ts` for the full account of what that changes — in particular
   * that `enterRoom`/`leaveRoom` have no equivalent and are gone, because an
   * execution stream is a subscription to ONE TURN rather than to a
   * conversation.
   */
  const settleStreamingMessages = useCallback((reason?: string) => {
    setMessages(prev =>
      prev.map(m =>
        m.isStreaming
          ? {
              ...m,
              isStreaming: false,
              statusMessage: undefined,
              ...(reason !== undefined && m.content === ''
                ? { content: reason, isError: true }
                : {}),
            }
          : m,
      ),
    );
  }, []);

  /*
   * ONE STREAM FRAME, APPLIED TO THE TRANSCRIPT.
   *
   * The decision lives in `utils/predictFrame.utils.ts` because it is pure and
   * because it is the half of this widget that a wrong assumption can silence
   * completely — see that module for the frame vocabulary this platform really
   * sends, and for the defect that reading the reference's vocabulary caused.
   */
  const handlePredict = useCallback((message: TSocketMessage) => {
    setMessages(prev => applyPredictFrame(prev, message));
  }, []);

  const handleError = useCallback((data: { error: string; code: string }) => {
    setMessages(prev => [
      ...prev,
      {
        id: generateUUID(),
        role: 'assistant' as const,
        content: data.error || 'An error occurred',
        timestamp: Date.now(),
        isError: true,
      },
    ]);
  }, []);

  /*
   * `handleConversationNameUpdated` IS GONE. It listened for
   * `chat_conversation_name_updated`, a frame the server pushed down the
   * conversation's socket ROOM after auto-naming it. There is no room to push it
   * down, so the history list picks the new name up on its next read instead of
   * live. See `stream.hook.ts`.
   */

  useEffect(() => {
    if (isInitLoading || hasInitializedRef.current) return;
    hasInitializedRef.current = true;

    setHistory(initialHistory);

    const mostRecent = initialHistory[0];
    if (mostRecent && initialConversation) {
      const parsed = parseConversationMessages(initialConversation);
      setMessages(parsed.length > 0 ? parsed : createWelcomeMessages());
      setCurrentConversationId(mostRecent.uuid);
    } else {
      setMessages(createWelcomeMessages());
    }
  }, [isInitLoading, initialHistory, initialConversation, createWelcomeMessages]);

  const stream = useSupportStream({
    /*
     * The cast is the transport seam, and it is narrow on purpose.
     *
     * `ExecutionEventData` is `Readonly<Record<string, unknown>>` — the SSE
     * layer deliberately does not type the frame, because the same envelope
     * carries every node event the runtime emits. `TSocketMessage` is the
     * widget's view of the subset it reads. They describe the SAME BYTES:
     * `features/chat-messages/lib/chatStreamFrame.ts` records that the SSE
     * `execution.node_event` payload is identical to the socket's `chat_predict`
     * receive event, down to it still carrying `sio_event: "chat_predict"`.
     *
     * `handlePredict` reads four fields and switches on `type`, treating an
     * unrecognised one as a no-op, so a frame that does not match this shape
     * falls through rather than throwing.
     */
    onFrame: frame => handlePredict(frame as unknown as TSocketMessage),
    onSettled: settleStreamingMessages,
  });

  const handleSend = useCallback(
    async (
      text: string,
      files?: readonly File[],
      onFileStatus?: (file: File, status: 'pending' | 'uploading' | 'done' | 'error') => void,
    ) => {
      let activeConversationId = currentConversationId;

      // Create conversation if needed
      if (!activeConversationId) {
        try {
          const created = await api.createConversation();
          activeConversationId = created.uuid;
          setCurrentConversationId(activeConversationId);
          setHistory(prev => [created, ...prev]);
        } catch {
          setMessages(prev => [
            ...prev,
            {
              id: generateUUID(),
              role: 'assistant',
              content: 'Failed to create conversation. Please try again.',
              timestamp: Date.now(),
              isError: true,
            },
          ]);
          return;
        }
      }

      setMessages(prev => [
        ...prev,
        { id: generateUUID(), role: 'user', content: text, timestamp: Date.now() },
      ]);

      if (!activeConversationId) return;

      /*
       * ATTACHMENTS (issue #625 item 2; multi-file since #877): uploaded
       * BEFORE the turn starts, one at a time — there is no batch upload
       * route, so each file rides the SAME single-file artifact path the
       * main chat composer uses server-side (internal/api/v2/
       * supportassistant/attachments.go). Sequential and abort-on-first-
       * failure by design: a failed upload reports failure and STOPS here,
       * before any later file uploads and before a turn the user believes
       * carried every attached file. `onFileStatus` lets the composer
       * (`../../components/chat/MessageInput.tsx`) show a per-file
       * queued/uploading/done/error chip as this loop runs.
       */
      let attachments: TSupportAttachmentRef[] | undefined;
      if (files && files.length > 0) {
        attachments = [];
        for (const file of files) {
          onFileStatus?.(file, 'uploading');
          try {
            // eslint-disable-next-line no-await-in-loop -- sequential by design: each upload must resolve before the next starts, there is no batched endpoint (matches entities/conversation/lib/hooks/useUploadAttachments.ts's identical loop).
            const uploaded = await api.uploadAttachment(activeConversationId, file);
            attachments.push({ filepath: uploaded.filepath, name: file.name });
            onFileStatus?.(file, 'done');
          } catch {
            onFileStatus?.(file, 'error');
            handleError({ error: 'Failed to attach the file. Please try again.', code: 'ATTACHMENT_UPLOAD_FAILED' });
            return;
          }
        }
      }

      /*
       * START THE TURN, then subscribe.
       *
       * `question_id` is generated HERE, once, and is the turn's idempotency
       * key: the server derives the turn's message identifiers from it, so a
       * retried POST resumes the same run rather than billing a second one. It
       * is why the endpoint requires it instead of minting one itself.
       */
      try {
        const started = await api.startTurn(activeConversationId, {
          content: text,
          question_id: generateUUID(),
          support_assistant_context: supportAssistantContext
            ? (supportAssistantContext as unknown as Record<string, unknown>)
            : undefined,
          attachments,
        });
        if (started.events_url) {
          stream.open(started.events_url);
        } else {
          // A 200 with no stream to subscribe to. The run may well be live, but
          // this client has no way to see it finish, and a spinner that never
          // resolves is worse than saying so.
          handleError({ error: 'The support assistant did not return a response stream.', code: 'NO_STREAM' });
        }
      } catch {
        handleError({ error: 'Failed to reach the support assistant. Please try again.', code: 'START_FAILED' });
      }
    },
    [
      currentConversationId,
      api,
      supportAssistantContext,
      stream,
      handleError,
    ],
  );

  const handleNewChat = useCallback(() => {
    // Dropping the stream WITHOUT settling: the user left the turn rather than
    // the turn failing, so nothing should be marked as an error. The run itself
    // keeps going server-side and its answer is in the transcript when they come
    // back to that conversation.
    stream.close();
    setCurrentConversationId(null);
    setMessages(createWelcomeMessages());
    setInputText('');
  }, [stream, createWelcomeMessages]);

  const handleSelectConversation = useCallback(
    async (conversationId: string) => {
      if (currentConversationId === conversationId) return;
      // #328's rule, in miniature: a stream belongs to the conversation that
      // started it. Leaving that conversation drops it, so its frames cannot be
      // folded into the transcript now on screen.
      stream.close();

      setCurrentConversationId(conversationId);
      setInputText('');
      setMessages([]);
      setIsSwitchingConversation(true);

      try {
        const conversation = await api.getConversation(conversationId);
        const parsed = parseConversationMessages(conversation);
        setMessages(parsed.length > 0 ? parsed : createWelcomeMessages());
      } catch {
        setMessages(createWelcomeMessages());
      } finally {
        setIsSwitchingConversation(false);
      }
    },
    [currentConversationId, stream, createWelcomeMessages, api],
  );

  return {
    messages,
    inputText,
    setInputText,
    history,
    currentConversationId: currentConversationId ?? '',
    isLoading,
    isStreaming,
    handleNewChat,
    handleSelectConversation,
    handleSend,
    handleAnimationComplete,
  };
};
