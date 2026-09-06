/**
 * Turn one invocation poll into the frames the CHAT reducer consumes.
 *
 * A sibling of `features/wiki-generation/lib/framesFromPoll.ts`, and NOT a copy
 * of it. The two adapters feed reducers that read the same field differently,
 * and getting that wrong is silent:
 *
 *   generation puts the event text in `content`
 *   chat        puts it in `response_metadata.message`
 *
 * The chat reducer looks for a STRUCTURED EVENT — a JSON string carrying
 * `{event, data}` — in `response_metadata.message`, and only in there. Route a
 * `tool_start` through `content` instead and it does not fail: it falls out of
 * the structured path into the plain-text one and renders as a log line. Every
 * tool card, every research plan and every "thinking" chip would quietly
 * degrade into a list of raw JSON strings, with nothing logged on either side.
 *
 * That is why this file exists rather than an import. The two features cannot
 * import each other anyway — `no-sideways-features` — but the reason to keep
 * them apart is that they are genuinely different adapters, not that a rule
 * says so.
 *
 * EVENTS ARE READ-ONCE. `custom_events` arrives only on the poll that drained
 * it. Every event must become a frame HERE or it is gone.
 *
 * TWO CHANNELS SHARE THE EVENT LIST. The poll envelope is frozen
 * (`conformance/provider/fixtures/deepwiki/spi/custom_events.json`) as
 * `{"custom_events": [{"data": {"message": str}}]}`, so the answer's tokens
 * travel inside the same `message` string as a STRUCTURED EVENT —
 * `{"event":"llm_chunk","data":{"text":"…"}}` — the way `todo_update` and
 * `tool_start` already do. This file separates them again: an `llm_chunk`
 * becomes an `agent_llm_chunk` frame carrying the fragment in `content`, and
 * everything else becomes a thinking step. See
 * `conformance/provider/fixtures/deepwiki/stream/token_events.json`, which the
 * engine, the host and this adapter all answer to (issue #701).
 *
 * ORDER IS THE POINT of doing it here rather than one layer down. Progress and
 * tokens arrive interleaved in one list, and the frames must keep that order:
 * an answer that arrives around a tool call is only readable if the two are
 * not sorted apart.
 *
 * A TOKEN MUST NOT REACH THE REDUCER AS A THINKING FRAME. The reducer reads a
 * structured event out of `response_metadata.message`, and its `default:`
 * branch SHOWS an event it has no reading for as one more card. So an
 * `llm_chunk` left on the thinking path does not fail: it renders the answer,
 * fragment by fragment, as a list of thinking cards, and nothing anywhere
 * reports a problem.
 */
import {
  drainEventMessages,
  isTerminalPoll,
  terminalOutcome,
  type InvocationPoll,
} from '@/entities/provider-run';
import { field, structuredEvent } from '../model/frames/shared';
import { ChatFrameType, type ChatFrame } from '../model/types';

/** The structured event name that carries one fragment of the answer. */
const TOKEN_EVENT = 'llm_chunk';

// The envelope is the run entity's (ADR-0023 d4); the chat names stay for
// this adapter's callers and tests.
export type ChatInvocationPoll = InvocationPoll;
export const isTerminalChatPoll = isTerminalPoll;

/** The identifier echoed onto every synthesised frame. */
export interface ChatPollContext {
  readonly streamId: string;
}

export function framesFromChatPoll(
  poll: ChatInvocationPoll | undefined,
  context: ChatPollContext,
): ChatFrame[] {
  const metadataBase = { stream_id: context.streamId };
  const frames: ChatFrame[] = [];

  for (const message of drainEventMessages(poll)) {
    const fragment = tokenFragment(message);
    if (fragment !== null) {
      frames.push({
        type: ChatFrameType.AgentLlmChunk,
        // In `content`, because that is where the reducer's accumulator
        // reads a fragment from. The metadata carries the stream id only.
        content: fragment,
        response_metadata: { ...metadataBase },
      });
      continue;
    }
    frames.push({
      type: ChatFrameType.AgentThinkingStep,
      response_metadata: { ...metadataBase, message },
    });
  }

  const terminal = terminalFrame(poll, metadataBase);
  if (terminal) frames.push(terminal);

  return frames;
}

/**
 * The answer fragment one event carries, or null when it carries none.
 *
 * A NON-STRING `text` IS NOT A FRAGMENT, and an empty one is not either. Both
 * come back as null and take the thinking path, so the event is still shown as
 * a step rather than swallowed: an `llm_chunk` whose payload this build cannot
 * read is something happening that the reader should see, and appending
 * `undefined` to a live answer would be worse than showing the envelope.
 */
function tokenFragment(message: unknown): string | null {
  const event = structuredEvent(message);
  if (event === null || event.event !== TOKEN_EVENT) return null;
  const text = field(event.data, 'text');
  return typeof text === 'string' && text !== '' ? text : null;
}

function terminalFrame(
  poll: ChatInvocationPoll | undefined,
  metadataBase: { stream_id: string },
): ChatFrame | null {
  const outcome = terminalOutcome(poll, 'The request failed.');
  if (outcome === null) return null;
  if (outcome.kind === 'completed') {
    return {
      type: ChatFrameType.AgentResponse,
      // The result is passed through UNPARSED. The reducer knows five shapes it
      // can arrive in — a bare string, a JSON envelope, the platform's result
      // array — and picking one here would decide for it.
      content: outcome.result,
      response_metadata: { ...metadataBase, status: outcome.status },
    };
  }
  return {
    type: ChatFrameType.Error,
    content: outcome.message,
    response_metadata: {
      ...metadataBase,
      status: outcome.status,
      error_category: outcome.errorCategory,
      error_type: outcome.errorType,
    },
  };
}
