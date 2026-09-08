/**
 * predictFrame.utils.ts — ONE STREAM FRAME APPLIED TO THE TRANSCRIPT.
 *
 * It was a `switch` inside `useChat`. It moved out for two reasons and neither
 * is line count alone: the decision it makes is pure — a frame and a message
 * list in, a message list out — and it is the half of this widget that a wrong
 * assumption can silence completely, so it deserves to be readable and testable
 * on its own.
 *
 * # The assumption that was wrong
 *
 * The reference opens the assistant bubble on `start_task` and reads answer
 * text out of `chunk`. Both are true of its socket.io worker and NEITHER is
 * true here. Measured on the live standalone stack, one support turn streams:
 *
 *     agent_start → agent_on_transitional_edge → agent_llm_start
 *                 → agent_llm_chunk × N  (the answer, one token per frame)
 *
 * No `start_task`. No `chunk`. So the old reducer created no message, then
 * matched every later frame by id against a message that did not exist, and
 * dropped the whole answer. The widget accepted the question, the server ran
 * the agent, the answer landed in the database — and the panel stayed empty.
 *
 * The main chat surface never had the defect: its reducer appends a message
 * whenever the frame's id matches none (`features/chat-messages/lib/
 * chatStreamTurnFrames.ts`). This applies the same rule in this widget's shape.
 */
import { MESSAGE_TYPES } from '../constants';
import type { TMessage, TSocketMessage } from '../types';

/**
 * The progress label a frame puts under the spinner. The empty string means
 * "this frame says nothing about progress".
 */
export function statusForFrameType(type: string): string {
  if (type === MESSAGE_TYPES.START_TASK || type === MESSAGE_TYPES.AGENT_START) return 'Starting up...';
  if (type === MESSAGE_TYPES.AGENT_LLM_START) return 'Looking things up...';
  if (type === MESSAGE_TYPES.AGENT_TOOL_START) return 'Consulting knowledge base...';
  if (type === MESSAGE_TYPES.AGENT_LLM_CHUNK) return 'Writing response...';
  return '';
}

/**
 * Open the turn's assistant bubble if no frame has opened it yet.
 *
 * Idempotent by message id, so the several frames that can be "first" cannot
 * produce several bubbles for one turn.
 */
function openAssistantMessage(
  previous: TMessage[],
  messageId: string,
  statusMessage: string,
): TMessage[] {
  if (previous.some(m => m.id === messageId)) return previous;
  return [
    ...previous,
    {
      id: messageId,
      role: 'assistant' as const,
      content: '',
      timestamp: Date.now(),
      isStreaming: true,
      statusMessage,
    },
  ];
}

/** Apply one stream frame. An unrecognised frame returns the list unchanged. */
export function applyPredictFrame(previous: TMessage[], message: TSocketMessage): TMessage[] {
  const { message_id, type, content, response_metadata } = message;
  const status = statusForFrameType(type);

  switch (type) {
    // The turn opens. Any of these can be the first frame the client sees.
    case MESSAGE_TYPES.START_TASK:
    case MESSAGE_TYPES.AGENT_START:
    case MESSAGE_TYPES.AGENT_LLM_START:
    case MESSAGE_TYPES.AGENT_TOOL_START:
      return openAssistantMessage(previous, message_id, status).map(m =>
        m.id === message_id && m.isStreaming ? { ...m, statusMessage: status } : m,
      );

    // Bookkeeping. Named rather than defaulted, so an unhandled frame stays
    // visibly unhandled.
    case MESSAGE_TYPES.AGENT_TOOL_END:
    case MESSAGE_TYPES.AGENT_LLM_END:
    case MESSAGE_TYPES.AGENT_ON_TRANSITIONAL_EDGE:
    case MESSAGE_TYPES.AGENT_ON_FUNCTION_TOOL_NODE:
      return previous;

    /*
     * THE THREE CHUNK FLAVOURS ARE ONE BEHAVIOUR: append the delta.
     *
     * `agent_llm_chunk` used to set a status label and DROP its content. This
     * platform's workers stream the answer itself in that field, so dropping it
     * dropped the answer. A chunk that carries no content is still a progress
     * ping, so it keeps the label instead of clearing it.
     */
    case MESSAGE_TYPES.CHUNK:
    case MESSAGE_TYPES.AI_MESSAGE_CHUNK:
    case MESSAGE_TYPES.AGENT_LLM_CHUNK: {
      const chunk =
        typeof content === 'string' ? content : content == null ? '' : JSON.stringify(content);
      const finished = !!response_metadata?.finish_reason;
      return openAssistantMessage(previous, message_id, status).map(m =>
        m.id === message_id
          ? {
              ...m,
              content: m.content + chunk,
              statusMessage: chunk === '' ? status : undefined,
              ...(finished && { isStreaming: false }),
            }
          : m,
      );
    }

    // The whole answer, replacing whatever the chunks built.
    case MESSAGE_TYPES.AGENT_RESPONSE: {
      const responseContent = typeof content === 'string' ? content : JSON.stringify(content);
      return previous.map(m =>
        m.id === message_id
          ? {
              ...m,
              content: responseContent,
              isStreaming: false,
              isAnimating: true,
              statusMessage: undefined,
            }
          : m,
      );
    }

    case MESSAGE_TYPES.PIPELINE_FINISH:
      return previous.map(m =>
        m.id === message_id && m.isStreaming
          ? { ...m, isStreaming: false, statusMessage: undefined }
          : m,
      );

    case MESSAGE_TYPES.ERROR:
    case MESSAGE_TYPES.AGENT_EXCEPTION:
      return previous.map(m =>
        m.id === message_id
          ? {
              ...m,
              content: typeof content === 'string' ? content : 'An error occurred',
              isStreaming: false,
              isAnimating: false,
              isError: true,
              statusMessage: undefined,
            }
          : m,
      );

    default:
      return previous;
  }
}
