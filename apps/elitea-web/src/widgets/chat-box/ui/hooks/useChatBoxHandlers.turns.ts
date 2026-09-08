/** Focused factories for ordinary ChatBox turn actions. */
import type { ChatMessage } from "@/features/chat-messages";
import { t } from "@/shared/i18n";
import { ROLES } from "@/shared/lib/enums";

import {
  buildDefaultMessagePayload,
  buildFailedTurnMessage,
  buildOptimisticUserMessage,
  buildSendResult,
  findActionRequiredToolAction,
  NO_STREAM_TRANSPORT,
  readServerUrl,
  resolveConversationForSend,
  resolveParticipantId,
  resolveUploadConversationId,
  toProjectIdString,
  trackMcpAuthDecision,
  tryEmit,
  uploadPendingAttachments,
  UPLOAD_FAILED,
} from "./useChatBoxHandlers.helpers";
import type {
  ChatBoxHandlerDeps,
  SendQuestionParams,
  SendResult,
} from "./useChatBoxHandlers.helpers";

/**
 * The history a turn NO TRANSPORT ACCEPTED leaves behind.
 *
 * THE QUESTION STAYS. `sendQuestion` has already cleared the composer by the
 * time this runs, so the optimistic bubble is the only copy of what the person
 * typed, and `buildFailedTurnMessage` anchors the reason to it through
 * `questionId` — an error on its own no longer says which message failed.
 * Dropping it unconditionally is what made journeys 8, 9 and 12 read an empty
 * `chat-message-list`: the E2E stack serves `vite_socket_server: ""` and has no
 * runtime plane, so EVERY turn there reaches this branch.
 *
 * It is dropped in exactly one case: another user message already carries the
 * same text. That is what re-sending a question the server persisted before the
 * transport dropped produces — the refusal ("a previous agent turn is still
 * being recovered") arrives with the original already on screen, and a second
 * identical bubble is noise rather than information.
 *
 * A previous failure for the SAME question is always replaced, so a retry that
 * reuses `questionId` does not stack error bubbles.
 */
function historyAfterFailedTurn(
  previous: readonly ChatMessage[],
  questionId: string,
  question: string,
  failure: string,
): readonly ChatMessage[] {
  const alreadyOnScreen = previous.some(
    (message) =>
      message.id !== questionId &&
      message.role === ROLES.User &&
      message.content === question,
  );
  return [
    ...previous.filter(
      (message) =>
        message.id !== `${questionId}-error` &&
        !(alreadyOnScreen && message.id === questionId),
    ),
    buildFailedTurnMessage(questionId, failure),
  ];
}

export const undeliveredText = (): string =>
  t(
    "widgets.chatBox.turnNotDelivered",
    "The message was not sent: no chat connection is available. Reload the page and try again.",
  );

async function deliverTurn(
  deps: ChatBoxHandlerDeps,
  params: {
    readonly conversationUuid: string;
    readonly payload: Record<string, unknown>;
  },
): Promise<string | undefined> {
  const outcome = deps.startStreamedExecution
    ? await deps.startStreamedExecution(params)
    : NO_STREAM_TRANSPORT;
  if (outcome.started) return undefined;
  if (outcome.reason === "rejected") return outcome.message;
  const emitted = tryEmit(
    () =>
      deps.emitSocket("chat_predict", {
        ...params.payload,
        conversation_uuid: params.conversationUuid,
        project_id: toProjectIdString(deps.projectId),
      }),
    "chat_predict",
  );
  return emitted ? undefined : undeliveredText();
}

export function createSendQuestion(
  deps: ChatBoxHandlerDeps,
): (params: SendQuestionParams) => Promise<SendResult> {
  return async ({ question, attachments, isSendingToUser, userIds }) => {
    if (!question.trim()) return { success: false };
    const lastMessage = deps.chatHistory[deps.chatHistory.length - 1];
    const pendingAuth = findActionRequiredToolAction(lastMessage);
    trackMcpAuthDecision(
      deps.sessionDeclinedMcpServersRef,
      pendingAuth,
      readServerUrl(pendingAuth),
      true,
    );
    const questionId = crypto.randomUUID();
    const participant = deps.getActiveParticipant?.() ?? null;
    const userParticipant = deps.getUserParticipant?.();
    const participantId = resolveParticipantId(participant);
    const { uuid: resolvedConversationUuid, createdConversation } =
      await resolveConversationForSend(deps, question);
    // The conversation's UUID, not its numeric id — see
    // `resolveUploadConversationId`: the object key it becomes is what
    // admission authorises the attachment against.
    const uploadConversationId = resolveUploadConversationId(
      createdConversation,
      deps.conversationUuid,
    );
    const attachmentList = await uploadPendingAttachments(
      deps,
      attachments,
      uploadConversationId,
    );
    // The conversation is already committed on the server by this point, so it
    // is announced even though this send is being abandoned (see `SendResult`).
    if (attachmentList === UPLOAD_FAILED) return buildSendResult(createdConversation, false);
    const payload = (
      deps.generateMessagePayload ?? buildDefaultMessagePayload
    )({
      question,
      questionId,
      participant,
      conversationUuid: resolvedConversationUuid,
      attachmentList,
      isSendingToUser,
      userIds,
    });
    deps.setChatHistory((prev) => [
      ...prev,
      buildOptimisticUserMessage(
        questionId,
        question,
        userParticipant,
        participantId,
      ),
    ]);
    if (!deps.isStreamingNow) deps.setStreamingInfo(questionId);
    const failure = resolvedConversationUuid
      ? await deliverTurn(deps, {
          conversationUuid: resolvedConversationUuid,
          payload,
        })
      : t(
          "widgets.chatBox.conversationNotCreated",
          "The message was not sent: this chat could not be created.",
        );
    if (failure !== undefined) {
      deps.setChatHistory((prev) =>
        historyAfterFailedTurn(prev, questionId, question, failure),
      );
      /*
       * DEFECT this closes. `createdConversation` used to be dropped here, and
       * this branch is not rare: every deployment without a live transport
       * lands in it (`historyAfterFailedTurn`'s own note — the E2E stack serves
       * `VITE_SOCKET_SERVER: ""`), as does a turn the runtime plane refuses.
       *
       * The row is NOT hypothetical at that point. `resolveConversationForSend`
       * has already POSTed it and the server answered 201. Dropping it meant
       * the app forgot a conversation it had just created: `onConversationCreated`
       * never ran, so the route stayed on `/chat` and the rail's cached listing
       * was never invalidated. The conversation existed in the database and on
       * no screen — a reload was the only way to find it.
       *
       * `success: false` still says the TURN failed, and the failure bubble is
       * on screen; announcing the row does not contradict that.
       */
      return buildSendResult(createdConversation, false);
    }
    return buildSendResult(createdConversation);
  };
}

export function createDeleteAnswer(
  deps: ChatBoxHandlerDeps,
): (messageId: string) => Promise<void> {
  return async (messageId) => {
    if (!deps.triggerDeleteMessage) {
      console.warn(
        "[useChatBoxHandlers] deleteAnswer: triggerDeleteMessage not provided",
      );
      return;
    }
    const conversationId =
      deps.conversationUuid ??
      (deps.conversationId !== undefined ? String(deps.conversationId) : "");
    try {
      const result = await deps.triggerDeleteMessage({
        projectId: toProjectIdString(deps.projectId),
        id: messageId,
        conversationId,
      });
      const removed = new Set<string>(
        result?.deleted && result.deleted.length > 0
          ? result.deleted.map(String)
          : [String(messageId)],
      );
      deps.setChatHistory((prev) =>
        prev.filter((item) => !removed.has(String(item.id))),
      );
    } catch (error) {
      console.warn("[useChatBoxHandlers] deleteAnswer failed:", error);
    }
  };
}

export function createClearChat(
  deps: ChatBoxHandlerDeps,
): () => Promise<void> {
  return async () => {
    if (!deps.triggerDeleteAllMessages) {
      console.warn(
        "[useChatBoxHandlers] clearChat: triggerDeleteAllMessages not provided",
      );
      return;
    }
    const conversationId =
      deps.conversationUuid ??
      (deps.conversationId !== undefined ? String(deps.conversationId) : "");
    try {
      await deps.triggerDeleteAllMessages({
        projectId: toProjectIdString(deps.projectId),
        conversationId,
      });
      deps.setChatHistory([]);
    } catch (error) {
      console.warn("[useChatBoxHandlers] clearChat failed:", error);
    }
  };
}
