/**
 * Data primitives hook for ChatBox.
 *
 * Supplies the conversation lifecycle (create / edit / delete / select /
 * regenerate), streaming management (including a real stop-streaming
 * backend call + socket room leave), live socket-synced chat history,
 * model resolution (default + saved-model lookup), pending-HITL-interrupt
 * detection, and the participant-resolution helper — all of which the
 * composition root needs before it can even attempt to render a message
 * list or call NewChatInput.
 *
 * Port of `ChatBox.jsx` lines ~186-199 (pending HITL), ~213-260/303-378/
 * 394-397 (model / defaults), ~545-593/632-635 (chat socket + stop
 * streaming), and ~735-765 (attachment removal, via `useAttachmentState`/
 * `useUploadAttachments`).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Dispatch, SetStateAction } from 'react';

import {
  useConversationLifecycle,
  useChatStreaming,
  chatHelpers,
  conversationApi,
  useAttachmentState,
  useUploadAttachments,
} from '@/entities/conversation';
import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message';
import {
  convertMessagesToChatHistory,
  useSyncChatMessage,
} from '@/features/chat-messages';
import type { ChatMessage } from '@/features/chat-messages';
import { useListModelsQuery } from '@/shared/api/configurationsApi';
import type { ConfigModel } from '@/shared/api/configurationsApi';
import { useSocketClient } from '@/shared/api/socket/client';

/**
 * `UseChatStreamingResult`/`StreamingChatHistoryItem` aren't re-exported from
 * `entities/conversation`'s barrel (§3.5 20-export budget) — derived here
 * from the barrel-exported `useChatStreaming` itself rather than deep-
 * importing `lib/hooks/useChatStreaming`/`lib/wire` (R-L3,
 * `no-deep-slice-import-cross-slice`), matching the same derivation pattern
 * `widgets/chat-box/ui/ChatBox.tsx` uses for `NewChatInputHandle`. The same
 * pattern derives `ConversationForSync` from the barrel-exported
 * `useSyncChatMessage` below, for the identical reason.
 */
type UseChatStreamingResult = ReturnType<typeof useChatStreaming>;
type StreamingChatHistoryItem = NonNullable<Parameters<typeof useChatStreaming>[0]['chatHistory']>[number];
type ConversationForSync = Parameters<typeof useSyncChatMessage>[0]['activeConversation'];

/* ------------------------------------------------------------------ */
/*  Derived shapes                                                      */
/* ------------------------------------------------------------------ */

/** Resolved LLM model reference. */
export interface ChatBoxModel {
  readonly name?: string;
  readonly projectId?: string;
  readonly supportsReasoning?: boolean;
}

/** Normalised message list ready for ChatMessageList rendering. */
export interface ChatBoxMessageList {
  readonly messages: readonly ChatMessage[];
  /** Whether any message is currently streaming (derived from persisted history). */
  readonly isStreamingFromHistory: boolean;
}

/* ------------------------------------------------------------------ */
/*  Hook                                                                */
/* ------------------------------------------------------------------ */

/**
 * Params required from the composition root — only primitive values so the
 * hook result is referentially stable across renders for the same identity.
 */
export interface UseChatBoxDataParams {
  readonly conversationId: string | number | undefined;
  readonly conversationUuid: string | undefined;
  /** Wire-shape participants array from the conversation. */
  readonly participants: unknown[] | undefined;
  /** Wire-shape message_groups[] from the conversation details API. */
  readonly messageGroups: MessageGroupWire[] | undefined;
  /** The active agent / pipeline / toolkit participant (may be null for model-only chat). */
  readonly activeParticipant: unknown;
  readonly projectId: string | number | undefined;
  readonly userId: string | undefined;
  readonly userName: string | undefined;
  readonly userAvatar: string | undefined;
  readonly isAgentsPage: boolean | undefined;
  readonly conversationIsPlayingBack: boolean;
}

export interface UseChatBoxDataResult {
  /** Bundled lifecycle actions (create / edit / delete / select / unselect). */
  readonly lifecycle: ReturnType<typeof useConversationLifecycle>;
  /** Streaming state and controls (setStreamingInfo, stopStreaming, isStreamingNow). `stopStreaming` now calls the real backend stop-task + leaves the stream's socket room. */
  readonly streaming: UseChatStreamingResult;
  /** Attachment management (local CRUD + chunked upload). */
  readonly attachments: {
    state: ReturnType<typeof useAttachmentState<File>>;
    upload: ReturnType<typeof useUploadAttachments>;
  };
  /** Normalised, live socket-synced message list for ChatMessageList (starts from `messageGroups`, updated by `chat_message_sync` events). */
  readonly messageList: ChatBoxMessageList;
  /** Direct setter for the SAME live chat-history state `messageList.messages` reads from — the seam action handlers (send / regenerate / HITL resume / delete) use for optimistic local updates that `chat_message_sync` alone can't drive (e.g. before the backend has even persisted a reply). */
  readonly setChatHistory: Dispatch<SetStateAction<readonly ChatMessage[]>>;
  /** Resolve a participant by ID from the conversation's participant list. */
  readonly resolveParticipant: (participantId: string | number) => MessageParticipantWire | undefined;
  /** Set the selected LLM model (called when user picks one from the dropdown). */
  readonly setSelectedModel: (model: ChatBoxModel | null) => void;
  /** Currently selected LLM model. */
  readonly selectedModel: ChatBoxModel | null;
  /** Default LLM model from available models (null until resolved). */
  readonly defaultModel: ChatBoxModel | null;
  /** Resolve the saved or default model for the current conversation/participant. */
  readonly selectSavedOrDefaultModel: (forceSelect?: boolean) => void;
  /** The most recent message carrying a pending HITL interrupt, if any (baseline: `ChatBox.jsx:186-195`). */
  readonly pendingHitlMessage: ChatMessage | undefined;
  /** Whether `pendingHitlMessage` actually carries an interrupt to act on (baseline: `ChatBox.jsx:197-199`). */
  readonly hasPendingHitlInterrupt: boolean;
}

/** `ChatBoxModel` from a raw `ConfigModel` (or `null` when none resolved). */
function toChatBoxModel(model: ConfigModel | null | undefined): ChatBoxModel | null {
  if (!model) return null;
  return {
    name: model.name,
    projectId: model.project_id,
    supportsReasoning: Boolean(model.supports_reasoning),
  };
}

/**
 * Hook that provides conversation lifecycle, streaming, attachment management,
 * live socket-synced message normalisation, model resolution, and pending-HITL
 * detection for the ChatBox composition root.
 */
export function useChatBoxData(params: UseChatBoxDataParams): UseChatBoxDataResult {
  const {
    conversationId,
    conversationUuid,
    participants,
    messageGroups,
    projectId,
    userId,
    isAgentsPage,
    // conversationIsPlayingBack is reserved for future use in streaming mode
    conversationIsPlayingBack: _conversationIsPlayingBack,
  } = params;

  // -- Lifecycle --
  const lifecycle = useConversationLifecycle(projectId);

  // -- Socket + stop-task mutation (feeds streaming's onStopStreaming below) --
  const socketClient = useSocketClient();
  const { mutateAsync: stopChatTaskMutateAsync } = conversationApi.useStopTask();

  const onStopStreaming = useCallback(
    (message: StreamingChatHistoryItem): (() => void) | undefined => {
      // `message` is actually the raw wire message_group `useChatStreaming`
      // found it in (`streamingChatHistory` below), just narrowly typed —
      // same cast pattern `resolveParticipant` uses for `chatHelpers`.
      const wireMessage = message as unknown as { readonly uuid?: string; readonly id?: string | number; readonly task_id?: string };
      const streamId = wireMessage.uuid ?? (wireMessage.id !== undefined ? String(wireMessage.id) : undefined);
      if (streamId === undefined) return undefined;
      return () => {
        if (wireMessage.task_id && projectId !== undefined) {
          void stopChatTaskMutateAsync({ projectId, messageGroupUuid: streamId });
        }
        socketClient.emit('chat_leave_rooms', [streamId]);
      };
    },
    [projectId, socketClient, stopChatTaskMutateAsync],
  );

  // -- Streaming --
  // The streaming hook needs the chat history in StreamingChatHistoryItem shape.
  // Since we normalise to ChatMessage[] for rendering, we re-use the raw
  // message groups (which carry the same discriminating fields) as the
  // streaming chat history source.
  const streamingChatHistory: readonly StreamingChatHistoryItem[] | undefined = useMemo(() => {
    if (!messageGroups || messageGroups.length === 0) return undefined;
    return messageGroups;
  }, [messageGroups]);

  const streaming = useChatStreaming({
    projectId,
    conversationId: conversationUuid ?? String(conversationId ?? ''),
    chatHistory: streamingChatHistory,
    onStopStreaming,
    isChatStreaming: !isAgentsPage,
  });

  // -- Attachments --
  const attachmentState = useAttachmentState<File>();
  const uploadAttachmentsHook = useUploadAttachments();

  // -- Live, socket-synced chat history --
  // Seeded from messageGroups/participants (conversation load or switch) and
  // kept live by `useSyncChatMessage`'s `chat_message_sync` listener, which
  // merges each incoming persisted message_group into `chat_history` below.
  const seedConversationForSync = useCallback(
    (): ConversationForSync => ({
      // `exactOptionalPropertyTypes`: only set `id`/`participants` when
      // actually present, rather than assigning an explicit `undefined`.
      ...(conversationId !== undefined ? { id: conversationId } : {}),
      chat_history: convertMessagesToChatHistory(messageGroups ?? [], participants as MessageParticipantWire[] | undefined),
      ...(participants !== undefined ? { participants: participants as MessageParticipantWire[] } : {}),
    }),
    [conversationId, messageGroups, participants],
  );

  const [conversationForSync, setConversationForSync] = useState<ConversationForSync>(seedConversationForSync);

  /**
   * Which conversation the live history currently belongs to. `undefined` is a
   * real value here: a chat with no row yet, which is where the FIRST SEND
   * starts.
   */
  const seededIdentity = conversationUuid ?? (conversationId !== undefined ? String(conversationId) : undefined);
  const seededIdentityRef = useRef<string | undefined>(seededIdentity);

  /*
   * DEFECT this closes. The first send COMMITS the conversation before it tries
   * any transport, and the page then routes to `/chat/{id}` — so the very act
   * of adopting the row re-ran this effect with a server answer that has no
   * messages in it yet, and the optimistic question (plus, on a refused turn,
   * its failure bubble and error trace) was erased from the screen a moment
   * after it was drawn. On a deployment with a live transport the socket sync
   * put the persisted copy back and hid it; with no transport — every E2E stack
   * (`VITE_SOCKET_SERVER: ""`) and any refused turn — nothing put it back and
   * the reader's own question vanished.
   *
   * So an EMPTY server seed is not authoritative over a non-empty live history:
   * it only means "the server has nothing to add yet". It replaces the history
   * only when the reader really moved to a DIFFERENT conversation, which is the
   * case the re-seed exists for. A seed that carries messages still wins, and
   * the Create control resets the surface by remounting the subtree
   * (`useCreateChatReset` keys it), so neither path is weakened here.
   */
  useEffect(() => {
    const previousIdentity = seededIdentityRef.current;
    seededIdentityRef.current = seededIdentity;
    const seed = seedConversationForSync();
    const switchedConversation = previousIdentity !== undefined && previousIdentity !== seededIdentity;
    const seedIsEmpty = (seed.chat_history ?? []).length === 0;
    setConversationForSync((prev) => {
      const live = prev.chat_history ?? [];
      if (switchedConversation || !seedIsEmpty || live.length === 0) return seed;
      return { ...seed, chat_history: live };
    });
  }, [seedConversationForSync, seededIdentity]);

  useSyncChatMessage({ activeConversation: conversationForSync, setActiveConversation: setConversationForSync });

  // -- Messages --
  const messageList = useMemo<ChatBoxMessageList>(
    () => ({ messages: conversationForSync.chat_history ?? [], isStreamingFromHistory: false }),
    [conversationForSync.chat_history],
  );

  const setChatHistory = useCallback<Dispatch<SetStateAction<readonly ChatMessage[]>>>((action) => {
    setConversationForSync((prev) => {
      const prevHistory = prev.chat_history ?? [];
      const nextHistory = typeof action === 'function' ? action(prevHistory) : action;
      return { ...prev, chat_history: nextHistory };
    });
  }, []);

  // -- Pending HITL interrupt --
  const pendingHitlMessage = useMemo(
    () => messageList.messages.findLast((item) => Boolean(item.hitlInterrupt) || Boolean(item.hitlInterrupts?.length)),
    [messageList.messages],
  );
  const hasPendingHitlInterrupt = Boolean(pendingHitlMessage?.hitlInterrupt || pendingHitlMessage?.hitlInterrupts?.length);

  // -- Participant resolution --
  const resolveParticipant = useCallback(
    (participantId: string | number): MessageParticipantWire | undefined => {
      if (!participants || !Array.isArray(participants)) return undefined;
      return chatHelpers.getParticipantById(
        { participants } as Parameters<typeof chatHelpers.getParticipantById>[0],
        String(participantId),
      ) as MessageParticipantWire | undefined;
    },
    [participants],
  );

  // -- Models --
  const [selectedModel, setSelectedModel] = useState<ChatBoxModel | null>(null);

  const { data: modelsData } = useListModelsQuery(
    { projectId: projectId !== undefined ? String(projectId) : '', include_shared: true },
    { enabled: projectId !== undefined },
  );

  const defaultModel = useMemo<ChatBoxModel | null>(() => {
    const items = modelsData?.items ?? [];
    return toChatBoxModel(items.find((m) => m.default) ?? items[0] ?? null);
  }, [modelsData?.items]);

  const selectSavedOrDefaultModel = useCallback(
    (_forceSelect = true) => {
      const items = modelsData?.items ?? [];
      const conversationForModel = { participants } as Parameters<typeof chatHelpers.getSelectedConversationModel>[0];
      const saved = chatHelpers.getSelectedConversationModel(conversationForModel, items, userId) as unknown as ConfigModel | null;
      setSelectedModel(saved ? toChatBoxModel(saved) : defaultModel);
    },
    [modelsData?.items, participants, userId, defaultModel],
  );

  useEffect(() => {
    selectSavedOrDefaultModel(false);
  }, [selectSavedOrDefaultModel, conversationId, conversationUuid]);

  return {
    lifecycle,
    streaming,
    attachments: { state: attachmentState, upload: uploadAttachmentsHook },
    messageList,
    setChatHistory,
    resolveParticipant,
    setSelectedModel,
    selectedModel,
    defaultModel,
    selectSavedOrDefaultModel,
    pendingHitlMessage,
    hasPendingHitlInterrupt,
  };
}
