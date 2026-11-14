/**
 * Response wiring slice of `./useToolkitChat.hooks.ts`. Carries BOTH live
 * transports for the toolkit/index run:
 *
 *  - SSE (issue #93, preferred): once `./useToolkitChat.hooks.ts`'s REST
 *    start call returns a `task_id`, `useExecutionEvents` follows that
 *    execution's durable event stream. `execution.node_event` frames are
 *    the same NodeEvent v1 envelope the socket carried, so they go through
 *    the SAME `generateChatMessageBasedOnResponse` reducer;
 *    `index.ingest.completed` and `execution.failed` are the terminal
 *    frames the socket path had no distinct equivalent for.
 *  - socket.io (fallback): the original `chat_predict` response listener
 *    and `chat_enter_room`/`chat_leave_rooms` room lifecycle, kept intact
 *    for backends that answer the start call without a `task_id`. Baseline
 *    citation `useToolkitChat.hooks.js:180-226`; see the
 *    `mcp_authorization_required`/`useSocketRoom` deviation rationale in
 *    `./useToolkitChat.hooks.ts`.
 *
 * The two never double-drive the reducer in practice: the SSE stream only
 * opens when an `executionId` exists, and that only happens on the REST
 * path, which does not emit `chat_predict`. Both remain wired
 * unconditionally because a run started before a backend upgrade must keep
 * streaming on the transport it started on.
 *
 * TWO issue #310 fixes live in `handleNodeEvent`/the `useExecutionEvents`
 * wiring below:
 *
 *  - MESSAGE-ID CORRELATION: `handleNodeEvent` used to forward any
 *    `execution.node_event` frame with a `type` straight to the reducer,
 *    regardless of which run's `message_id` it carried. `trackedMessageIdRef`
 *    locks onto the first frame's `message_id` for the run currently
 *    following `executionId`, and `isFrameForCurrentIndexExecution`
 *    (`../../indexes/lib/helpers/indexExecution.helpers.ts`) drops any later
 *    frame that names a DIFFERENT one — a stray frame from another run must
 *    not corrupt this run's transcript.
 *  - ONE-SHOT FALLBACK: `onStreamError` used to fire `runSocketFallback`
 *    (start the run over socket.io) on ANY stream error, including one that
 *    arrives long after the stream genuinely opened and had already been
 *    carrying real frames — a network blip or a backgrounded tab would
 *    re-dispatch the SAME run a second time. `hasStreamOpenedRef`
 *    (set from the `open` event `useEventSource.ts` now exposes) makes
 *    `onStreamError` a no-op once the stream has opened at least once; only
 *    an error BEFORE any successful open still means "this task_id was not
 *    a real Go execution" and falls back.
 *
 * Both refs are reset whenever `executionId` changes, so a fresh run is
 * never gated by the PREVIOUS run's state.
 */
import { useCallback, useEffect, useRef } from 'react';

import { useSocketClient, type SocketClient } from '@/shared/api/socket/client';
import type { ReceivePayloadOf } from '@/shared/api/socket/events';
import { useSocketRoom } from '@/shared/api/socket/rooms';
import { useExecutionEvents, type ExecutionEventData } from '@/shared/api/sse';

import { IndexStatuses } from '../../indexes/lib/constants/indexDetails.constants';
import type { GenerateChatMessageBasedOnResponseParams, IndexChatMessage } from '../../indexes/lib/helpers/indexChat.helpers';
import { generateChatMessageBasedOnResponse, generateMockMessageTemplate } from '../../indexes/lib/helpers/indexChat.helpers';
import { isFrameForCurrentIndexExecution } from '../../indexes/lib/helpers/indexExecution.helpers';
import { buildIndexRunSummaryMessage, ensureRunMessage, settleIndexRunProgress } from '../helpers/indexRunNotices.helpers';
import type { ToolkitChatMessage } from './useToolkitChat.types';

/**
 * `index.ingest.completed`'s `status` -> this app's `IndexStatuses`.
 *
 * Server side, `indexReplayData` (`services/elitea-main/internal/infra/db/
 * repos/index_ingest_results.go:613-645`) writes ONE OF TWO SHAPES, and they
 * mean different things:
 *
 *  - the SUMMARY shape, `{status, message}`, whose `status` is the closed set
 *    `ok` | `partly_indexed` | `error` (`internal/application/output/
 *    index_ingest.go:118-122`);
 *  - the ARTIFACT shape, `{artifact_id, immutable_version, media_type,
 *    byte_length, digest, classification}`, which carries NO `status` key —
 *    the reference type has no outcome field at all, and the projection only
 *    writes it after `IndexIngestResult.Validate()` and artifact verification
 *    have both passed.
 *
 * The two are therefore treated differently, where a single `default:` used
 * to treat them the same:
 *
 *  - An artifact-shaped frame keeps settling as `completed`. It is a verified
 *    terminal projection and there is nothing in it to read an outcome out of;
 *    inventing a failure here would be as much of a guess as the old code's
 *    success.
 *  - A frame that DOES carry a `status` but not one of the three known values
 *    now settles as `failed`, not `completed`. That branch is the real defect
 *    the old `default:` hid: an unrecognised terminal status is not a success
 *    claim, and `completed` is in `RUNNABLE_INDEX_STATUSES` — so the old
 *    mapping did not merely paint the wrong colour, it advertised the index as
 *    searchable on the strength of a status this build has never seen.
 */
function toIndexState(frame: ExecutionEventData): string {
  const status = frame['status'];
  switch (status) {
    case 'ok':
      return IndexStatuses.success;
    case 'error':
      return IndexStatuses.fail;
    case 'partly_indexed':
      return IndexStatuses.partlyOk;
    default:
      // Absent `status` ⇒ the artifact shape ⇒ a verified completion.
      // Present-but-unknown ⇒ refuse to claim success.
      return status === undefined ? IndexStatuses.success : IndexStatuses.fail;
  }
}

/**
 * The user-visible text for an `execution.failed` frame.
 *
 * The frame is not empty and never was: `infra/db/repos/command_outbox.go:
 * 29-30` writes `{"code", "safe_message", "retryable"}`, and `safe_message` is
 * named that way precisely because it is the one field cleared for display.
 * The handler used to take NO ARGUMENT, so every one of those bytes was
 * discarded and a cancelled run, a deadline retirement and a genuine runtime
 * fault were all indistinguishable on screen.
 */
function failureNotice(frame: ExecutionEventData): string {
  const safeMessage = typeof frame['safe_message'] === 'string' ? frame['safe_message'] : '';
  const code = typeof frame['code'] === 'string' ? frame['code'] : '';
  const retryable = frame['retryable'] === true ? '\n\nThis can be retried.' : '';
  const detail = safeMessage !== '' ? safeMessage : 'The run failed before it produced a result.';
  return `❌ ${detail}${code !== '' ? `\n\n**Code:** ${code}` : ''}${retryable}`;
}

/**
 * The user-visible text for an `execution.replay_reset` frame.
 *
 * Emitted when the durable log was pruned past the cursor being resumed from
 * (`infra/db/repos/replay_events.go:89-102`), i.e. progress frames exist that
 * this client will never receive. The run itself is unaffected — which is
 * exactly why it needs saying: without it the transcript looks complete.
 */
function replayResetNotice(frame: ExecutionEventData): string {
  const reason = typeof frame['reason'] === 'string' && frame['reason'] !== '' ? frame['reason'] : 'unknown';
  return `⚠️ Some earlier progress updates are no longer available and have been skipped (${reason}). The run itself is still going.`;
}

/**
 * An `execution.node_event` frame -> the `chat_predict` receive shape. The
 * NodeEvent v1 JSON contract (`services/elitea-worker-python/src/
 * elitea_worker/protocol/node_event.py`) is a superset of
 * `streamEnvelopeSchema`, so this narrows rather than converts. A frame
 * without the required `type` discriminant is not a usable envelope and is
 * dropped by the caller.
 */
function toStreamEnvelope(frame: ExecutionEventData): ReceivePayloadOf<'chat_predict'> | undefined {
  return typeof frame['type'] === 'string' ? (frame as unknown as ReceivePayloadOf<'chat_predict'>) : undefined;
}

/**
 * `ReceivePayloadOf<'chat_predict'>` (`streamEnvelopeSchema`, `shared/api/
 * socket/events.ts`) -> `SocketMessageLike` (`indexChat.helpers.ts`'s own,
 * narrower, REQUIRED-`message_id` shape). Picks only the fields the
 * reducer reads; `exactOptionalPropertyTypes` requires the
 * conditional-spread pattern for the optional ones (an absent source field
 * must produce an ABSENT target key, never a present key holding
 * `undefined` — matching `entities/application-form/lib/normalise.ts`'s
 * convention).
 */
function toSocketMessageLike(message: ReceivePayloadOf<'chat_predict'>): GenerateChatMessageBasedOnResponseParams['message'] {
  type TargetMessage = GenerateChatMessageBasedOnResponseParams['message'];

  // Built imperatively, not via a ternary-conditional-spread: spreading a
  // `{field: X} | {}` UNION infers the merged property as `field?: X |
  // undefined` (an explicit-undefined optional), which `exactOptionalPropertyTypes`
  // treats as a stricter shape than the target's true "absent or X" optional
  // (`field?: X`) — assigning a mutable local sidesteps that inference
  // entirely, matching the ACTUAL "absent means absent" semantics.
  const result: TargetMessage = {
    message_id: message.message_id ?? '',
    type: message.type,
    // `content`'s target type (`SocketMessageLike.content?: unknown`)
    // already accepts `undefined` as a valid `unknown` value, so no guard
    // is needed for this one field.
    content: message.content,
  };

  // `shared/api/socket/events.ts`'s `responseMetadataSchema` is a
  // deliberately permissive `Record<string, unknown>` (see that schema's
  // own doc comment — dozens of wildly different nested keys per
  // discriminant); `indexChat.helpers.ts`'s `ResponseMetadataLike` names
  // the specific keys THIS reducer reads out of that same permissive blob.
  // Both are real, already-landed shapes for the same wire value — this is
  // a type bridge between them, not an assumption about the data.
  if (message.response_metadata !== undefined) {
    result.response_metadata = message.response_metadata;
  }
  if (message.created_at !== undefined) {
    result.created_at = message.created_at;
  }

  return result;
}

export interface UseToolkitChatSocketParams {
  readonly isAuthCheckSession: boolean;
  readonly onMcpAuthRequired: ((message: Readonly<Record<string, unknown>>) => void) | undefined;
  readonly onRunFinish: (state: string) => void;
  readonly onStartTask: (taskId: string | undefined) => void;
  readonly setChatHistory: (update: (prev: ToolkitChatMessage[]) => ToolkitChatMessage[]) => void;
  readonly activeConversationId: string | number | undefined;
  readonly activeConversationUuid: string | undefined;
  readonly projectId: string | undefined;
  readonly roomEnabled: boolean;
  /** The `task_id` the REST start call returned (issue #93). Undefined ⇒ socket-only run. */
  readonly executionId: string | undefined;
  /**
   * The stream failed to open (or dropped) BEFORE it ever opened
   * successfully. This is the ONLY reliable "that `task_id` was not a Go
   * execution" signal — see `./useToolkitChatDispatch.hooks.ts`'s header —
   * so the caller uses it to emit the run on socket.io after all. Once the
   * stream has opened at least once, this hook stops calling it (issue
   * #310) — a later drop is a transport hiccup on a run that is genuinely
   * in progress, not proof the run needs restarting.
   */
  readonly onStreamError: () => void;
}

/** Live run wiring: the SSE execution stream plus the socket.io `chat_predict` fallback and its room lifecycle; returns the shared socket client for `useToolkitChat.hooks.ts`'s own `chat_predict` EMIT call. */
export function useToolkitChatSocket(params: UseToolkitChatSocketParams): SocketClient {
  const { isAuthCheckSession, onMcpAuthRequired, onRunFinish, onStartTask, setChatHistory, activeConversationId, activeConversationUuid, projectId, roomEnabled, executionId, onStreamError } = params;
  const socket = useSocketClient();

  const isAuthCheckSessionRef = useRef(isAuthCheckSession);
  useEffect(() => {
    isAuthCheckSessionRef.current = isAuthCheckSession;
  }, [isAuthCheckSession]);

  const onMcpAuthRequiredRef = useRef(onMcpAuthRequired);
  useEffect(() => {
    onMcpAuthRequiredRef.current = onMcpAuthRequired;
  }, [onMcpAuthRequired]);

  const handleSocketResponse = useCallback(
    (message: ReceivePayloadOf<'chat_predict'>) => {
      if (isAuthCheckSessionRef.current) return;

      if (message.type === 'mcp_authorization_required') {
        onMcpAuthRequiredRef.current?.(message);
        return;
      }

      setChatHistory((prev) =>
        generateChatMessageBasedOnResponse({
          message: toSocketMessageLike(message),
          // `prev` may hold recovered `Message`-shaped entries (see
          // `./useToolkitChat.types.ts`'s `ToolkitChatMessage` doc comment)
          // — this reducer only ever reads `.id`/`.toolActions`/`.content`,
          // present on both shapes at runtime; the bridge cast documents
          // that real looseness rather than silently dropping entries via a
          // shape-narrowing filter.
          chatHistory: ensureRunMessage(prev, message.type, message.message_id, Date.now()) as IndexChatMessage[],
          onFinish: onRunFinish,
          onStartTask,
        }),
      );
    },
    [onRunFinish, onStartTask, setChatHistory],
  );

  useEffect(() => {
    socket.on('chat_predict', handleSocketResponse);
    return () => socket.off('chat_predict', handleSocketResponse);
  }, [socket, handleSocketResponse]);

  /**
   * The `message_id` the CURRENT `executionId`'s stream is tracking — locked
   * to the first frame seen, then used to drop a stray frame from a
   * different run (issue #310). Reset whenever `executionId` changes so a
   * fresh run is never gated by the previous one's id.
   */
  const trackedMessageIdRef = useRef<string | undefined>(undefined);
  /** Whether the CURRENT `executionId`'s stream has opened at least once — see `onStreamError`'s own doc comment and the module header. */
  const hasStreamOpenedRef = useRef(false);
  /** When the CURRENT `executionId` was first followed: the summary's duration is measured from it. */
  const runStartedAtRef = useRef<number | undefined>(undefined);
  useEffect(() => {
    trackedMessageIdRef.current = undefined;
    hasStreamOpenedRef.current = false;
    runStartedAtRef.current = executionId === undefined ? undefined : Date.now();
  }, [executionId]);

  const handleNodeEvent = useCallback(
    (frame: ExecutionEventData) => {
      const envelope = toStreamEnvelope(frame);
      if (!envelope) return;
      if (!isFrameForCurrentIndexExecution(envelope.message_id, trackedMessageIdRef.current)) return;
      trackedMessageIdRef.current ??= envelope.message_id;
      handleSocketResponse(envelope);
    },
    [handleSocketResponse],
  );

  const handleStreamOpen = useCallback(() => {
    hasStreamOpenedRef.current = true;
  }, []);

  const handleStreamError = useCallback(() => {
    // Once the stream has genuinely opened, the run IS on the Go execution
    // log — a later drop must not be treated as "start over on socket.io"
    // (issue #310: that would dispatch a SECOND run alongside the one still
    // progressing server-side).
    if (hasStreamOpenedRef.current) return;
    onStreamError();
  }, [onStreamError]);

  const handleIngestCompleted = useCallback(
    (frame: ExecutionEventData) => {
      if (isAuthCheckSessionRef.current) return;
      // The outcome where the user is looking (#2041), not only in History.
      const summary = buildIndexRunSummaryMessage(frame, runStartedAtRef.current, Date.now());
      setChatHistory((prev) => settleIndexRunProgress(prev, summary));
      onRunFinish(toIndexState(frame));
    },
    [onRunFinish, setChatHistory],
  );

  const handleExecutionFailed = useCallback(
    (frame: ExecutionEventData) => {
      if (isAuthCheckSessionRef.current) return;
      // Rendered as a chat entry rather than routed to `onRunFinish`, which
      // takes a bare status string and has nowhere to put a reason.
      const notice = generateMockMessageTemplate(failureNotice(frame), 'toolkit');
      setChatHistory((prev) => settleIndexRunProgress(prev, notice));
      onRunFinish(IndexStatuses.fail);
    },
    [onRunFinish, setChatHistory],
  );

  const handleReplayReset = useCallback(
    (frame: ExecutionEventData) => {
      if (isAuthCheckSessionRef.current) return;
      // NOT terminal: the run continues, only the transcript has a hole. So
      // this appends a notice and deliberately does not call `onRunFinish`.
      const notice = generateMockMessageTemplate(replayResetNotice(frame), 'toolkit');
      setChatHistory((prev) => [...prev, notice]);
    },
    [setChatHistory],
  );

  useExecutionEvents({
    projectId,
    executionId,
    onNodeEvent: handleNodeEvent,
    onIndexIngestCompleted: handleIngestCompleted,
    onFailed: handleExecutionFailed,
    onReplayReset: handleReplayReset,
    onOpen: handleStreamOpen,
    onError: handleStreamError,
  });

  useSocketRoom(activeConversationId !== undefined ? String(activeConversationId) : undefined, {
    context: { conversation_uuid: activeConversationUuid, project_id: projectId },
    enabled: roomEnabled,
  });

  return socket;
}
