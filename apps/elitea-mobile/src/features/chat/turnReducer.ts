import type { Frame, NodeEvent } from '@/features/chat/frames';

/**
 * One turn's live state, folded from execution frames.
 *
 * Wire semantics (observed, see __fixtures__/README.md):
 * - `agent_llm_chunk` / `chunk` / `AIMessageChunk` carry a DELTA of the answer;
 * - `agent_response` carries a WHOLE reply: with `finish_reason` it is the
 *   final answer (and ends the turn), without it an intermediate step;
 * - the turn ends on `pipeline_finish`, a final `agent_response`, a failure
 *   (`error`, `llm_error`, `agent_exception`, `execution.failed`) or a
 *   terminal pause (a HITL interrupt that is not a fan-out child, or
 *   `mcp_authorization_required` carrying `authorization_requests`).
 */

export type TurnPhase =
  /** POST in flight (or being retried). */
  | 'sending'
  /** Execution started; waiting for or receiving frames. */
  | 'streaming'
  | 'done'
  /** Paused for something mobile cannot do (approval, MCP sign-in). */
  | 'paused'
  | 'failed';

export interface TurnError {
  message: string;
  retryable: boolean;
}

export interface TurnState {
  phase: TurnPhase;
  content: string;
  /** One-line progress ("Using web_search…"), or null. */
  status: string | null;
  error: TurnError | null;
  /** Incremented when frames were lost (replay reset): refill from REST. */
  refreshRequests: number;
  /** Unknown frame types seen (dev diagnostics). */
  unknownFrames: number;
  /** Set after an intermediate response: the next delta starts a new paragraph. */
  paragraphPending: boolean;
}

export const INITIAL_TURN: TurnState = {
  phase: 'sending',
  content: '',
  status: null,
  error: null,
  refreshRequests: 0,
  unknownFrames: 0,
  paragraphPending: false,
};

export const PAUSED_STATUS = 'Needs your approval — continue in the web app.';
export const GENERIC_FAILURE = 'The agent could not finish this reply.';
const MAX_ERROR_TEXT = 300;

const DELTA_TYPES = new Set(['agent_llm_chunk', 'chunk', 'AIMessageChunk']);
const FAILURE_TYPES = new Set(['error', 'llm_error', 'agent_exception']);
const PROGRESS_TYPES = new Set(['agent_start', 'start_task', 'agent_llm_start', 'agent_llm_end']);

function textOf(content: unknown): string {
  return typeof content === 'string' ? content : '';
}

function isFanoutChildPause(node: NodeEvent): boolean {
  const meta = node.response_metadata?.metadata ?? {};
  const parent = (meta as Record<string, unknown>).parent_agent_name;
  const child = (meta as Record<string, unknown>).child_thread_id;
  return Boolean(parent) && typeof child === 'string' && child !== '';
}

function isTerminalPause(node: NodeEvent): boolean {
  if (node.type === 'mcp_authorization_required') {
    return Array.isArray(node.response_metadata?.authorization_requests);
  }
  return node.type === 'agent_hitl_interrupt' && !isFanoutChildPause(node);
}

/** Is this the last frame of the turn (the client must then close the stream)? */
export function isTurnTerminal(frame: Frame): boolean {
  if (frame.kind === 'failed') return true;
  if (frame.kind !== 'node') return false;
  const { node } = frame;
  if (node.type === 'pipeline_finish') return true;
  if (node.type === 'agent_response') return Boolean(node.response_metadata?.finish_reason);
  if (FAILURE_TYPES.has(node.type)) return true;
  return isTerminalPause(node);
}

function appendDelta(state: TurnState, delta: string): TurnState {
  if (delta === '') return state;
  const sep = state.paragraphPending && state.content !== '' ? '\n\n' : '';
  return { ...state, content: state.content + sep + delta, paragraphPending: false };
}

function failureText(content: unknown): string {
  const text = textOf(content).trim();
  if (text === '') return GENERIC_FAILURE;
  return text.length > MAX_ERROR_TEXT ? `${text.slice(0, MAX_ERROR_TEXT)}…` : text;
}

export function reduceTurn(state: TurnState, frame: Frame): TurnState {
  // A settled turn ignores late frames.
  if (state.phase === 'done' || state.phase === 'failed' || state.phase === 'paused') return state;

  switch (frame.kind) {
    case 'failed':
      return {
        ...state,
        phase: 'failed',
        status: null,
        error: {
          message: frame.failure.safe_message?.trim() || GENERIC_FAILURE,
          retryable: frame.failure.retryable === true,
        },
      };
    case 'replay_reset':
      return { ...state, refreshRequests: state.refreshRequests + 1 };
    case 'other':
    case 'invalid':
      return { ...state, unknownFrames: state.unknownFrames + 1 };
    case 'node':
      break;
  }

  const { node } = frame;
  const streaming: TurnState = state.phase === 'sending' ? { ...state, phase: 'streaming' } : state;

  if (DELTA_TYPES.has(node.type)) {
    return appendDelta(streaming, textOf(node.content));
  }
  switch (node.type) {
    case 'agent_response': {
      const text = textOf(node.content);
      if (node.response_metadata?.finish_reason) {
        return {
          ...streaming,
          phase: 'done',
          status: null,
          content: text !== '' ? text : streaming.content,
          paragraphPending: false,
        };
      }
      if (text === '' || streaming.content.endsWith(text)) {
        return { ...streaming, paragraphPending: true };
      }
      return { ...appendDelta(streaming, text), paragraphPending: true };
    }
    case 'pipeline_finish': {
      const text = textOf(node.content);
      return {
        ...streaming,
        phase: 'done',
        status: null,
        content: streaming.content === '' && text !== '' ? text : streaming.content,
      };
    }
    case 'agent_tool_start': {
      const tool = node.response_metadata?.tool_name;
      return { ...streaming, status: tool ? `Using ${tool}…` : 'Using a tool…' };
    }
    case 'agent_tool_end':
      return { ...streaming, status: null };
    default:
      break;
  }
  if (FAILURE_TYPES.has(node.type)) {
    return {
      ...streaming,
      phase: 'failed',
      status: null,
      error: { message: failureText(node.content), retryable: true },
    };
  }
  if (isTerminalPause(node)) {
    return { ...streaming, phase: 'paused', status: PAUSED_STATUS };
  }
  if (node.type === 'mcp_authorization_required') return streaming;
  if (PROGRESS_TYPES.has(node.type) || node.type.startsWith('agent_')) return streaming;
  return { ...streaming, unknownFrames: streaming.unknownFrames + 1 };
}
