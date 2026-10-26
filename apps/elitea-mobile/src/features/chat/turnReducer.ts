import { ANSWER_DELTA_TYPES, FRAME_TYPE, TURN_FAILURE_TYPES, mergeSteeringAll, type SteeringItem } from '@elitea/client-core/stream';

import type { Frame, NodeEvent } from '@/features/chat/frames';
import { budgetRefusalText, budgetScopeOf, type BudgetScope } from '@/features/usage/budgetRefusal';
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
  /** Set when a model budget refused the turn (the thread offers the usage card). */
  budget?: BudgetScope;
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
  /**
   * The run was stopped (client contract 1.3 stop, from this device or
   * another): the server ends the stream with `execution.failed`
   * `CANCELLED`. It settles like a finished answer, keeping its partial text.
   */
  stopped: boolean;
  /**
   * The steering messages of this turn (client contract 1.26), in queue
   * order: from the `steer.*` frames and from the steering calls' answers.
   */
  steering: SteeringItem[];
}

/**
 * Folds steering messages into the turn, also after it ended (the REST
 * answers and the list read at the end come after the terminal frame).
 */
export function withSteering(state: TurnState, items: readonly SteeringItem[]): TurnState {
  if (items.length === 0) return state;
  return { ...state, steering: mergeSteeringAll(state.steering, items) };
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
  stopped: false,
  steering: [],
};
/** The `execution.failed` code of a run that was stopped. */
export const CANCELLED_CODE = 'CANCELLED';
export const GENERIC_FAILURE = 'The agent could not finish this reply.';
const MAX_ERROR_TEXT = 300;

const PROGRESS_TYPES: ReadonlySet<string> = new Set<string>([FRAME_TYPE.AgentStart, FRAME_TYPE.StartTask, FRAME_TYPE.AgentLlmStart, FRAME_TYPE.AgentLlmEnd]);

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
  if (node.type === FRAME_TYPE.McpAuthorizationRequired) {
    return Array.isArray(node.response_metadata?.authorization_requests);
  }
  return node.type === FRAME_TYPE.AgentHitlInterrupt && !isFanoutChildPause(node);
}

/** Is this the last frame of the turn (the client must then close the stream)? */
export function isTurnTerminal(frame: Frame): boolean {
  if (frame.kind === 'failed') return true;
  if (frame.kind !== 'node') return false;
  const { node } = frame;
  if (node.type === FRAME_TYPE.PipelineFinish) return true;
  if (node.type === FRAME_TYPE.AgentResponse) return Boolean(node.response_metadata?.finish_reason);
  if (TURN_FAILURE_TYPES.has(node.type)) return true;
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
  if (frame.kind === 'steer') return withSteering(state, [frame.item]);
  // A settled turn ignores late frames.
  if (state.phase === 'done' || state.phase === 'failed' || state.phase === 'paused') return state;

  switch (frame.kind) {
    case 'failed':
      if (frame.failure.code === CANCELLED_CODE) {
        return { ...state, phase: 'done', status: null, error: null, stopped: true };
      }
      {
        const budget = budgetScopeOf(frame.failure.code);
        if (budget !== null) {
          // A policy outcome, not a fault: a plain message, and retrying cannot help.
          return { ...state, phase: 'failed', status: null, error: { message: budgetRefusalText(budget), retryable: false, budget } };
        }
      }
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

  if (ANSWER_DELTA_TYPES.has(node.type)) {
    return appendDelta(streaming, textOf(node.content));
  }
  switch (node.type) {
    case FRAME_TYPE.AgentResponse: {
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
    case FRAME_TYPE.PipelineFinish: {
      const text = textOf(node.content);
      return {
        ...streaming,
        phase: 'done',
        status: null,
        content: streaming.content === '' && text !== '' ? text : streaming.content,
      };
    }
    case FRAME_TYPE.AgentRequiresConfirmation:
      // The answer stopped at the model's output limit; it still settles.
      return { ...streaming, pause: pauseOf(node) };
    default:
      break;
  }
  if (isToolFrame(node.type)) {
    const tools = reduceToolFrame(streaming.tools, node);
    if (node.type === FRAME_TYPE.AgentToolStart) {
      const started = tools.find((t) => t.status === 'running' && t.id === tools[tools.length - 1]?.id);
      const label = started?.displayName ?? started?.name;
      return { ...streaming, tools, status: label ? `Using ${label}…` : 'Using a tool…' };
    }
    const running = tools.some((t) => t.status === 'running');
    return { ...streaming, tools, status: running ? streaming.status : null };
  }
  if (TURN_FAILURE_TYPES.has(node.type)) {
    return {
      ...streaming,
      phase: 'failed',
      status: null,
      error: { message: failureText(node.content), retryable: true },
    };
  }
  if (node.type === FRAME_TYPE.AgentHitlInterrupt || node.type === FRAME_TYPE.McpAuthorizationRequired) {
    const tools = node.type === FRAME_TYPE.McpAuthorizationRequired ? reduceToolFrame(streaming.tools, node) : streaming.tools;
    const pause = pauseOf(node);
    const merged = pause === null ? streaming.pause : mergeFanout(streaming.pause, pause);
    if (isTerminalPause(node)) return { ...streaming, tools, pause: merged, phase: 'paused', status: null };
    return { ...streaming, tools, pause: merged };
  }
  if (PROGRESS_TYPES.has(node.type) || node.type.startsWith('agent_')) return streaming;
  return { ...streaming, unknownFrames: streaming.unknownFrames + 1 };
}
