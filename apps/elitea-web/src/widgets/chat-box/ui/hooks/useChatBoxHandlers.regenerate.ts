/**
 * useChatBoxHandlers.regenerate.ts — regenerating ONE persisted answer, and the
 * one server refusal that is worth repeating.
 *
 * Split out of `useChatBoxHandlers.turns.ts` to keep that file inside the §3.5
 * file-length budget of 400 lines — the same move
 * `features/chat-messages/model/useChatStreamRunStarters.ts` records in its own
 * header. The seam is one family: the regenerate click, the two transports it
 * can reach, and the bounded retry that sits between them.
 */
import { conversationApi } from "@/entities/conversation";
import type { ChatMessage } from "@/features/chat-messages";
import { EliteaApiError } from "@/shared/api/generated/mutator";
import { t } from "@/shared/i18n";

import {
  buildRegeneratePayload,
  findQuestionForAnswer,
  maybeSetStreamingInfo,
  regeneratingPatch,
} from "./useChatBoxHandlers.helpers";
import type {
  ChatBoxHandlerDeps,
  StreamStartOutcome,
  UpdatedMessageItem,
} from "./useChatBoxHandlers.helpers";

/**
 * Put the answer back the way it was before the optimistic regeneration patch,
 * and — when a `reason` is given — say on the card why the regeneration did not
 * happen.
 *
 * The reason rides on `exception` because that is this app's answer-side
 * failure surface: `ApplicationAnswer` renders `ErrorTrace` for an assistant
 * message that carries one, which is the same mechanism
 * `buildFailedTurnMessage` and `revertContinuation` already use. elitea-web has
 * no global toast/snackbar host — every Snackbar in the tree is local to one
 * form — so a "toast" here would mean a new provider mounted above `ChatBox`
 * for a single message, and it would appear detached from the answer it is
 * about.
 */
function restoreAnswer(
  deps: ChatBoxHandlerDeps,
  messageId: string,
  answer: ChatMessage | undefined,
  reason?: string,
): void {
  if (!answer) return;
  const restored = reason === undefined ? answer : { ...answer, exception: reason };
  deps.setChatHistory((prev) =>
    prev.map((item) => (item.id !== messageId ? item : restored)),
  );
}

/** Shown on the answer card when the previous run never released it inside the retry budget. */
export const regenerationStillFinalizingText = (): string =>
  t(
    "widgets.chatBox.regenerationStillFinalizing",
    "The previous answer is still being finalized. Wait a moment and press Regenerate again.",
  );

/**
 * Ask the streamed regeneration route, repeating ONLY while it reports
 * `retry-later`.
 *
 * WHY THE RETRY IS HERE AND NOT ON THE REST FALLBACK BELOW. The 409
 * `agent_regeneration_pending` exists on ONE route: the contract route this
 * function calls (`?execution_contract=agent.regenerate.v1`). The legacy
 * trigger `triggerRegenerateWithRetry` drives posts no contract at all —
 * `buildRegeneratePayload` has no such field — and the Go route answers 400
 * before admission ever runs, so that call can never observe this refusal. A
 * retry attached there would loop on a different error and leave the real one
 * exactly as swallowed as before.
 *
 * The budget is bounded by `REGENERATE_RETRY_DELAYS_MS`: the flag clears
 * milliseconds-to-about-a-second after the answer text lands, so a short fixed
 * ladder covers the window without turning a genuinely stuck turn into a
 * spinner that never resolves.
 */
async function regenerateStreamedWithRetry(
  attempt: NonNullable<ChatBoxHandlerDeps["regenerateStreamedExecution"]>,
  input: {
    readonly messageId: string;
    readonly questionId: string;
    readonly question: string;
    readonly updatedItems?: readonly UpdatedMessageItem[] | undefined;
    readonly participantId?: string | undefined;
  },
): Promise<StreamStartOutcome> {
  for (let index = 0; ; index += 1) {
    // eslint-disable-next-line no-await-in-loop -- a retry is sequential by definition
    const outcome = await attempt(input);
    if (outcome.started || outcome.reason !== "retry-later") return outcome;
    const delay = REGENERATE_RETRY_DELAYS_MS[index];
    if (delay === undefined) return outcome;
    // eslint-disable-next-line no-await-in-loop -- ditto
    await sleep(delay);
  }
}

/**
 * Backoff before each retry of a still-finalizing regeneration.
 *
 * `POST /elitea_core/regenerate/...` answers 409 `agent_regeneration_pending`
 * (`retryable: true`) while `chat_message_group.is_streaming` is still TRUE —
 * `ResolveCurrentRegeneration` (services/elitea-main) refuses on that flag,
 * which clears only once the previous answer text is written. That window is
 * short but the composer is already released and the Regenerate button
 * clickable, so a plain restore+warn makes the click silently no-op. These
 * delays (a few attempts across ~2s, matching the window; the server sends
 * `Retry-After: 1`) let the click land instead, without reading `is_streaming`
 * at the click site.
 */
const REGENERATE_RETRY_DELAYS_MS: readonly number[] = [400, 700, 1000];

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => {
    setTimeout(resolve, ms);
  });

/**
 * True for the transient still-finalizing refusal — an `EliteaApiError` HTTP
 * failure whose parsed body carries the `agent_regeneration_pending` code and
 * `retryable: true`. Read off `error.failure` the same way every other consumer
 * branches on it (`kind: 'http'` is the only failure that carries a `body`).
 */
function isRetryableRegenerationPending(error: unknown): boolean {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== "http") {
    return false;
  }
  const body = error.failure.body;
  if (typeof body !== "object" || body === null) return false;
  const record = body as Readonly<Record<string, unknown>>;
  return (
    record.error === "agent_regeneration_pending" && record.retryable === true
  );
}

/**
 * Drive the legacy REST regenerate, auto-retrying the still-finalizing 409
 * across its short window before falling back to today's restore+warn. A
 * non-retryable error, or a persistent 409 that exhausts the budget, restores
 * the previous answer.
 */
async function triggerRegenerateWithRetry(
  deps: ChatBoxHandlerDeps,
  messageId: string,
  previousAnswer: ChatMessage | undefined,
  payload: Parameters<typeof conversationApi.regenerate>[0],
): Promise<void> {
  const trigger = deps.triggerRegenerate;
  if (!trigger) return;
  for (
    let attempt = 0;
    attempt <= REGENERATE_RETRY_DELAYS_MS.length;
    attempt += 1
  ) {
    try {
      await trigger(payload);
      return;
    } catch (error) {
      const delay = REGENERATE_RETRY_DELAYS_MS[attempt];
      if (isRetryableRegenerationPending(error) && delay !== undefined) {
        await sleep(delay);
        continue;
      }
      console.warn("[useChatBoxHandlers] regenerate failed:", error);
      restoreAnswer(deps, messageId, previousAnswer);
      return;
    }
  }
}

export function createRegenerateAnswer(
  deps: ChatBoxHandlerDeps,
): (
  messageId: string,
  updatedItems?: readonly UpdatedMessageItem[],
) => Promise<void> {
  return async (messageId, updatedItems) => {
    if (!deps.regenerateStreamedExecution && !deps.triggerRegenerate) {
      console.warn(
        "[useChatBoxHandlers] regenerateAnswer: no regeneration transport provided",
      );
      return;
    }
    const answer = deps.chatHistory.find((item) => item.id === messageId);
    const questionMessage = findQuestionForAnswer(deps.chatHistory, answer);
    let previousAnswer: ChatMessage | undefined;
    deps.setChatHistory((prev) => {
      previousAnswer = prev.find((item) => item.id === messageId);
      return prev.map((item) =>
        item.id !== messageId ? item : regeneratingPatch(item),
      );
    });
    maybeSetStreamingInfo(deps.setStreamingInfo, questionMessage?.id);
    if (deps.regenerateStreamedExecution && questionMessage) {
      const outcome = await regenerateStreamedWithRetry(
        deps.regenerateStreamedExecution,
        {
          messageId,
          questionId: questionMessage.id,
          question: questionMessage.content,
          ...(updatedItems !== undefined ? { updatedItems } : {}),
          // The agent the question went to answers it again (ADR-0050 D7c).
          ...(questionMessage.participantId !== undefined ? { participantId: questionMessage.participantId } : {}),
        },
      );
      if (outcome.started) return;
      if (outcome.reason === "retry-later") {
        // The retry budget is spent and the previous run still owns this
        // answer. Falling through to the REST trigger would post the same
        // regeneration without an `execution_contract` and collect a 400, so
        // the user would be told the wrong thing — or, as before, nothing.
        restoreAnswer(
          deps,
          messageId,
          previousAnswer,
          regenerationStillFinalizingText(),
        );
        return;
      }
    }
    if (!deps.triggerRegenerate) {
      restoreAnswer(deps, messageId, previousAnswer);
      return;
    }
    const payload = buildRegeneratePayload(
      deps,
      messageId,
      questionMessage,
      updatedItems,
    ) as Parameters<typeof conversationApi.regenerate>[0];
    await triggerRegenerateWithRetry(deps, messageId, previousAnswer, payload);
  };
}
