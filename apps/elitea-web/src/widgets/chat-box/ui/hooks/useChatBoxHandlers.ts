/**
 * Action handlers for ChatBox — send, regenerate, copy, delete, HITL resume,
 * MCP continuation, token-limit continuation. Port of `ChatBox.jsx`'s
 * `onPredictStream`/`onCopyToClipboard`/`onRegenerateAnswer`/
 * `continueMcpExecution`/`onContinueTokenLimitExecution`/`onHitlResume`/
 * `onClickClearChat` (lines ~530-1508), as closures over caller-injected deps.
 * Dependency-injection types + pure helpers live in `./useChatBoxHandlers.helpers`
 * (split out to stay under the §3.5 file-length budget).
 *
 * DISCLOSED GAPS vs. baseline: payloads carry only fields this hook's deps
 * can supply — baseline's `generateMessagePayload`/
 * `generateApplicationStreamingPayload`/`getRegeneratePayload` still have
 * participant-specific gaps. `continueHitl` decides ONE card
 * (`decideHitlCard`): the card is queued, and the response's whole pending set
 * resumes as one continuation once its last card is decided — today's route
 * consumes the complete set. A legacy fan-out child still resumes alone.
 *
 * CONTINUATION TRANSPORT. `continueHitl` and `continueTokenLimit` resume over REST first — `POST
 * /api/v2/elitea_core/continue_predict/prompt_lib/{projectID}/{conversationID}`
 * with `execution_contract=agent.continue.hitl.v1` — and emits
 * `chat_continue_predict` only when no route answered at all. The order
 * matters: the socket client is a no-op stub whenever `vite_socket_server` is
 * empty. That is what the shipped deployment serves. So the socket alone left
 * every approval paused server-side. It never emits after a route that
 * ACCEPTED the resume; that would run the agent twice.
 *
 * Delegated authorization uses its exact REST contract. It never falls back
 * to the socket because that payload cannot identify the paused invocation.
 * Authorization retains browser tokens and batches exact parallel decisions.
 */

import { conversationApi } from "@/entities/conversation";
import type { ChatMessage } from "@/features/chat-messages";

import {
  buildChatContinuePayload,
  extractCopyableContent,
  findQuestionText,
  revertContinuation,
  tryEmit,
} from "./useChatBoxHandlers.helpers";
import { decideHitlCard } from "./useChatBoxHandlers.hitl";
import { createResumeMcpFlow } from "./useChatBoxHandlers.mcpAuth";
import type {
  ChatBoxHandlerDeps,
  HitlInterruptAction,
  UseChatBoxHandlersResult,
} from "./useChatBoxHandlers.helpers";
import {
  createClearChat,
  createDeleteAnswer,
  createSendQuestion,
  undeliveredText,
} from "./useChatBoxHandlers.turns";
import { createRegenerateAnswer } from "./useChatBoxHandlers.regenerate";

/*
 * [#71] `UpdatedMessageItem` and `UploadedAttachmentOutcome` were dropped from
 * this re-export list. The issue's triage flagged `UploadedAttachmentOutcome`
 * as appearing twice — an `interface` in `useChatBoxHandlers.helpers.ts:37` and
 * a `type` here — and asked whether the two shapes agree before deleting
 * either. They are not two shapes: there is one declaration, in the helpers
 * module, and this block merely re-exported it, which is why knip listed it
 * under both files. Nothing to reconcile, no merge artifact.
 *
 * `UploadedAttachmentOutcome` is internal to the helpers module (it only names
 * the element type of `ChatBoxHandlerDeps.uploadAttachments`'s resolved value,
 * and a caller builds that object literal structurally), so it is no longer
 * exported there either. `UpdatedMessageItem` is still exported by the helpers
 * module — `buildRegeneratePayload` takes it — it just has no consumer for the
 * re-export.
 */
export type {
  ChatBoxHandlerDeps,
  HitlInterruptAction,
  SendQuestionParams,
  SendResult,
  UseChatBoxHandlersResult,
} from "./useChatBoxHandlers.helpers";

/** Creates a bundle of imperative action handlers for the ChatBox, each a closure over caller-injected `deps`. */
export function useChatBoxHandlers(
  deps: ChatBoxHandlerDeps,
): UseChatBoxHandlersResult {
  const {
    emitSocket,
    setChatHistory,
    setStreamingInfo,
  } = deps;
  const sendQuestion = createSendQuestion(deps);
  const copyToClipboard = async (message: ChatMessage): Promise<boolean> => {
    const content = message.exception
      ? JSON.stringify(message.exception)
      : extractCopyableContent(message);
    if (!content) return false;
    try {
      await navigator.clipboard.writeText(content);
      return true;
    } catch {
      return false;
    }
  };
  const regenerateAnswer = createRegenerateAnswer(deps);
  const deleteAnswer = createDeleteAnswer(deps);
  const clearChat = createClearChat(deps);
  // One card at a time; the set goes out when its last card is decided.
  const continueHitl = (action: HitlInterruptAction): Promise<void> => decideHitlCard(deps, action);
  const resumeMcpFlow = createResumeMcpFlow(deps);
  const continueTokenLimit = async (messageId: string): Promise<void> => {
    const message = deps.chatHistory.find((item) => item.id === messageId);
    if (!message) return;
    setChatHistory((prev) =>
      prev.map((msg) =>
        msg.id !== messageId
          ? msg
          : { ...msg, isLoading: true, isStreaming: true },
      ),
    );
    setStreamingInfo(message.questionId ?? messageId);
    const projectId = Number(deps.projectId);
    if (
      deps.continueStreamedExecution &&
      deps.conversationUuid &&
      Number.isSafeInteger(projectId) &&
      projectId > 0
    ) {
      const outcome = await deps.continueStreamedExecution({
        conversationUuid: deps.conversationUuid,
        contract: conversationApi.contracts.continueOutputLimit,
        body: {
          project_id: projectId,
          conversation_uuid: deps.conversationUuid,
          message_id: messageId,
        },
      });
      if (outcome.started) return;
      if (outcome.reason === "rejected") {
        revertContinuation(setChatHistory, message, outcome.message);
        return;
      }
    }
    const question = findQuestionText(deps.chatHistory, message) ?? "Continue";
    const payload = {
      ...buildChatContinuePayload(deps, {
        messageId,
        threadId: message.threadId,
        question,
      }),
      token_limit_continuation: true,
    };
    if (
      !tryEmit(
        () => emitSocket("chat_continue_predict", payload),
        "continueTokenLimit",
      )
    ) {
      revertContinuation(setChatHistory, message, undeliveredText());
    }
  };
  return {
    sendQuestion,
    copyToClipboard,
    regenerateAnswer,
    deleteAnswer,
    clearChat,
    continueHitl,
    resumeMcpFlow,
    continueTokenLimit,
  };
}
