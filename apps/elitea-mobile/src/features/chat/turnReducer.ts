import type { Frame, NodeEvent } from '@/features/chat/frames';
import {
  isToolFrame,
  mergeFanout,
  pauseOf,
  reduceToolFrame,
  type ToolStep,
  type TurnPause,
} from '@/features/chat/toolActivity';

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
  /** Paused for the user: an approval, an answer, or a toolkit sign-in (`pause`). */
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
  /** The tool calls of this turn, in start order (memory only, capped I/O). */
  tools: ToolStep[];
  /**
   * What the turn waits for. Terminal pauses set phase `paused`; a fan-out
   * child's approval and the output-limit offer are recorded while the turn
   * goes on.
   */
  pause: TurnPause | null;
}

export const INITIAL_TURN: TurnState = {
  phase: 'sending',
  content: '',
  status: null,
  error: null,
  refreshRequests: 0,
  unknownFrames: 0,
  paragraphPending: false,
  tools: [],
  pause: null,
};
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
    case 'agent_requires_confirmation':
      // The answer stopped at the model's output limit; it still settles.
      return { ...streaming, pause: pauseOf(node) };
    default:
      break;
  }
  if (isToolFrame(node.type)) {
    const tools = reduceToolFrame(streaming.tools, node);
    if (node.type === 'agent_tool_start') {
      const started = tools.find((t) => t.status === 'running' && t.id === tools[tools.length - 1]?.id);
      const label = started?.displayName ?? started?.name;
      return { ...streaming, tools, status: label ? `Using ${label}…` : 'Using a tool…' };
    }
    const running = tools.some((t) => t.status === 'running');
    return { ...streaming, tools, status: running ? streaming.status : null };
  }
  if (FAILURE_TYPES.has(node.type)) {
    return {
      ...streaming,
      phase: 'failed',
      status: null,
      error: { message: failureText(node.content), retryable: true },
    };
  }
  if (node.type === 'agent_hitl_interrupt' || node.type === 'mcp_authorization_required') {
    const tools = node.type === 'mcp_authorization_required' ? reduceToolFrame(streaming.tools, node) : streaming.tools;
    const pause = pauseOf(node);
    const merged = pause === null ? streaming.pause : mergeFanout(streaming.pause, pause);
    if (isTerminalPause(node)) return { ...streaming, tools, pause: merged, phase: 'paused', status: null };
    return { ...streaming, tools, pause: merged };
  }
  if (PROGRESS_TYPES.has(node.type) || node.type.startsWith('agent_')) return streaming;
  return { ...streaming, unknownFrames: streaming.unknownFrames + 1 };
}
