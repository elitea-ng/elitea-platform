/**
 * Index-execution admission helpers (issue #310).
 *
 * `../../lib/hooks/useToolkitChatDispatch.hooks.ts`'s `startToolRun` used to
 * treat EVERY `startIndexExecution` failure the same way: swallow it in a
 * bare `catch {}` and fall through to the socket.io `chat_predict` emit. A
 * `409 "Indexing is already in progress for this index"` is not a failure
 * to retry around — it is the Go handler
 * (`services/elitea-main/internal/api/v2/indexing/start_handler.go`'s
 * `writeStartError`, `ActiveIndexConflictError` branch) reporting that a run
 * for this index is ALREADY admitted, and naming its `task_id`. Falling
 * through started a SECOND run: duplicate work, double billing, and (since
 * the error was swallowed) no test could observe it.
 *
 * Ported from the legacy branch's `indexExecution.helpers.js`
 * (`EliteaAI/EliteaUI@feat/replatform-index-sse-pov-main-sync`) —
 * `isBoundedIndexExecutionTaskId`, `parseIndexStartConflictTaskId` and
 * `canStartToolkitRun` only. The rest of that file
 * (`buildPendingIndexExecutionKey`/`findAuthoritativeActiveIndex`/
 * `sameIndexExecution`/`resolveAuthoritativeIndexExecutionTaskId`/session-
 * storage reattach) backs a fuller "reattach on reload" flow issue #310
 * explicitly scopes OUT (see its "Done means" — that flow is not one of the
 * five listed behaviours), and depends on machinery (`refetchIndexesList`
 * returning the fresh list synchronously, an `onActiveIndexReattach`
 * confirmation callback) this app's port of the hook does not carry. Not
 * ported here.
 */

/**
 * The literal `MaxClientCorrelationBytes` bound `start_handler.go`'s own
 * `validTaskID` enforces server-side on every task id it ever hands back
 * (`services/elitea-main/internal/application/indexing/start.go`) — a task
 * id this app receives should never exceed it either.
 */
const MAX_INDEX_EXECUTION_TASK_ID_BYTES = 512;

/**
 * Bounds-checks a task id before it is trusted to become `executionId`
 * (and, from there, interpolated into the SSE events URL —
 * `@/shared/api/sse`'s own `isBoundedExecutionId` re-guards that exact
 * boundary, see that file's header for why the check is duplicated rather
 * than imported across the shared/feature layer boundary). Matches
 * `start_handler.go`'s `validTaskID` exactly: non-empty, no leading/
 * trailing whitespace, no NUL/CR/LF, at most 512 bytes.
 */
export function isBoundedIndexExecutionTaskId(value: unknown): value is string {
  return (
    typeof value === 'string' &&
    value.length > 0 &&
    value === value.trim() &&
    !value.includes('\0') &&
    !value.includes('\r') &&
    !value.includes('\n') &&
    new TextEncoder().encode(value).length <= MAX_INDEX_EXECUTION_TASK_ID_BYTES
  );
}

/** The exact message `start_handler.go`'s `writeStartError` writes for an `ActiveIndexConflictError`. */
export const ACTIVE_INDEX_CONFLICT_MESSAGE = 'Indexing is already in progress for this index';

/**
 * Duck-typed rather than an `instanceof EliteaApiError` check — same
 * convention as `features/toolkits/sharepoint/lib/hooks/
 * useSharepointCheckConnection.hooks.ts`'s `isEliteaApiErrorLike` (that
 * file's own doc comment explains the reasoning: it keeps this module
 * testable against a hand-built fixture without importing the concrete
 * class from `shared/api/generated/mutator`, and works identically against
 * the real thing since `EliteaApiError` carries exactly this shape).
 */
interface EliteaApiErrorLike {
  readonly failure?: { readonly kind?: string; readonly status?: number; readonly body?: unknown };
}

function isEliteaApiErrorLike(value: unknown): value is EliteaApiErrorLike {
  return typeof value === 'object' && value !== null && 'failure' in value;
}

/**
 * Whether `body` is EXACTLY `{error, task_id}` (nothing more, nothing less)
 * with the expected conflict text — split out of `parseIndexStartConflictTaskId`
 * to keep that function's branch count under the repo's complexity budget.
 */
function isActiveIndexConflictBody(body: unknown): body is { readonly task_id?: unknown } {
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return false;
  const keys = Object.keys(body).sort();
  if (keys.length !== 2 || keys[0] !== 'error' || keys[1] !== 'task_id') return false;
  return (body as { error?: unknown }).error === ACTIVE_INDEX_CONFLICT_MESSAGE;
}

/**
 * Extracts the `task_id` of the run already in flight from a 409 thrown by
 * `startIndexExecution` — the caller adopts it instead of retrying (issue
 * #310). Deliberately narrow, matching the legacy branch's own contract
 * note verbatim: this parser is for the CURRENT `startIndexExecution` 409
 * only, and callers must not treat an arbitrary 409 as proof an execution
 * is authorized or visible. Trusts the body only when it is EXACTLY
 * `{error, task_id}` (nothing more, nothing less) with the expected error
 * text and a bounded task id — anything else returns `undefined` so the
 * caller falls through to its existing (non-retrying-the-same-run) failure
 * handling.
 */
export function parseIndexStartConflictTaskId(error: unknown): string | undefined {
  if (!isEliteaApiErrorLike(error)) return undefined;
  const { failure } = error;
  if (failure?.kind !== 'http' || failure.status !== 409) return undefined;
  if (!isActiveIndexConflictBody(failure.body)) return undefined;

  const { task_id: taskId } = failure.body;
  return isBoundedIndexExecutionTaskId(taskId) ? taskId : undefined;
}

/** A refusal text is shown as is, so it is bounded and single-purpose. */
const MAX_START_REFUSAL_CHARS = 400;

/**
 * The reason `start_handler.go` gave for REFUSING an index start (#2042): `422`
 * "No embedding model is available to this project…", `{error: <sentence>}`. Only
 * this definitive refusal ends the run: a `503` "Embedding model is unavailable"
 * is a transient catalogue error and keeps the previous fallback. Such a start was admitted by
 * nobody, so falling back to the socket (which does nothing here) only hid the
 * reason behind a "no live connection" notice; the caller shows this text
 * instead. A `422` validation body carries an ERROR LIST, not a sentence, so it does not match.
 */
export function parseIndexStartRefusalMessage(error: unknown): string | undefined {
  const message = refusalBodyError(error);
  const text = message?.trim();
  return text !== undefined && text !== '' && text.length <= MAX_START_REFUSAL_CHARS ? text : undefined;
}

/** `error` of a `422` body that is a plain object (a validation body carries a list, not a sentence), if it is a string. */
function refusalBodyError(error: unknown): string | undefined {
  if (!isEliteaApiErrorLike(error)) return undefined;
  const { failure } = error;
  if (failure?.kind !== 'http' || failure.status !== 422) return undefined;
  const body = failure.body;
  if (typeof body !== 'object' || body === null || Array.isArray(body)) return undefined;
  const message = (body as { error?: unknown }).error;
  return typeof message === 'string' ? message : undefined;
}

export interface CanStartToolkitRunParams {
  readonly indexing: boolean;
  readonly isCreateIndexMode: boolean;
  readonly isValidForm: boolean;
  readonly isRunning: boolean;
  /** The index this run would start is ALREADY active, per the last known server metadata. */
  readonly isIndexing: boolean;
}

/**
 * Gate a run start (issue #310: "a start is gated when one is already
 * active"). Port of the legacy branch's `canStartToolkitRun`, minus its
 * `indexStartPending` flag: that flag exists in the baseline to cover the
 * gap between a click and the start call's response, but this app's
 * `useToolkitChat.hooks.ts` already closes that gap by setting `isRunning`
 * to `true` SYNCHRONOUSLY before dispatching (`run()`'s `setIsRunning(true)`
 * precedes `executeRunTool`) — `isRunning` alone already covers what
 * `indexStartPending` covers in the baseline. `isIndexing` is the real gap
 * this app was missing: `IndexActions.tsx` already hides the start button
 * once ITS OWN copy of `isIndexing` is true, but nothing stopped `run()`
 * itself from starting a second REST call when local state has not caught
 * up with a run started elsewhere (a different tab, a stale
 * `refetchIndexesList` poll) — exactly the race issue #310 traces to a 409.
 */
export function canStartToolkitRun(params: CanStartToolkitRunParams): boolean {
  const { indexing, isCreateIndexMode, isValidForm, isRunning, isIndexing } = params;
  return ((indexing && !isCreateIndexMode) || isValidForm) && !isRunning && (!indexing || !isIndexing);
}

/**
 * Correlates one SSE `execution.node_event` frame's `message_id` against the
 * id the CURRENT run is tracking (issue #310: "No message_id guard — any
 * frame arriving on the stream reaches the reducer, regardless of which run
 * it belongs to"). `trackedMessageId` is `undefined` until the first frame
 * of a run has been seen; from then on a frame carrying a DIFFERENT,
 * non-empty `message_id` is not part of this run and must be dropped rather
 * than reach the chat-history reducer — see `../../../lib/hooks/
 * useToolkitChatSocket.hooks.ts`'s `handleNodeEvent`, which locks
 * `trackedMessageId` to the first frame's id and calls this on every frame
 * after. A frame with no `message_id` at all always passes: the socket
 * `chat_predict` envelope schema (`shared/api/socket/events.ts`) declares it
 * optional, and a frame that carries none carries no correlation
 * information to reject on.
 */
export function isFrameForCurrentIndexExecution(
  frameMessageId: string | undefined,
  trackedMessageId: string | undefined,
): boolean {
  if (trackedMessageId === undefined) return true;
  if (frameMessageId === undefined || frameMessageId === '') return true;
  return frameMessageId === trackedMessageId;
}
