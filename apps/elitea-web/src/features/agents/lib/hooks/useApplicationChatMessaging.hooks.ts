import { useCallback } from 'react';

import { t } from '@/shared/i18n';
import { NAME_MAX_LENGTH, clipToLimit } from '@/shared/lib/textLimits';
import { ROLES } from '@/shared/lib/enums';
import type { Participant } from '@/entities/participant';

import { buildLlmSettingsFallback } from './applicationChat.helpers';
import type {
  ChatApplicationVersionDetails,
  ChatConversation,
  ChatConversationAdapter,
  ChatHistoryMessage,
  ChatSource,
  CreateConversationAdapterResult,
  SendMessageData,
  SendResult,
} from './applicationChat.types';

/**
 * "Send a message" slice of `useApplicationChat` —
 * `handleCreateConversationOnFirstMessage`/`handleMessage`/`onSend`. Split
 * out of `useApplicationChat.hooks.ts` purely to keep every function under
 * this codebase's `complexity`/`max-lines` gates (see that file's own
 * module doc comment for the full baseline citation and disclosed-
 * deviation list — this split changes no behaviour).
 */
export interface UseApplicationChatMessagingParams {
  readonly applicationName: string | undefined;
  readonly applicationParticipant: Participant | null;
  readonly applicationVersionDetails: ChatApplicationVersionDetails | undefined;
  readonly projectId: string | undefined;
  readonly source: ChatSource;
  readonly adapter: ChatConversationAdapter;
  readonly activeConversationId: string | number | undefined;
  readonly setActiveConversation: (c: ChatConversation) => void;
  readonly setActiveParticipant: (p: Participant) => void;
  readonly onError?: ((message: string) => void) | undefined;
}

export interface UseApplicationChatMessagingResult {
  readonly onSend: (messageData: SendMessageData) => Promise<SendResult>;
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
  applicationVersionDetails: ChatApplicationVersionDetails | undefined,
  projectId: string | undefined,
  messageData: SendMessageData,
): Readonly<Record<string, unknown>> {
  const { userInput, question_id, eventPayload } = messageData;
  const { attachments_info, mcp_tokens, ignored_mcp_servers } = eventPayload ?? {};
  return {
    user_input: userInput,
    llm_settings: eventPayload?.llm_settings ?? buildLlmSettingsFallback(applicationVersionDetails),
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
  applicationParticipant: Participant,
  applicationVersionDetails: ChatApplicationVersionDetails | undefined,
  projectId: string | undefined,
  messageData: SendMessageData,
  setActiveConversation: (c: ChatConversation) => void,
  setActiveParticipant: (p: Participant) => void,
): SendResult {
  // `result.data` is guaranteed defined by the caller's guard before this function runs.
  const createdConversation: ChatConversation = {
    ...result.data,
    chat_history: [],
    isApplicationChat: true,
    participants: result.data?.participants ?? [],
  };
  setActiveConversation(createdConversation);

  const appParticipant = result.data?.participants?.find((p) => p.entityName === 'application');
  if (appParticipant) setActiveParticipant(appParticipant);
  const resolvedParticipant = appParticipant ?? applicationParticipant;

  return {
    success: true,
    updatedEventPayload: buildCreatedConversationPayload(result, resolvedParticipant, applicationVersionDetails, projectId, messageData),
    createdConversation,
    activeParticipant: resolvedParticipant,
    updatedMessages: stampParticipantId(messageData.newMessages ?? [], resolvedParticipant),
  };
}

/** The fixed, sanitized message shown on ANY conversation-creation failure — see `createConversationOnFirstMessage`'s own doc comment for why this never varies with the underlying cause. */
function createConversationFailedMessage(): string {
  return t('features.agents.applicationChat.createConversationFailed', 'Failed to create conversation');
}

/**
 * `useApplicationChat.hooks.js:374-380`'s `handleCreateConversationOnFirstMessage` catch block.
 *
 * **Confirmed regression fix (A1-application-chat cluster, finding 3):** baseline ALWAYS calls
 * `toastError('Failed to create conversation')` here regardless of what was thrown/rejected — the
 * real error is only ever `console.error`'d, never shown to the user. This function previously
 * surfaced `applicationErrorMessage(caught)` (the raw error text) via `onError`, which degrades to
 * the literal string `"[object Object]"` for a non-`Error`, non-`string` rejection and, even for a
 * real `Error`, leaks internal error text baseline deliberately hides. Restored to baseline parity:
 * a fixed, translated message every time, with `caught` logged for diagnostics only.
 */
async function createConversationOnFirstMessage(
  params: UseApplicationChatMessagingParams,
  messageData: SendMessageData,
): Promise<SendResult> {
  const { applicationName, applicationParticipant, applicationVersionDetails, projectId, source, adapter, setActiveConversation, setActiveParticipant, onError } = params;

  if (!applicationParticipant) {
    onError?.(createConversationFailedMessage());
    return { success: false };
  }

  try {
    const result = await adapter.createConversation({
      is_private: true,
      name: clipToLimit(`Chat with ${applicationName ?? ''}`, NAME_MAX_LENGTH),
      source,
      meta: { single_participant: applicationParticipant, internal_tools: applicationVersionDetails?.meta?.internal_tools },
      participants: [applicationParticipant],
      projectId,
    });

    if (!result.data) {
      onError?.(createConversationFailedMessage());
      return { success: false };
    }

    return applyCreatedConversationResult(
      result,
      applicationParticipant,
      applicationVersionDetails,
      projectId,
      messageData,
      setActiveConversation,
      setActiveParticipant,
    );
  } catch (caught) {
    console.error('Failed to create conversation:', caught);
    onError?.(createConversationFailedMessage());
    return { success: false };
  }
}

function sendToExistingConversation(
  applicationVersionDetails: ChatApplicationVersionDetails | undefined,
  messageData: SendMessageData,
): SendResult {
  const { eventPayload } = messageData;
  if (eventPayload?.llm_settings?.['model_name']) return { success: true };

  return {
    success: true,
    updatedEventPayload: {
      ...eventPayload,
      llm_settings: eventPayload?.llm_settings ?? buildLlmSettingsFallback(applicationVersionDetails),
    },
  };
}

export function useApplicationChatMessaging(params: UseApplicationChatMessagingParams): UseApplicationChatMessagingResult {
  const { applicationVersionDetails, activeConversationId } = params;

  const onSend = useCallback(
    async (messageData: SendMessageData): Promise<SendResult> => {
      if (messageData.needsConversationCreation && !activeConversationId) {
        return createConversationOnFirstMessage(params, messageData);
      }
      return sendToExistingConversation(applicationVersionDetails, messageData);
    },
    // `params` is a plain object rebuilt every render by the caller (useApplicationChat) from its
    // own already-memoised pieces, so referential identity tracks real changes for this dep array.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [activeConversationId, applicationVersionDetails, params],
  );

  return { onSend };
}
