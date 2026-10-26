/**
 * `useExecutionEvents` — the durable execution-replay SSE stream (issue #93).
 *
 * One Go route serves every long-running execution the runtime admits —
 * index ingest, agent chat, agent regeneration, configuration validation:
 *
 *   GET {vite_server_url}/executions/{projectId}/{executionId}/events
 *   (services/elitea-main/internal/api/router.go's `runtimeEventsPath`,
 *    handled by internal/api/v2/executions/events.go)
 *
 * The SSE `event:` name is the durable replay event type verbatim
 * (events.go writes `id: <cursor>\nevent: <type>\ndata: <json>`), and the
 * whole vocabulary is a closed set of five constants in the Go repos layer:
 *
 *   execution.node_event              infra/db/repos/node_events.go
 *   execution.failed                  infra/db/repos/{configuration_validation_results,command_outbox}.go
 *   execution.replay_reset            infra/db/repos/replay_events.go
 *   index.ingest.completed            infra/db/repos/index_ingest_results.go
 *   configuration.validation.completed infra/db/repos/configuration_validation_results.go
 *
 * DISCLOSED DEVIATION FROM ISSUE #93's TEXT: the issue names
 * `chat.stream.chunk`, `chat.stream.done` and an SSE `chat_message_sync`
 * for the chat surface. Those names exist in the legacy EliteaUI branch's
 * proof-of-concept, NOT in this repo's Go backend — grepping
 * `services/**` for them returns nothing, while agent execution is
 * explicitly routed through the SAME durable replay log as index ingest
 * (`node_events.go:197,543` gate on `agent.execute.application.v1` /
 * `agent.execute.adhoc.v1`). This module therefore implements the event
 * vocabulary the server actually emits. Add names here the day the backend
 * grows them.
 *
 * `execution.node_event`'s `data` is the bounded NodeEvent v1 JSON
 * (`services/elitea-worker-python/src/elitea_worker/protocol/node_event.py`
 * — thirteen fields: type, stream_id, message_id, question_id, content,
 * thinking, response_metadata, references, sio_event, created_at,
 * parent_message_id, agent_name, execution_generation). That is a superset
 * of `shared/api/socket/events.ts`'s `streamEnvelopeSchema` — the same
 * envelope the socket.io `chat_predict` receive path already carries — so
 * an SSE frame drops straight into the reducers the socket path feeds.
 */
import { useMemo } from 'react';

import { EXECUTION_EVENT, parseEventData, type EventData } from '@elitea/client-core/stream';

import { getConfig } from '@/shared/config';

import { useEventSource } from './useEventSource';

/** The durable replay event names this app subscribes to (shared with the mobile client). */
export const EXECUTION_EVENT_NODE = EXECUTION_EVENT.node;
export const EXECUTION_EVENT_FAILED = EXECUTION_EVENT.failed;
export const EXECUTION_EVENT_INDEX_INGEST_COMPLETED = EXECUTION_EVENT.indexIngestCompleted;
/**
 * The durable log was PRUNED past the cursor this client asked to resume
 * from, so an unknown number of progress frames between that cursor and the
 * one carried here will never be delivered
 * (`infra/db/repos/replay_events.go:89-102` — `{"reason":
 * "progress_retention_window_elapsed"}`). Registered because an unregistered
 * SSE `event:` name is dropped SILENTLY by `EventSource`: without this the
 * frame arrives, nothing reacts, and a resumed long run shows a continuous
 * transcript with a hole in the middle that nothing on screen discloses.
 */
export const EXECUTION_EVENT_REPLAY_RESET = EXECUTION_EVENT.replayReset;
/**
 * Client contract 1.27: the steering events of a running chat turn. The
 * `data` of each is one `SteeringMessage`. They are registered for the same
 * reason as `replay_reset`: `EventSource` drops an unregistered name silently.
 */
export const EXECUTION_EVENT_STEER_QUEUED = EXECUTION_EVENT.steerQueued;
export const EXECUTION_EVENT_STEER_INJECTED = EXECUTION_EVENT.steerInjected;
export const EXECUTION_EVENT_STEER_DROPPED = EXECUTION_EVENT.steerDropped;

/** The SSE `event:` name of a steering frame. */
type SteerEventName = typeof EXECUTION_EVENT_STEER_QUEUED | typeof EXECUTION_EVENT_STEER_INJECTED | typeof EXECUTION_EVENT_STEER_DROPPED;

/**
 * A parsed frame body. Deliberately a loose record: every consumer reads it
 * defensively, exactly as `responseMetadataSchema`'s doc comment in
 * `shared/api/socket/events.ts` argues for the socket side of the same wire.
 */
export type ExecutionEventData = EventData;

/**
 * The bound every backend admission route enforces on a client-supplied or
 * client-correlated id — e.g. `services/elitea-main/internal/application/
 * indexing/start.go`'s `MaxClientCorrelationBytes`, which `start_handler.go`'s
 * `validTaskID` applies to every `task_id` it ever returns.
 */
const MAX_EXECUTION_ID_BYTES = 512;

/**
 * Bounds-checks an execution/task id BEFORE it is interpolated into the SSE
 * URL below (issue #310: "task ids are bounds-checked before use in a URL")
 * — non-empty, no leading/trailing whitespace, no NUL/CR/LF, at most 512
 * bytes. This is the one place every caller's `executionId` (however it was
 * obtained — a normal start response, an adopted 409 conflict, a recovered
 * value) actually becomes a URL, so the guard lives here rather than relying
 * on every caller to have checked first. `features/toolkits/indexes/lib/
 * helpers/indexExecution.helpers.ts`'s `isBoundedIndexExecutionTaskId`
 * applies the identical rule where a `task_id` is first accepted off the
 * wire — duplicated rather than imported because `shared/` must not depend
 * on a feature slice (R-L3): this file protects the transport boundary for
 * every execution kind, not only index runs.
 */
function isBoundedExecutionId(value: string): boolean {
  return (
    value.length > 0 &&
    value === value.trim() &&
    !value.includes('\0') &&
    !value.includes('\r') &&
    !value.includes('\n') &&
    new TextEncoder().encode(value).length <= MAX_EXECUTION_ID_BYTES
  );
}

/**
 * `JSON.parse` at a transport boundary, as a value and never a throw (§3.6):
 * a malformed or non-object frame yields `undefined` and the handler is
 * skipped, rather than killing the React event handler that called it.
 */
export function parseExecutionEventData(event: MessageEvent): ExecutionEventData | undefined {
  return parseEventData(event.data);
}

/**
 * Resolve a server-supplied `events_url` against the configured API origin.
 *
 * The agent-execution start endpoint returns an ABSOLUTE PATH it builds
 * itself (`"/api/v2/executions/" + projectID + "/" + executionID +
 * "/events"`, `internal/api/v2/agentexecution/route.go`) — never a
 * full URL. When `vite_server_url` is a same-origin prefix (the compose and
 * dev deployments, where it is literally `/api/v2`) that path is already
 * correct and must NOT be prefixed again. When `vite_server_url` names a
 * DIFFERENT ORIGIN, the bare path would resolve against the web origin
 * instead of the API's, so the origin — and only the origin — is prepended.
 */
export function resolveExecutionEventsUrl(serverUrl: string | null, eventsUrl: string | null | undefined): string | null {
  if (!eventsUrl) return null;
  if (/^https?:\/\//i.test(eventsUrl)) return eventsUrl;
  if (serverUrl && /^https?:\/\//i.test(serverUrl)) {
    try {
      return new URL(eventsUrl, serverUrl).toString();
    } catch {
      // A `vite_server_url` that does not parse is a config problem, not a
      // reason to lose the stream — fall through to the raw path (§3.6).
      return eventsUrl;
    }
  }
  return eventsUrl;
}

/** The three callbacks an execution stream can drive. Shared by both subscription entry points below. */
export interface ExecutionEventCallbacks {
  /** One streamed progress frame (NodeEvent v1 — `streamEnvelopeSchema`-compatible). */
  readonly onNodeEvent?: ((frame: ExecutionEventData) => void) | undefined;
  /** The index-ingest terminal frame. */
  readonly onIndexIngestCompleted?: ((frame: ExecutionEventData) => void) | undefined;
  /** The runtime-failure frame (also emitted on deadline retirement and cancellation). Carries `code`, `safe_message` and `retryable` — see `infra/db/repos/command_outbox.go:29-30`. */
  readonly onFailed?: ((frame: ExecutionEventData) => void) | undefined;
  /** Progress frames were pruned before this client could read them; the frame carries `reason`. */
  readonly onReplayReset?: ((frame: ExecutionEventData) => void) | undefined;
  /** A steering frame (`steer.queued`, `steer.injected` or `steer.dropped`) and its event name. */
  readonly onSteer?: ((event: SteerEventName, frame: ExecutionEventData) => void) | undefined;
  /**
   * The durable cursor of the frame just delivered — `events.go` writes an
   * `id: <cursor>` line before every event, and that is what a resume has to
   * send back (`./resume.ts`). Fired for EVERY event name, including the ones
   * this caller has no handler for, because the cursor is a property of the
   * stream rather than of any one frame family: resuming from the last frame
   * a caller happened to care about would ask the server to replay everything
   * in between all over again.
   */
  readonly onCursor?: ((cursor: string) => void) | undefined;
  /**
   * The stream failed to OPEN, or dropped — a transport failure, not a
   * frame. Distinct from `onFailed` (which is the server telling you the
   * execution itself failed): this fires when there is no stream at all,
   * e.g. the route is not mounted, the execution belongs to a backend that
   * serves no replay stream, or the admission gate answered 429.
   * `EventSource` does not retry after an HTTP status, so a caller that
   * needs the run to proceed must act here.
   */
  readonly onError?: ((event: Event) => void) | undefined;
  /**
   * The stream actually OPENED (see `./useEventSource.ts`'s own doc comment
   * on this option) — fired before any frame arrives, as soon as the HTTP
   * response headers come back successfully. A caller that must tell "this
   * `executionId` was never a real stream" apart from "it opened, then
   * later dropped" needs this: `onError` alone cannot make that
   * distinction (issue #310).
   */
  readonly onOpen?: ((event: Event) => void) | undefined;
}

/**
 * Subscribe to an execution stream by the `events_url` the START ENDPOINT
 * returned (issue #93's chat surface). Prefer this over re-deriving the
 * path: the server owns that shape.
 *
 * All seven event names are ALWAYS registered, whether or not the caller
 * passed the matching callback: the registered name set is what
 * `useEventSource` keys its connection on, so a conditional map would
 * reopen the HTTP stream whenever a caller's callback appeared or
 * disappeared. Frames with no callback are parsed and dropped.
 */
export function useExecutionEventStream(eventsUrl: string | null | undefined, callbacks: ExecutionEventCallbacks): void {
  const { onNodeEvent, onIndexIngestCompleted, onFailed, onReplayReset, onSteer, onCursor, onError, onOpen } = callbacks;
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;
  const url = useMemo(() => resolveExecutionEventsUrl(serverUrl, eventsUrl), [serverUrl, eventsUrl]);

  /**
   * Record the cursor FIRST, then dispatch the frame.
   *
   * The cursor advances even for a frame that fails to parse: a resume from
   * before a malformed frame would ask the server to send that same frame
   * again, and it would fail again — an unparseable frame is a hole in the
   * transcript, not a reason to loop on it.
   */
  const deliver = (handler: ((frame: ExecutionEventData) => void) | undefined) => (event: MessageEvent) => {
    if (event.lastEventId) onCursor?.(event.lastEventId);
    const frame = parseExecutionEventData(event);
    if (frame) handler?.(frame);
  };

  useEventSource(url, {
    [EXECUTION_EVENT_NODE]: deliver(onNodeEvent),
    [EXECUTION_EVENT_INDEX_INGEST_COMPLETED]: deliver(onIndexIngestCompleted),
    [EXECUTION_EVENT_FAILED]: deliver(onFailed),
    [EXECUTION_EVENT_REPLAY_RESET]: deliver(onReplayReset),
    [EXECUTION_EVENT_STEER_QUEUED]: deliver(onSteer && ((frame) => { onSteer(EXECUTION_EVENT_STEER_QUEUED, frame); })),
    [EXECUTION_EVENT_STEER_INJECTED]: deliver(onSteer && ((frame) => { onSteer(EXECUTION_EVENT_STEER_INJECTED, frame); })),
    [EXECUTION_EVENT_STEER_DROPPED]: deliver(onSteer && ((frame) => { onSteer(EXECUTION_EVENT_STEER_DROPPED, frame); })),
  }, { onError, onOpen });
}

export interface UseExecutionEventsParams extends ExecutionEventCallbacks {
  readonly projectId: string | number | undefined;
  /** The `task_id`/`execution_id` the start endpoint returned. Undefined ⇒ no stream. */
  readonly executionId: string | undefined;
}

/**
 * Subscribe to one execution's durable event stream.
 *
 * All seven event names are ALWAYS registered, whether or not the caller
 * passed the matching callback: the registered name set is what
 * `useEventSource` keys its connection on, so a conditional map would
 * reopen the HTTP stream whenever a caller's callback appeared or
 * disappeared. Frames with no callback are parsed and dropped.
 */
export function useExecutionEvents(params: UseExecutionEventsParams): void {
  const { projectId, executionId, onNodeEvent, onIndexIngestCompleted, onFailed, onReplayReset, onSteer, onCursor, onError, onOpen } = params;
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;

  // The index surface starts its run through a route that returns only a
  // `task_id`, so the path is derived here rather than supplied. Bounds-checked
  // (issue #310) before it is interpolated: an out-of-bounds `executionId`
  // produces no URL at all, exactly like a missing one — see
  // `isBoundedExecutionId`'s own doc comment.
  const eventsUrl = useMemo(
    () =>
      projectId !== undefined && projectId !== '' && executionId && isBoundedExecutionId(executionId) && serverUrl
        ? `${serverUrl}/executions/${String(projectId)}/${executionId}/events`
        : null,
    [projectId, executionId, serverUrl],
  );

  // Already absolute against `vite_server_url` ⇒ `useExecutionEventStream`'s
  // own resolution is a no-op for it (same-origin prefix returns the path
  // unchanged; an absolute origin was already baked in above).
  useExecutionEventStream(eventsUrl, { onNodeEvent, onIndexIngestCompleted, onFailed, onReplayReset, onSteer, onCursor, onError, onOpen });
}
