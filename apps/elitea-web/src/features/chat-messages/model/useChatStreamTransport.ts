/**
 * model/useChatStreamTransport.ts — the chat transport swap (issue #93, Surface B).
 *
 * This is the producer the reducer was written for. Until now the chat
 * surface started its run by emitting socket.io `chat_predict` and rendered
 * the answer only once `chat_message_sync` pushed the PERSISTED message group
 * — no live streaming at all, because, as `useChatBoxHandlers` said in place:
 * "the chat surface has no consumer for the streamed `execution.node_event`
 * envelope — the streaming reducer the old app fed from `chat_predict` was
 * never ported". It is ported now, so this hook closes the loop: start the run
 * over REST, subscribe to the durable replay stream, and fold each frame into
 * chat history with `applyChatStreamFrame`.
 *
 * WHAT IS LEFT HERE, AND WHAT MOVED. Two families were split out of this file
 * to keep it inside the §3.5 400-line budget — the same move
 * `lib/chatStreamSettle.ts` records in its own header:
 *
 *  - `./useChatStreamRunStarters.ts` — the three admission routes (start,
 *    resume, regenerate) and the classification of their failures. It carries
 *    the FALLBACK CONTRACT in full: the Go route requires a recognised
 *    `execution_contract`, so a failure — or a 200 with no `events_url` — is
 *    an unambiguous "this backend has not landed the SSE path", `start`
 *    answers `false`, and the caller emits `chat_predict` exactly as before.
 *  - `./useChatStreamConnection.ts` — the SSE connection lifecycle, and with
 *    it the RESUME contract of issue #329 in full: a drop reopens the SAME
 *    execution's stream at `?cursor=<last id seen>` rather than re-running
 *    anything, and the retries stop once the turn is over.
 *
 * What stays is what needs all three at once: run OWNERSHIP, frame dispatch
 * into chat history, and how a turn ends.
 *
 * IT NEVER RE-STARTS A RUN. Once the POST has succeeded the execution exists
 * server-side, so a transport failure after that point must not fall back to
 * the socket — that would run the agent twice and bill it twice. The stream is
 * REOPENED instead and, once the retry budget is spent, the spinner stops and
 * the failure is surfaced.
 *
 * STREAM OWNERSHIP (issue #328). A stream belongs to the conversation that
 * started it, and to nothing else. The hook stays mounted across a
 * conversation switch — `ChatBox` re-renders with a new `conversationUuid`
 * rather than remounting — so an open stream would otherwise keep folding its
 * frames into whatever history is mounted next, i.e. into a DIFFERENT
 * conversation's transcript. The stream is therefore closed the moment the
 * active conversation stops matching the stream's owner — and, for the one
 * ordering that closing cannot cover, a `start` whose POST resolves after the
 * user has already left subscribes to nothing at all.
 */
import { useCallback, useEffect, useMemo, useRef } from "react";

import {
  type AgentExecutionStart,
  type StopChatTaskParams,
  stopChatTask,
} from "@/entities/conversation/api/conversationApi";
import type { ExecutionEventData } from "@/shared/api/sse";

import {
  recordStreamFailure,
  runtimeFailureReason,
  settleInFlight,
} from "../lib/chatStreamSettle";
import {
  applyChatStreamFrame,
  type ChatStreamContext,
} from "../lib/chatStreamReducer";
import { isChatStreamFrame } from "../lib/chatStreamFrame";
import { shouldForwardAgentEvent } from "../lib/agentGraphEvents";
import { isTurnTerminalFrame } from "../lib/chatStreamTurnEnd";

import { useChatStreamConnection } from "./useChatStreamConnection";
import {
  nonEmptyString,
  useChatStreamRunStarters,
  type ChatStreamRunStarters,
} from "./useChatStreamRunStarters";

import type { ChatMessage } from "../lib/convertMessagesToChatHistory";

type SetChatHistory = (
  updater: (prev: readonly ChatMessage[]) => readonly ChatMessage[],
) => void;

/** @public Params for `useChatStreamTransport`. */
export interface UseChatStreamTransportParams {
  readonly setChatHistory: SetChatHistory;
  /**
   * The conversation currently on screen. A stream whose owner (the
   * `conversationUuid` its `start` was called with) stops matching this is
   * closed, and its frames are dropped rather than folded into the
   * conversation that is now mounted — issue #328. Leaving it undefined
   * disables the guard, which is only right for a caller that has exactly one
   * conversation for its whole lifetime.
   */
  readonly conversationUuid?: string | undefined;
  /** Identity for messages the reducer has to create, plus the participant roster. */
  readonly context?: ChatStreamContext | undefined;
  /**
   * Graph frames, for a surface that renders a run timeline (the pipeline
   * flow editor). Chat itself ignores them; forwarding is the caller's half of
   * the baseline's `onRcvAgentEvent`, see `agentGraphEvents.ts`.
   */
  readonly onAgentEvent?: ((frame: ExecutionEventData) => void) | undefined;
  /** The run itself failed server-side, or its stream dropped. */
  readonly onStreamError?: ((reason: string) => void) | undefined;
}

/**
 * @public
 *
 * `startDetailed`, `start`, `resume` and `regenerate` come from
 * `ChatStreamRunStarters`, which documents each one — including the boolean
 * every caller reads to decide whether a socket fallback is still safe.
 */
export interface UseChatStreamTransportResult extends ChatStreamRunStarters {
  /** Whether a stream is currently subscribed — drives the composer's Stop affordance. */
  readonly isStreaming: boolean;
  /**
   * Detach from the current stream WITHOUT cancelling the run: close the
   * connection, cancel any pending reconnect, and leave the history alone.
   *
   * This is the conversation-switch / teardown path. It does not settle the
   * spinner because the history it would settle is the one now on screen,
   * which belongs to a different conversation.
   */
  readonly close: () => void;
  /**
   * The user pressed Stop. Cancels the run SERVER-SIDE (`DELETE
   * /elitea_core/task/prompt_lib/{projectId}/{responseMessageId}` — the id the
   * start endpoint returned), settles the message, and closes the stream
   * without reconnecting.
   */
  readonly stop: () => void;
}

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
    closeStreamRef.current();
  }, []);

  const onNodeEvent = useCallback(
    (frame: ExecutionEventData) => {
      // A frame with no `type` names no case; the reducer would return the
      // same array, but the forward below would still fire on it.
      if (!isChatStreamFrame(frame)) return;
      const frameQuestionId = nonEmptyString(frame.question_id);
      // Authorization/output-limit resumes identify the existing answer and
      // need not repeat its question. Normalize null/empty wire values to
      // absence even without a request hint: they must not erase that answer's
      // persisted question link and send the next Regenerate to the legacy API.
      const identifiedFrame = {
        ...frame,
        question_id: frameQuestionId ?? questionIdRef.current,
      };
      setChatHistory((prev) =>
        applyChatStreamFrame(prev, identifiedFrame, contextRef.current ?? {}),
      );
      // A terminal frame ENDS the turn, so the transport stops owning a run
      // right here — it does not wait for the connection to close, because
      // the server never closes it (executions/events.go keeps the stream open
      // and only emits `: heartbeat` comments afterwards). `isStreaming` is
      // "a connection exists", and ChatBox now gates BOTH the Stop button and
      // the composer on it, so leaving the connection open past the terminal
      // frame left the composer disabled for the rest of the session — caught
      // by the #284 journey's "the composer must be released when the turn
      // ends".
      //
      // detach() never touches chat history, and it must not here: the
      // terminal frame has already settled the message through the reducer.
      if (isTurnTerminalFrame(identifiedFrame)) detach();
      if (shouldForwardAgentEvent(identifiedFrame.type))
        onAgentEventRef.current?.(identifiedFrame);
    },
    [setChatHistory, detach],
  );

  /**
   * End the turn with a reason the user can read.
   *
   * The identity is captured BEFORE `detach`, which clears `questionIdRef` —
   * without that ordering a failure-only message would lose its link back to
   * the question it answers.
   */
  const failWith = useCallback(
    (reason: string) => {
      const streamContext = contextRef.current;
      const questionId = questionIdRef.current;
      detach();
      setChatHistory((prev) =>
        recordStreamFailure(prev, reason, streamContext, questionId),
      );
      onStreamErrorRef.current?.(reason);
    },
    [setChatHistory, detach],
  );

  const onFailed = useCallback(
    (frame: ExecutionEventData) => {
      // The server reporting the EXECUTION failed — distinct from the stream
      // dropping. A refusal can be the run's FIRST frame, so this must not
      // assume a message exists to carry it; `recordStreamFailure` appends one
      // when nothing is in flight.
      failWith(runtimeFailureReason(frame));
    },
    [failWith],
  );

  const connection = useChatStreamConnection({
    onNodeEvent,
    onFailed,
    // A spent retry budget ends the turn exactly like a runtime failure: the
    // reason goes on the message, not only to the caller's toast.
    onConnectionLost: failWith,
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
      questionIdRef.current = questionId;
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
    [openStream],
  );

  const starters = useChatStreamRunStarters(subscribeToRun);

  const stop = useCallback(() => {
    // Nothing of this hook's is running — the run was already stopped, already
    // finished, or belonged to a conversation the user has left (which
    // detached it). Settling here would clear the spinner on somebody else's
    // in-flight message.
    if (!ownsRun()) return;
    const target = cancelRef.current;
    detach();
    setChatHistory((prev) => settleInFlight(prev));
    // Closing the client stream does not stop the agent: the execution exists
    // server-side and would keep burning tokens against the user's budget
    // while nobody watches. The DELETE is what actually ends it; its failure
    // is not surfaced because the run is already off this user's screen and a
    // 409 ("not active or cannot be stopped") is the expected answer when the
    // turn finished between the click and the request.
    if (target) void stopChatTask(target).catch(() => undefined);
  }, [detach, setChatHistory, ownsRun]);

  return useMemo(
    () => ({
      ...starters,
      isStreaming,
      close: detach,
      stop,
    }),
    [starters, isStreaming, detach, stop],
  );
}
