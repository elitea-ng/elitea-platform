/**
 * The streamed agent draft (ADR-0037 decision 4), as pure state shared by the
 * web, desktop and mobile builders.
 *
 * The server route is `generateApplicationDraftStream`
 * (`POST /elitea_core/generate_application_draft_stream/prompt_lib/{project_id}`).
 * It answers `draft.token` frames (model text, shown while it arrives), then
 * one `draft.complete` (the validated draft) or one `draft.error`. Each host
 * reads the bytes with its own HTTP client, feeds them to `EventStreamParser`
 * (`../stream`), and folds the events with `draftReducer`.
 */
import { parseEventData, type SseEvent } from '../stream';

/** The draft stream's event names. */
export const DRAFT_EVENT = {
  token: 'draft.token',
  complete: 'draft.complete',
  error: 'draft.error',
} as const;

/** One suggested project resource of a generated draft. */
export interface DraftSuggestion {
  readonly id: string;
  readonly name: string;
  readonly type?: string | undefined;
  readonly description?: string | undefined;
  readonly agent_type?: string | undefined;
}

/** The validated draft `draft.complete` carries (the `ApplicationDraft` schema). */
export interface StreamedAgentDraft {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcome_message: string;
  readonly conversation_starters: readonly string[];
  readonly suggested_toolkits: readonly DraftSuggestion[];
  readonly suggested_mcp: readonly DraftSuggestion[];
  readonly suggested_pipelines: readonly DraftSuggestion[];
  readonly suggested_agents: readonly DraftSuggestion[];
  readonly suggested_skills: readonly DraftSuggestion[];
}

/** The data of `draft.error` (the `DraftStreamError` schema). */
export interface DraftStreamFailure {
  readonly code: string;
  readonly status: number;
  readonly message: string;
}

/** One turn of the builder conversation, as the route takes it. */
export interface DraftTurn {
  readonly role: 'user' | 'assistant';
  readonly content: string;
}

/** The route keeps at most this many history turns; the client sends no more. */
export const MAX_DRAFT_HISTORY = 20;

export type DraftStreamEvent =
  | { readonly type: 'token'; readonly text: string }
  | { readonly type: 'complete'; readonly draft: StreamedAgentDraft }
  | { readonly type: 'error'; readonly failure: DraftStreamFailure };

const list = (value: unknown): DraftSuggestion[] => (Array.isArray(value) ? (value as DraftSuggestion[]) : []);
const text = (value: unknown): string => (typeof value === 'string' ? value : '');

/** The draft in a `draft.complete` frame, or undefined when it is not one. */
function readDraft(data: unknown): StreamedAgentDraft | undefined {
  if (typeof data !== 'object' || data === null) return undefined;
  const raw = data as Record<string, unknown>;
  if (typeof raw['name'] !== 'string' || typeof raw['instructions'] !== 'string') return undefined;
  return {
    name: raw['name'],
    description: text(raw['description']),
    instructions: raw['instructions'],
    welcome_message: text(raw['welcome_message']),
    conversation_starters: Array.isArray(raw['conversation_starters']) ? raw['conversation_starters'].filter((s): s is string => typeof s === 'string') : [],
    suggested_toolkits: list(raw['suggested_toolkits']),
    suggested_mcp: list(raw['suggested_mcp']),
    suggested_pipelines: list(raw['suggested_pipelines']),
    suggested_agents: list(raw['suggested_agents']),
    suggested_skills: list(raw['suggested_skills']),
  };
}

/** One SSE event of the draft stream as a builder event; unknown events are ignored (undefined). */
export function draftStreamEvent(event: SseEvent): DraftStreamEvent | undefined {
  const data = parseEventData(event.data);
  if (event.event === DRAFT_EVENT.token) {
    const delta = typeof data === 'object' && data !== null ? (data as { text?: unknown }).text : undefined;
    return typeof delta === 'string' ? { type: 'token', text: delta } : undefined;
  }
  if (event.event === DRAFT_EVENT.complete) {
    const draft = readDraft(data);
    return draft === undefined ? malformed() : { type: 'complete', draft };
  }
  if (event.event === DRAFT_EVENT.error) {
    const raw = typeof data === 'object' && data !== null ? (data as Record<string, unknown>) : {};
    return {
      type: 'error',
      failure: {
        code: text(raw['code']) || 'draft_failed',
        status: typeof raw['status'] === 'number' ? raw['status'] : 502,
        message: text(raw['message']) || 'The draft could not be generated.',
      },
    };
  }
  return undefined;
}

const malformed = (): DraftStreamEvent => ({ type: 'error', failure: { code: 'draft_malformed', status: 502, message: 'The server sent a draft this app cannot read.' } });

/** The builder: idle, streaming (with the text so far), a draft to review, or a failure. */
export type DraftState =
  | { readonly phase: 'idle' }
  | { readonly phase: 'streaming'; readonly text: string }
  | { readonly phase: 'review'; readonly draft: StreamedAgentDraft; readonly text: string }
  | { readonly phase: 'failed'; readonly failure: DraftStreamFailure; readonly text: string };

export const INITIAL_DRAFT_STATE: DraftState = { phase: 'idle' };

export type DraftAction = DraftStreamEvent | { readonly type: 'start' } | { readonly type: 'reset' } | { readonly type: 'ended' };

/** The failure a stream that ended without `draft.complete` or `draft.error` becomes. */
export const STREAM_CUT_FAILURE: DraftStreamFailure = { code: 'stream_cut', status: 502, message: 'The connection closed before the draft was complete. Try again.' };

/** Folds one action. A token after the end, or an end after a result, changes nothing. */
export function draftReducer(state: DraftState, action: DraftAction): DraftState {
  switch (action.type) {
    case 'start':
      return { phase: 'streaming', text: '' };
    case 'reset':
      return INITIAL_DRAFT_STATE;
    case 'token':
      return state.phase === 'streaming' ? { phase: 'streaming', text: state.text + action.text } : state;
    case 'complete':
      return state.phase === 'streaming' ? { phase: 'review', draft: action.draft, text: state.text } : state;
    case 'error':
      return state.phase === 'streaming' ? { phase: 'failed', failure: action.failure, text: state.text } : state;
    case 'ended':
      return state.phase === 'streaming' ? { phase: 'failed', failure: STREAM_CUT_FAILURE, text: state.text } : state;
    default:
      return action satisfies never;
  }
}

/** The fields of a draft the model is shown when it revises one (`current_draft`). */
export interface CurrentDraft {
  readonly name: string;
  readonly description: string;
  readonly instructions: string;
  readonly welcome_message?: string;
  readonly conversation_starters?: readonly string[];
}

export interface DraftRequestInput {
  readonly request: string;
  readonly history?: readonly DraftTurn[];
  readonly current?: CurrentDraft | undefined;
}

/** The route's request body: the newest turns only, and the current draft when one is being revised. */
export function draftRequestBody(input: DraftRequestInput): Record<string, unknown> {
  const history = (input.history ?? []).filter((turn) => turn.content.trim() !== '').slice(-MAX_DRAFT_HISTORY);
  return {
    user_description: input.request,
    ...(history.length > 0 ? { conversation_history: history.map((turn) => ({ role: turn.role, content: turn.content })) } : {}),
    ...(input.current === undefined ? {} : { current_draft: input.current }),
  };
}

/**
 * Reads a whole stream of decoded events into actions, in order, and ends
 * the stream with `ended` (a no-op after a result). Hosts with an async byte
 * stream call `feed` per chunk and `finish` at the end.
 */
export interface DraftStreamReader {
  feed(events: readonly SseEvent[]): void;
  finish(): void;
}

export function draftStreamReader(dispatch: (action: DraftAction) => void): DraftStreamReader {
  return {
    feed(events) {
      for (const event of events) {
        const action = draftStreamEvent(event);
        if (action !== undefined) dispatch(action);
      }
    },
    finish() {
      dispatch({ type: 'ended' });
    },
  };
}
