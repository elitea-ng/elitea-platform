import { staticPauseFromFrame } from './chatStreamStaticPause';
/**
 * lib/chatStreamTurnEnd.ts — the predicate that ends a turn.
 *
 * The stream transport reads it to know when it stops owning a run. The file
 * is separate because §3.5 caps a file at 400 lines with no warning tier.
 */
import { isTerminalPauseFrame } from './chatStreamInterruptFrames';
import { SocketMessageType, type ChatStreamFrame } from './chatStreamFrame';
import { isFullMessageResultFrame, isRootAnswerFrame } from './chatStreamFinalResult';

/**
 * The frames on which the run FAILED.
 *
 * The reducer settles the message on each of them. The worker sends no more
 * node events after a failure.
 */
const TURN_FAILURE_TYPES: ReadonlySet<string> = new Set<string>([
  SocketMessageType.Error,
  SocketMessageType.LlmError,
  SocketMessageType.AgentException,
]);

/**
 * Is this the last frame of the turn?
 *
 * The predicate tells the transport when it stops owning a run. The reducer,
 * not this predicate, owns what a frame does to the message.
 *
 * A turn ends in one of four ways:
 * - `pipeline_finish` ends the whole execution.
 * - `agent_response` WITH a `finish_reason` carries the model's stop reason.
 *   A response without one is an intermediate answer in a multi-step pipeline.
 *   It must not end the stream.
 * - a failure frame (`error`, `llm_error`, `agent_exception`).
 * - a terminal PAUSE. A paused run sends no `pipeline_finish` and no
 *   `agent_response`. Without this arm the connection stays open for ever.
 *   `isStreaming` stays true, the composer stays disabled, and the stream
 *   holds a server admission slot. `isTerminalPauseFrame` separates a terminal
 *   pause from a mid-run one. Read its own comment.
 */
export function isTurnTerminalFrame(frame: ChatStreamFrame): boolean {
  if (!isRootAnswerFrame(frame)) return false;
  if (isFullMessageResultFrame(frame)) return true;
  if (frame.type === SocketMessageType.PipelineFinish) return true;
  if (frame.type === SocketMessageType.AgentResponse) return Boolean(frame.response_metadata?.finish_reason);
  if (frame.type !== undefined && TURN_FAILURE_TYPES.has(frame.type)) return true;
  return isTerminalPauseFrame(frame) || staticPauseFromFrame(frame) !== undefined;
}

/** Durable workers publish success progress before the result-bearing frame. */
export function isObserverTerminalFrame(frame: ChatStreamFrame, generation?: string): boolean {
  if (generation && (frame.type === SocketMessageType.PipelineFinish || frame.type === SocketMessageType.AgentResponse)) return false;
  return isTurnTerminalFrame(frame);
}
