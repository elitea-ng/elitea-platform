/**
 * Durable chat transport: admit one execution and fold its replayed frames.
 *
 * Run starters own admission and its failure classification. The connection
 * hook owns reconnects to the same execution. This hook owns conversation
 * identity, generation fences, history dispatch, and final-result observation.
 *
 * A conversation switch closes its observer. A delayed admission cannot
 * subscribe to another conversation's transcript. Reconnects do not admit
 * another execution. Stop requests cancellation and retains observation until
 * the authoritative terminal outcome arrives.
 *
 * Reload observation resets the seeded turn with its first accepted frame.
 * Success progress retains the observer until the generation-bound result.
 */
import { useCallback, useEffect, useMemo, useRef } from "react";

import {
  conversationDetails,
  type ContinueAgentExecutionParams,
  type AgentExecutionStart,
  type StopChatTaskParams,
  stopChatTask,
} from "@/entities/conversation/api/conversationApi";
import type { ExecutionEventData } from "@/shared/api/sse";
import { EliteaApiError } from "@/shared/api/generated/mutator";
import { t } from "@/shared/i18n";

import {
  recordStreamFailure,
  runtimeFailureReason,
} from "../lib/chatStreamSettle";
import {
  applyChatStreamFrame,
  type ChatStreamContext,
} from "../lib/chatStreamReducer";
import { isRootContextFrame } from "../lib/chatStreamContextFrames";
import { isChatStreamFrame } from "../lib/chatStreamFrame";
import { shouldForwardAgentEvent } from "../lib/agentGraphEvents";
import { isObserverTerminalFrame } from "../lib/chatStreamTurnEnd";
import { resultReference, settleFinalResultObserver } from '../lib/chatStreamFinalResult';
import { completeReferencedChatResult } from './completeReferencedChatResult';
import { resetTurnForReplay } from "../lib/chatStreamReattach";

import { useChatStreamConnection } from "./useChatStreamConnection";
import { usePendingReplay, useReattach } from "./usePendingReplay";
import {
  nonEmptyString,
  useChatStreamRunStarters,
} from "./useChatStreamRunStarters";

import { convertMessagesToChatHistory } from "../lib/convertMessagesToChatHistory";
import type { MessageGroupWire, MessageParticipantWire } from "@/entities/message/lib/wire";

import type { UseChatStreamTransportParams, UseChatStreamTransportResult } from './useChatStreamTransport.types';
export type { UseChatStreamTransportParams, UseChatStreamTransportResult } from './useChatStreamTransport.types';

export function useChatStreamTransport(
  params: UseChatStreamTransportParams,
): UseChatStreamTransportResult {
  const {
    setChatHistory,
    conversationUuid,
    context,
    onAgentEvent,
    onStreamError,
  } = params;

  // Read through a ref so a changing context does not re-open the stream:
  // `useExecutionEventStream` keys its connection on the URL and the handler
  // identities, and a reconnect mid-answer would replay the run from the
  // cursor and duplicate what is already on screen.
  const onContextChangedRef = useRef(params.onContextChanged);
  onContextChangedRef.current = params.onContextChanged;
  const contextProjectRef = useRef<string | number | undefined>(undefined);
  const refreshContext = useCallback(() => {
    if (contextProjectRef.current !== undefined) onContextChangedRef.current?.(contextProjectRef.current);
  }, []);
  const contextRef = useRef<ChatStreamContext | undefined>(context);
  contextRef.current = context;
  const onAgentEventRef = useRef(onAgentEvent);
  onAgentEventRef.current = onAgentEvent;
  const onStreamErrorRef = useRef(onStreamError);
  onStreamErrorRef.current = onStreamError;
  // The conversation on screen NOW, and the one the open stream was started
  // for. `start` is async and resolves outside React's render, so the current
  // value has to be readable there rather than closed over (#328).
  const activeConversationRef = useRef<string | undefined>(conversationUuid);
  activeConversationRef.current = conversationUuid;
  const ownerRef = useRef<string | undefined>(undefined);

  /** What Stop has to cancel server-side, from the start endpoint's answer. */
  const cancelRef = useRef<StopChatTaskParams | null>(null);
  const stopRequestRef = useRef<StopChatTaskParams | null>(null);
  /**
   * The user-message identity from the request that started this run.
   *
   * Main knows this identity at admission, but not every durable node event
   * repeats `question_id`. Without retaining it here, a response rendered from
   * those live frames has no link back to its question until the page reloads
   * it from persisted history. Regenerate then falls through to the legacy
   * request with an empty question and Main correctly rejects it. The request
   * is the authoritative turn boundary, so it supplies only the value an
   * individual frame omitted; an explicit frame value still wins.
   */
  const questionIdRef = useRef<string | undefined>(undefined);
  const generationRef = useRef<string | undefined>(undefined);
  const runEpochRef = useRef(0);
  const hydrationEpochRef = useRef<number | undefined>(undefined);

  /** The seeded message a reattach replays into, until its first frame (#6654). */
  const pendingReplay = usePendingReplay();
  const { take: takePendingReplay, clear: clearPendingReplay } = pendingReplay;

  /**
   * The connection's own `close`, held in a ref because the two sides need
   * each other: `useChatStreamConnection` is handed the frame handlers below,
   * and those handlers end the turn by detaching — which closes the
   * connection. One of the two references has to be late, and this is it. The
   * no-op initial value is never the one called: nothing can detach before a
   * run has been subscribed, and subscribing happens after this hook renders.
   */
  const closeStreamRef = useRef<() => void>(() => undefined);

  /**
   * Has this hook a run of its own? False once `detach` has run — including
   * during a pending reconnect, where nothing is subscribed but the run is
   * still this hook's to stop.
   */
  const ownsRun = useCallback(
    (): boolean => ownerRef.current !== undefined,
    [],
  );

  /** Forget the run and close its stream. Never touches chat history. */
  const detach = useCallback(() => {
    ownerRef.current = undefined;
    cancelRef.current = null;
    questionIdRef.current = undefined;
    generationRef.current = undefined;
    clearPendingReplay();
    closeStreamRef.current();
  }, [clearPendingReplay]);

  const onNodeEvent = useCallback(
    (frame: ExecutionEventData) => {
      // A frame with no `type` names no case; the reducer would return the
      // same array, but the forward below would still fire on it.
      if (!ownsRun() || !isChatStreamFrame(frame)) return;
      if (activeConversationRef.current !== undefined && activeConversationRef.current !== ownerRef.current) return;
      const generation = nonEmptyString(frame.execution_generation);
      if (generation && generationRef.current && generation !== generationRef.current) return;
      const responseId = cancelRef.current?.messageGroupUuid;
      if ((generation || generationRef.current) && responseId && frame.message_id !== responseId) return;
      const frameQuestionId = nonEmptyString(frame.question_id);
      if ((generation || generationRef.current) && frameQuestionId && questionIdRef.current && frameQuestionId !== questionIdRef.current) return;
      if (generation) generationRef.current = generation;
      // Preserve the admitted question link when a durable frame omits it.
      const identifiedFrame = {
        ...frame,
        question_id: frameQuestionId ?? questionIdRef.current,
        execution_generation: generation ?? generationRef.current,
      };
      const replayInto = takePendingReplay();
      setChatHistory((prev) => {
        const replayed = replayInto === undefined ? prev : resetTurnForReplay(prev, replayInto);
        // Bind assembly results that arrive without agent_start to the admitted generation.
        const owned = generation && replayed.some((message) => message.id === responseId && message.executionGeneration !== generation)
          ? replayed.map((message) => message.id === responseId ? { ...message, executionGeneration: generation } : message) : replayed;
        return applyChatStreamFrame(owned, identifiedFrame, contextRef.current ?? {});
      });
      // Success progress precedes full_message; the result releases the durable observer.
      const terminal = isObserverTerminalFrame(identifiedFrame, generationRef.current);
      if (terminal && (identifiedFrame.content === null || identifiedFrame.content === undefined) && resultReference(identifiedFrame)) {
        const target = cancelRef.current, owner = ownerRef.current, currentGeneration = generationRef.current, epoch = runEpochRef.current;
        if (hydrationEpochRef.current === epoch) return;
        hydrationEpochRef.current = epoch;
        const visible = () => runEpochRef.current === epoch && (activeConversationRef.current === undefined || activeConversationRef.current === owner);
        void completeReferencedChatResult({
          frame: identifiedFrame, target, conversationUuid: owner, generation: currentGeneration,
          isCurrent: () => visible() && cancelRef.current === target && ownerRef.current === owner && generationRef.current === currentGeneration,
          onResult: (result) => setChatHistory((prev) => visible() ? applyChatStreamFrame(prev, result, contextRef.current ?? {}) : prev),
          onError: (reason) => onStreamErrorRef.current?.(reason),
          onSettled: () => {
            setChatHistory((prev) => visible() ? settleFinalResultObserver(prev, identifiedFrame) : prev);
            refreshContext(); detach();
          },
        });
        return;
      }
      if (isRootContextFrame(identifiedFrame) || terminal) refreshContext();
      if (terminal) detach();
      if (shouldForwardAgentEvent(identifiedFrame.type))
        onAgentEventRef.current?.(identifiedFrame);
    },
    [setChatHistory, detach, refreshContext, ownsRun, takePendingReplay],
  );

  /**
   * End the turn with a reason the user can read.
   *
   * The identity is captured BEFORE `detach`, which clears `questionIdRef` —
   * without that ordering a failure-only message would lose its link back to
   * the question it answers.
   */
  const failWith = useCallback(
    (reason: string, failureCode?: string) => {
      const streamContext = contextRef.current;
      const questionId = questionIdRef.current;
      const responseMessageId = cancelRef.current?.messageGroupUuid;
      detach();
      setChatHistory((prev) =>
        recordStreamFailure(prev, reason, streamContext, questionId, responseMessageId, failureCode),
      );
      onStreamErrorRef.current?.(reason);
    },
    [setChatHistory, detach],
  );

  const onFailed = useCallback(
    (frame: ExecutionEventData) => {
      if (!ownsRun() || (activeConversationRef.current !== undefined && activeConversationRef.current !== ownerRef.current)) return;
      // The terminal body has no turn identity. Bind it to the admitted observer
      // before detach clears that identity, including failures without progress.
      const responseId = cancelRef.current?.messageGroupUuid, generation = generationRef.current, reason = runtimeFailureReason(frame);
      if (responseId && generation) onAgentEventRef.current?.({
        ...frame, type: 'execution.failed', message_id: responseId,
        execution_generation: generation, response_metadata: {}, content: reason,
      });
      refreshContext();
      const replayInto = takePendingReplay();
      if (replayInto !== undefined) setChatHistory((prev) => resetTurnForReplay(prev, replayInto));
      failWith(reason, typeof frame['code'] === 'string' ? frame['code'] : undefined);
    },
    [failWith, refreshContext, setChatHistory, takePendingReplay, ownsRun],
  );

  // A reattach that never opened (see PendingReplay.isArmed) is given up:
  // drop ownership so the composer is released; the seeded row is untouched.
  const onNeverOpened = useCallback((): boolean => {
    if (!pendingReplay.isArmed()) return false;
    detach();
    return true;
  }, [detach, pendingReplay]);

  const connection = useChatStreamConnection({
    onNodeEvent,
    onFailed,
    // A disconnected observer cannot declare the durable execution failed.
    // Retain ownership and Stop while the connection keeps retrying.
    onConnectionInterrupted: (reason) => onStreamErrorRef.current?.(reason),
    onNeverOpened,
  });
  closeStreamRef.current = connection.close;
  const { isStreaming, open: openStream } = connection;

  // #328: the conversation on screen changed while a stream was open. The
  // stream belongs to the previous one, so it is dropped — without settling
  // any history, because the history in scope now is not the run's.
  useEffect(() => {
    if (!isStreaming) return;
    const owner = ownerRef.current;
    if (
      owner === undefined ||
      conversationUuid === undefined ||
      owner === conversationUuid
    )
      return;
    detach();
  }, [conversationUuid, isStreaming, detach]);

  /**
   * Subscribe to the stream one accepted run answered with.
   *
   * `start`, `resume` and `regenerate` share it: all three own the run from
   * this point, and all three must apply the same #328 ownership rule and the
   * same cancel binding. Returns `false` only when the answer carries no
   * stream to watch.
   */
  const subscribeToRun = useCallback(
    (
      accepted: AgentExecutionStart,
      runConversationUuid: string,
      projectId: string | number,
      questionId?: string,
    ): boolean => {
      if (!accepted.events_url) return false;
      // The user left this conversation while the POST was in flight. The run
      // EXISTS server-side now, so nothing subscribes: those frames belong to
      // a transcript that is no longer on screen, and the durable log replays
      // them when it is reopened (#328).
      const active = activeConversationRef.current;
      if (active !== undefined && active !== runConversationUuid) return true;
      ownerRef.current = runConversationUuid;
      runEpochRef.current += 1;
      contextProjectRef.current = projectId;
      refreshContext();
      questionIdRef.current = questionId;
      generationRef.current = nonEmptyString(accepted['execution_generation']);
      // `response_message_id` is what the cancel route addresses
      // (`DELETE .../task/prompt_lib/{projectID}/{responseMessageID}`). Without
      // one there is nothing to cancel and Stop can only detach.
      cancelRef.current =
        typeof accepted.response_message_id === "string" &&
        accepted.response_message_id !== ""
          ? { projectId, messageGroupUuid: accepted.response_message_id }
          : null;
      openStream(accepted.events_url);
      return true;
    },
    [openStream, refreshContext],
  );

  const attachExistingRun = useCallback<UseChatStreamTransportResult['attachExistingRun']>((target) => {
    const { run, conversationUuid, projectId } = target;
    if (activeConversationRef.current !== conversationUuid || !run.can_control || run.phase !== 'RUNNING'
      || !run.events_url || run.events_url !== `/api/v2/executions/${encodeURIComponent(String(projectId))}/${encodeURIComponent(run.execution_id)}/events`) return false;
    detach();
    return subscribeToRun({ events_url: run.events_url, execution_id: run.execution_id, response_message_id: run.response_message_id, execution_generation: run.execution_generation }, conversationUuid, projectId, run.question_id);
  }, [detach, subscribeToRun]);

  const reconcileResume = useCallback(async (
    accepted: AgentExecutionStart, params: ContinueAgentExecutionParams,
  ): Promise<string | undefined> => {
    const previousId = nonEmptyString(params.body['message_id']);
    const responseId = nonEmptyString(accepted.response_message_id);
    if (!previousId || !responseId || previousId === responseId) return undefined;
    if (activeConversationRef.current !== undefined && activeConversationRef.current !== params.conversationUuid) return undefined;
    setChatHistory((previous) => previous.map((message) => message.id === previousId
      ? { ...message, isStreaming: false, isLoading: false, hitlInterrupt: undefined, hitlInterrupts: undefined }
      : message));
    try {
      const snapshot = await conversationDetails({
        projectId: params.projectId, id: params.conversationUuid,
        messages_limit: 50, sort_order: 'desc',
      }, AbortSignal.timeout(10_000));
      if (activeConversationRef.current !== undefined && activeConversationRef.current !== params.conversationUuid) return undefined;
      const groups = snapshot['message_groups'];
      if (!Array.isArray(groups)) throw new Error('Missing continuation history');
      const messages = convertMessagesToChatHistory(groups as MessageGroupWire[], snapshot.participants as readonly MessageParticipantWire[] | undefined);
      const response = messages.find((message) => message.id === responseId);
      const review = messages.find((message) => message.id === previousId);
      const decision = messages.find((message) => message.id === response?.questionId && message.role === 'user');
      if (!review || !decision) throw new Error('Missing continuation segments');
      setChatHistory((previous) => {
        const settled = previous.map((message) => message.id === previousId
          ? { ...message, ...review, questionId: message.questionId, toolActions: review.toolActions?.length ? review.toolActions : message.toolActions }
          : message);
        return settled.some((message) => message.id === decision.id) ? settled : [...settled, decision];
      });
      // Replay starts from the beginning. Do not seed partially generated response text here.
      return decision.id;
    } catch {
      if (activeConversationRef.current === undefined || activeConversationRef.current === params.conversationUuid) {
        onStreamErrorRef.current?.('The decision was saved, but its chat history could not be refreshed. Reload the chat to view it.');
      }
      // Admission succeeded. A history-read failure must never send the decision again.
      return undefined;
    }
  }, [setChatHistory]);
  const starters = useChatStreamRunStarters(subscribeToRun, reconcileResume);

  const stop = useCallback(() => {
    if (!ownsRun()) return;
    const target = cancelRef.current;
    if (!target || stopRequestRef.current === target) return;
    stopRequestRef.current = target;
    // Stop admission is not termination. Keep the durable observer and its
    // reconnect path alive until the worker confirms the terminal outcome.
    void stopChatTask(target).catch((error: unknown) => {
      if (cancelRef.current !== target) return;
      // Completion can win the race with Stop; its terminal event still owns
      // the outcome. Other request failures remain visible and retryable.
      if (error instanceof EliteaApiError && error.failure.kind === 'http' && error.failure.status === 409) return;
      onStreamErrorRef.current?.(t('chatMessages.stream.stopFailed', 'The stop request could not be confirmed. The run may still be active. Try Stop again.'));
    }).finally(() => {
      if (stopRequestRef.current === target) stopRequestRef.current = null;
      onContextChangedRef.current?.(target.projectId);
    });
  }, [ownsRun]);

  const reattach = useReattach(ownsRun, subscribeToRun, pendingReplay);

  return useMemo(
    () => ({
      ...starters,
      isStreaming,
      attachExistingRun,
      close: detach,
      stop,
      reattach,
    }),
    [starters, isStreaming, attachExistingRun, detach, stop, reattach],
  );
}
