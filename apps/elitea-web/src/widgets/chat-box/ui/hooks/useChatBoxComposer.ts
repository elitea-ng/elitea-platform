/**
 * Split out of `ChatBox.tsx` (issue 1808) to keep the composition root under
 * the §3.5 file-length budget with one statement per line: the composer's
 * hook wiring — "@"/"#"/"~" mentions, the action callbacks, the authored
 * pipeline pause, the A17 queue, steering, the seeded-turn reattach, the
 * unattended chain follow, Stop / Esc Esc and the host's imperative handle.
 *
 * The hooks run in the order `ChatBox` called them before the split.
 */
import type { ComponentRef, Ref, RefObject } from 'react';
import { useCallback, useImperativeHandle } from 'react';

import type { NewChatInput } from '@/features/chat-input';
import { useChatSteering, type ChatMessage, type ChatSteeringModel, type useDeleteMessageAlert } from '@/features/chat-messages';
import { useDoubleEscape } from '@/shared/lib/useDoubleEscape';

import type { ChatBoxHandle } from '../ChatBox.types';
import { chatBoxQueueKey, useChatBoxQueuedMessages, type ChatBoxQueuedMessagesModel } from '../ChatBoxQueuedMessages';
import { sendTypedStaticContinuation, useStaticPauseCards, type StaticPauseCardsBinding } from '../StaticPipelineContinueControls';
import { useChatBoxActions, type UseChatBoxActionsParams, type UseChatBoxActionsResult } from './useChatBoxActions';
import { useChatBoxChainFollow } from './useChatBoxChainFollow';
import type { useChatBoxData } from './useChatBoxData';
import type { useChatBoxHandlers } from './useChatBoxHandlers';
import { useChatBoxMentions, type UseChatBoxMentionsResult } from './useChatBoxMentions';
import type { UseChatBoxSendResult } from './useChatBoxSend';
import type { useChatBoxState } from './useChatBoxState';
import type { ChatBoxUnattended } from './useChatBoxUnattended';
import { stopChatGeneration } from './useEditorTestTransport';
import { useReattachSeededTurn } from './useReattachSeededTurn';
import { useStableRef } from './useStableRef';

type NewChatInputHandle = ComponentRef<typeof NewChatInput>;
type EditorTest = Parameters<typeof useStaticPauseCards>[0]['editorTest'];

interface UseChatBoxComposerParams {
  /** The host's `ChatBoxHandle` ref — never `chatInputRef` (see `ChatBox.types.ts`). */
  readonly ref: Ref<ChatBoxHandle> | undefined;
  readonly chatInputRef: RefObject<NewChatInputHandle | null>;
  readonly hidden: boolean;
  readonly state: ReturnType<typeof useChatBoxState>;
  readonly data: ReturnType<typeof useChatBoxData>;
  readonly handlers: ReturnType<typeof useChatBoxHandlers>;
  readonly deleteAlert: ReturnType<typeof useDeleteMessageAlert>;
  readonly messages: readonly ChatMessage[];
  readonly onChangeParticipant: (participant: unknown) => void;
  readonly participants: readonly unknown[] | undefined;
  readonly isAgentsPage: boolean | undefined;
  readonly isStreaming: boolean;
  readonly projectId: string | number | undefined;
  readonly conversationId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  readonly editorTest: EditorTest;
  readonly readAloud: { readonly stop: () => void };
  readonly onConversationCreated: UseChatBoxActionsParams['onConversationCreated'];
  readonly onSkillsUnavailable: (message: string) => void;
  readonly send: Pick<UseChatBoxSendResult, 'continueStreamedExecution' | 'reattachStreamedExecution' | 'stopStreamedExecution' | 'streamTarget'>;
  readonly unattended: Pick<ChatBoxUnattended, 'wire'>;
}

export interface ChatBoxComposerModel {
  readonly mentions: UseChatBoxMentionsResult;
  readonly actions: UseChatBoxActionsResult;
  readonly staticPause: StaticPauseCardsBinding | undefined;
  readonly queued: ChatBoxQueuedMessagesModel;
  readonly steering: ChatSteeringModel;
  /** The A17 queue key of the conversation on screen. */
  readonly queueKey: string;
  /** Stops the transport's run and, outside the editor test, the socket task (#328). */
  readonly stopGeneration: () => void;
  /** The composer's Stop, and Esc Esc anywhere on the page: the leftovers of the turn are offered, not sent. */
  readonly stopTurn: () => void;
}

export function useChatBoxComposer(params: UseChatBoxComposerParams): ChatBoxComposerModel {
  const { ref, chatInputRef, hidden, state, data, handlers, messages, isStreaming, projectId, conversationUuid, editorTest, send } = params;
  // "@" mention -> send-to-user/everyone routing, "~" skill selection, + "@"/"#" agents (ADR-0050 7c)
  const mentions = useChatBoxMentions({
    state,
    onChangeParticipant: params.onChangeParticipant,
    participants: params.participants,
    chatInput: chatInputRef,
    sendQuestion: handlers.sendQuestion,
  });

  // Action callbacks (send, regenerate, copy, delete, edit-resubmit, HITL resume, MCP/token-limit continue, clear chat, starter send)
  const readAloudRef = useStableRef(params.readAloud);
  const readAloudStop = useCallback(() => {
    readAloudRef.current.stop();
  }, [readAloudRef]);
  const actions = useChatBoxActions({
    chatInputRef,
    data,
    state,
    handlers,
    deleteAlert: params.deleteAlert,
    messages,
    isAgentsPage: params.isAgentsPage,
    readAloudStop,
    onConversationCreated: params.onConversationCreated,
    onSkillsUnavailable: params.onSkillsUnavailable,
    takeAddressedTo: mentions.takeAddressedTo,
  });
  const { handleSend, handleClear } = actions;

  const staticPause = useStaticPauseCards({ messages, setChatHistory: data.setChatHistory, projectId, conversationUuid, continueExecution: send.continueStreamedExecution, editorTest, isStreaming });
  // A message typed while an authored pipeline pause is current continues that
  // pause with the text (the durable static continuation), not a new turn.
  const sendNow = useCallback((question: string) => {
    if (!staticPause) return handleSend(question);
    void sendTypedStaticContinuation(staticPause, question, () => chatInputRef.current?.reset?.());
  }, [staticPause, handleSend, chatInputRef]);

  // A17: while a turn is open the composer QUEUES instead of sending; the queue
  // drains FIFO on settle down this same `sendQuestion` path, survives Stop and
  // a reload, and stamps its deliveries so the transcript can label them.
  const queueKey = chatBoxQueueKey(conversationUuid, params.conversationId);
  const queued = useChatBoxQueuedMessages({
    conversationKey: queueKey,
    isStreaming,
    onSendNow: sendNow,
    send: mentions.queuedSend, // a queued message keeps its own addressing (ADR-0050 7c)
    onConversationCreated: params.onConversationCreated,
  });

  // Imperative handle (stable via refs, so identity never churns)
  const handleClearRef = useStableRef(handleClear);
  const streamingRef = useStableRef(data.streaming);
  // Stop has two halves and needs both (#328): the socket-era stop-task + room
  // leave, for a task the SSE path never registered, and the transport's own
  // cancel-and-close, for a stream the socket path never opened.
  const stopStreamRef = useStableRef(send.stopStreamedExecution);
  const stopGeneration = useCallback(() => {
    stopChatGeneration(stopStreamRef.current, () => streamingRef.current.stopStreaming(), editorTest);
  }, [stopStreamRef, streamingRef, editorTest]);
  // Contract 1.27: a send while the owned cloud turn runs STEERS it; a refusal or a leftover goes through the A17 queue.
  const steering = useChatSteering({
    conversationKey: queueKey,
    isStreaming,
    streamTarget: send.streamTarget,
    send: queued.onSend,
    queueAfterTurn: queued.enqueue,
    answerFailed: (id) => messages.some((m) => m.id === id && (Boolean(m.exception) || m.failureCode !== undefined)),
  });
  // #6654: a reload mid-turn observes the run again
  useReattachSeededTurn({ messages, conversationUuid, reattach: send.reattachStreamedExecution, onReattached: steering.onReattached });
  // ADR-0045: what Main writes after an unattended answer settles (brief, goal check, continuations) shows without a reload.
  const chainFollow = useChatBoxChainFollow({
    enabled: !editorTest,
    projectId,
    conversationUuid,
    messages,
    isStreaming,
    setChatHistory: data.setChatHistory,
    // ADR-0050 7c: Main starts later floor turns itself
    granted: () => params.unattended.wire() !== undefined || mentions.addressableAgents.length >= 2,
  });
  const stopTurn = (): void => {
    chainFollow.cancel();
    steering.onStop();
    stopGeneration();
  };
  useDoubleEscape(isStreaming && !hidden, stopTurn);
  // MUST target the host `ref`, never `chatInputRef` — see `ChatBox.types.ts`.
  useImperativeHandle(ref, () => ({
    onClear: () => {
      handleClearRef.current();
    },
    mentionUser: (c) => {
      chatInputRef.current?.setValue?.(`@${c} `);
    },
    stopAll: stopGeneration,
  }), [handleClearRef, stopGeneration, chatInputRef]);

  return { mentions, actions, staticPause, queued, steering, queueKey, stopGeneration, stopTurn };
}
