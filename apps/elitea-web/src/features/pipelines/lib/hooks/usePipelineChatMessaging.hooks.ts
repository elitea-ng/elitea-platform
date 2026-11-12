import { useCallback, useState } from 'react';

import { ROLES } from '@/shared/lib/enums';
import type { Participant } from '@/entities/participant';

import { pipelineErrorMessage } from './pipelineErrorMessage';
import { buildLlmSettingsFallback } from './pipelineChat.helpers';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';
import type {
  ChatConversation,
  ChatConversationAdapter,
  ChatHistoryMessage,
  ChatPipelineVersionDetails,
  ChatSource,
  CreateConversationAdapterResult,
  SendMessageData,
  SendResult,
} from './pipelineChat.types';

/**
 * "Send a message" slice of `usePipelineChat` —
 * `handleCreateConversationOnFirstMessage`/`handleMessage`/`onSend`. Split
 * out of `usePipelineChat.hooks.ts` purely to keep every function under this
 * codebase's `complexity`/`max-lines` gates, mirroring
 * `features/agents/lib/hooks/useApplicationChatMessaging.hooks.ts` for the
 * sibling baseline hook.
 */
export interface UsePipelineChatMessagingParams {
  readonly pipelineName: string | undefined;
  readonly pipelineParticipant: Participant | null;
  readonly pipelineVersionDetails: ChatPipelineVersionDetails | undefined;
  readonly projectId: string | undefined;
  readonly source: ChatSource;
  readonly adapter: ChatConversationAdapter;
  readonly activeConversationId: string | number | undefined;
  readonly setActiveConversation: (c: ChatConversation) => void;
  readonly setActiveParticipant: (p: Participant) => void;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UsePipelineChatMessagingResult {
  readonly onSend: (messageData: SendMessageData) => Promise<SendResult>;
  /**
   * Tracks the REAL network latency of `adapter.createConversation(...)` inside
   * `createConversationOnFirstMessage` — true only while that `await` is in flight. Baseline
   * `usePipelineChat.hooks.js:94` sources its own `isLoadingConversation` from
   * `useConversationCreateMutation()`'s `isLoading`, i.e. the actual create-conversation request,
   * NOT from any of the local synchronous "am I about to setState" flags this port also tracks
   * (`usePipelineChatConversation.hooks.ts`'s `isCreatingConversation`, which — see that file's own
   * `useInitializeConversationEffect` — sets/unsets its flag synchronously within a single effect
   * body with no `await` between, so React 18's automatic batching means a consumer NEVER observes
   * it as `true` across a render; it cannot stand in for real request latency). This flag is the
   * fix: it flips `true` before the adapter call and `false` in a `finally`, so a caller gating a
   * send button/spinner on `usePipelineChat`'s returned `isLoadingConversation` (as baseline's
   * `ChatBox.jsx` did) actually sees it flip during the round-trip.
   */
  readonly isLoadingConversation: boolean;
}

function stampParticipantId(
  messages: readonly ChatHistoryMessage[],
  resolvedParticipant: Participant,
): readonly ChatHistoryMessage[] {
  return messages.map((msg) => ({
    ...msg,
    participant_id: msg.role === ROLES.User ? msg.participant_id : resolvedParticipant.id,
  }));
}

function buildCreatedConversationPayload(
  result: CreateConversationAdapterResult,
  resolvedParticipant: Participant,
  pipelineVersionDetails: ChatPipelineVersionDetails | undefined,
  projectId: string | undefined,
  messageData: SendMessageData,
): Readonly<Record<string, unknown>> {
  const { userInput, question_id, eventPayload } = messageData;
  const { attachments_info, mcp_tokens, ignored_mcp_servers } = eventPayload ?? {};
  return {
    user_input: userInput,
    llm_settings: eventPayload?.llm_settings ?? buildLlmSettingsFallback(pipelineVersionDetails, projectId),
    project_id: projectId,
    conversation_uuid: result.data?.uuid,
    question_id,
    participant_id: resolvedParticipant.id,
    attachments_info,
    mcp_tokens,
    ignored_mcp_servers,
  };
}

function applyCreatedConversationResult(
  result: CreateConversationAdapterResult,
  pipelineParticipant: Participant,
  pipelineVersionDetails: ChatPipelineVersionDetails | undefined,
  projectId: string | undefined,
  messageData: SendMessageData,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
): SendResult {
  // `result.data` is guaranteed defined by the caller's guard before this function runs.
  const createdConversation: ChatConversation = {
    ...result.data,
    chat_history: [],
    isPipelineChat: true,
    participants: result.data?.participants ?? [],
  };
  setActiveConversation(createdConversation);

  const resolvedFromBackend = result.data?.participants?.find((p) => p.entityName === 'application');
  if (resolvedFromBackend) setActiveParticipant(resolvedFromBackend);
  const resolvedParticipant = resolvedFromBackend ?? pipelineParticipant;

  return {
    success: true,
    updatedEventPayload: buildCreatedConversationPayload(result, resolvedParticipant, pipelineVersionDetails, projectId, messageData),
    createdConversation,
    activeParticipant: resolvedParticipant,
    updatedMessages: stampParticipantId(messageData.newMessages ?? [], resolvedParticipant),
  };
}

async function createConversationOnFirstMessage(
  params: UsePipelineChatMessagingParams,
  messageData: SendMessageData,
): Promise<SendResult> {
  const { pipelineName, pipelineParticipant, pipelineVersionDetails, projectId, source, adapter, setActiveConversation, setActiveParticipant, onError } =
    params;

  if (!pipelineParticipant) {
    onError?.('Failed to create conversation');
    return { success: false };
  }

  try {
    const result = await adapter.createConversation({
      is_private: true,
      name: clipToLimit(`Chat with ${pipelineName ?? ''}`, NAME_MAX_LENGTH),
      source,
      meta: { single_participant: pipelineParticipant, internal_tools: pipelineVersionDetails?.meta?.internal_tools },
      participants: [pipelineParticipant],
      projectId,
    });

    if (!result.data) {
      onError?.('Failed to create conversation');
      return { success: false };
    }

    return applyCreatedConversationResult(
      result,
      pipelineParticipant,
      pipelineVersionDetails,
      projectId,
      messageData,
      setActiveConversation,
      setActiveParticipant,
    );
  } catch (caught) {
    onError?.(pipelineErrorMessage(caught));
    return { success: false };
  }
}

function sendToExistingConversation(
  pipelineVersionDetails: ChatPipelineVersionDetails | undefined,
  projectId: string | undefined,
  messageData: SendMessageData,
): SendResult {
  const { eventPayload } = messageData;
  if (eventPayload?.llm_settings?.['model_name']) return { success: true };

  return {
    success: true,
    updatedEventPayload: {
      ...eventPayload,
      llm_settings: eventPayload?.llm_settings ?? buildLlmSettingsFallback(pipelineVersionDetails, projectId),
    },
  };
}

export function usePipelineChatMessaging(params: UsePipelineChatMessagingParams): UsePipelineChatMessagingResult {
  const { pipelineVersionDetails, projectId, activeConversationId } = params;
  const [isLoadingConversation, setIsLoadingConversation] = useState(false);

  const onSend = useCallback(
    async (messageData: SendMessageData): Promise<SendResult> => {
      if (messageData.needsConversationCreation && !activeConversationId) {
        setIsLoadingConversation(true);
        try {
          return await createConversationOnFirstMessage(params, messageData);
        } finally {
          setIsLoadingConversation(false);
        }
      }
      return sendToExistingConversation(pipelineVersionDetails, projectId, messageData);
    },
    // `params` is a plain object rebuilt every render by the caller (usePipelineChat) from its own
    // already-memoised pieces, so referential identity tracks real changes for this dep array.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [activeConversationId, pipelineVersionDetails, projectId, params],
  );

  return { onSend, isLoadingConversation };
}
