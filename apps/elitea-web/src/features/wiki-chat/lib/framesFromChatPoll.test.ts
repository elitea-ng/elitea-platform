import { describe, expect, it } from 'vitest';

import { reduceChatFrames } from '../model/reducer';
import { initialChatState, isThinkingBlock, type ChatState } from '../model/types';
import { framesFromChatPoll, isTerminalChatPoll } from './framesFromChatPoll';

const CONTEXT = { streamId: 'stream-1' };

function openState(): ChatState {
  return {
    ...initialChatState,
    messages: [{ type: 'thinking_steps', id: 'block-1', status: 'running', steps: [] }],
    activeBlockId: 'block-1',
    streamId: CONTEXT.streamId,
    isLoading: true,
  };
}

describe('framesFromChatPoll', () => {
  it('routes an event message to response_metadata, where the reducer looks', () => {
    // THE WHOLE POINT OF THIS ADAPTER. `content` is where the GENERATION
    // adapter puts it, and a structured event routed there degrades into a raw
    // JSON log line with nothing reporting it.
    const [frame] = framesFromChatPoll(
      { status: 'InProgress', custom_events: [{ data: { message: 'Reading files' } }] },
      CONTEXT,
    );
    expect(frame?.response_metadata?.['message']).toBe('Reading files');
    expect(frame?.content).toBeUndefined();
  });

  it('carries a structured event all the way to a tool card', () => {
    // The end-to-end statement: adapter plus reducer produce the card, so a
    // regression in EITHER of them fails here.
    const frames = framesFromChatPoll(
      {
        status: 'InProgress',
        custom_events: [
          {
            data: {
              message: JSON.stringify({
                event: 'tool_start',
                data: { id: 't-1', tool: 'search', input: 'router' },
              }),
            },
          },
        ],
      },
      CONTEXT,
    );

    const { state } = reduceChatFrames(openState(), frames);
    const block = state.messages.find(isThinkingBlock);
    expect(block?.steps).toHaveLength(1);
    expect(block?.steps[0]).toMatchObject({ id: 't-1', event: 'tool_start' });
    // Not the degraded reading: a log card holding the raw JSON.
    expect(block?.steps[0]?.event).not.toBe('log');
  });

  it('drops an event with no message rather than emitting a Processing card', () => {
    expect(
      framesFromChatPoll(
        { status: 'InProgress', custom_events: [{ data: {} }, { data: { message: '' } }, {}] },
        CONTEXT,
      ),
    ).toEqual([]);
  });

  it('stamps the stream id so the reducer can filter on it', () => {
    const frames = framesFromChatPoll(
      { status: 'Completed', result: 'done', custom_events: [{ data: { message: 'a' } }] },
      CONTEXT,
    );
    for (const frame of frames) {
      expect(frame.response_metadata?.['stream_id']).toBe('stream-1');
    }
  });

  it('passes a completed result through unparsed', () => {
    const envelope = JSON.stringify([{ object_type: 'message', data: 'The answer' }]);
    const [frame] = framesFromChatPoll({ status: 'Completed', result: envelope }, CONTEXT);
    expect(frame?.type).toBe('agent_response');
    expect(frame?.content).toBe(envelope);

    // And the reducer, not this adapter, is what reads it.
    const { state } = reduceChatFrames(openState(), [frame!]);
    expect(state.messages.at(-1)).toMatchObject({ content: 'The answer' });
  });

  it('turns Error and Stopped into an error frame, keeping the category', () => {
    for (const status of ['Error', 'Stopped']) {
      const [frame] = framesFromChatPoll(
        { status, message: 'no slots', error_category: 'service_busy' },
        CONTEXT,
      );
      expect(frame?.type).toBe('error');
      expect(frame?.content).toBe('no slots');
      expect(frame?.response_metadata?.['error_category']).toBe('service_busy');
    }
  });

  it('emits the events BEFORE the terminal frame', () => {
    // Order is the contract: the reducer closes the thinking block on the
    // terminal frame, and a step arriving after that is discarded.
    const frames = framesFromChatPoll(
      { status: 'Completed', result: 'done', custom_events: [{ data: { message: 'last step' } }] },
      CONTEXT,
    );
    expect(frames.map((frame) => frame.type)).toEqual(['agent_thinking_step', 'agent_response']);

    const { state } = reduceChatFrames(openState(), frames);
    expect(state.messages.find(isThinkingBlock)?.steps).toHaveLength(1);
  });

  it('emits nothing for a poll that is still running', () => {
    expect(framesFromChatPoll({ status: 'InProgress' }, CONTEXT)).toEqual([]);
    expect(framesFromChatPoll(undefined, CONTEXT)).toEqual([]);
  });
});

/**
 * The ANSWER TOKEN channel (issue #701).
 *
 * Every body below is the browser hop of
 * `conformance/provider/fixtures/deepwiki/stream/token_events.json`, which the
 * Python engine and the Go host answer to as well
 * (`services/elitea-deepwiki/tests/unit/test_answer_tokens.py`,
 * `services/elitea-subapp-host/internal/apps/deepwiki/run/stream_test.go`).
 */
describe('framesFromChatPoll: answer tokens', () => {
  /** One fragment, in the envelope the host writes. */
  function token(text: string) {
    return { data: { message: JSON.stringify({ event: 'llm_chunk', data: { text } }) } };
  }

  it('turns an llm_chunk event into a chunk frame carrying the fragment in content', () => {
    // `content`, not `response_metadata.message`: that is where the reducer's
    // accumulator reads a fragment from. Put in the metadata instead, the
    // reducer would treat it as an UNKNOWN structured event and SHOW it as a
    // thinking card — the answer rendered as a column of log lines, and
    // nothing failing anywhere.
    const [frame] = framesFromChatPoll(
      { status: 'InProgress', custom_events: [token('The wiki pages ')] },
      CONTEXT,
    );
    expect(frame?.type).toBe('agent_llm_chunk');
    expect(frame?.content).toBe('The wiki pages ');
    expect(frame?.response_metadata?.['message']).toBeUndefined();
    expect(frame?.response_metadata?.['stream_id']).toBe('stream-1');
  });

  it('keeps tokens and progress interleaved in the order the provider sent them', () => {
    // The golden sequence, hop three. An adapter that collected the tokens
    // and appended them after the progress would pass every count and put an
    // answer that arrived around a tool call in the wrong place.
    const frames = framesFromChatPoll(
      {
        status: 'InProgress',
        custom_events: [
          { data: { message: 'Searching the wiki index' } },
          token('The wiki pages '),
          token('live in the '),
          { data: { message: 'Composing the answer' } },
          token('wiki-artifacts bucket.'),
        ],
      },
      CONTEXT,
    );

    expect(frames.map((frame) => frame.type)).toEqual([
      'agent_thinking_step',
      'agent_llm_chunk',
      'agent_llm_chunk',
      'agent_thinking_step',
      'agent_llm_chunk',
    ]);

    const { state } = reduceChatFrames(openState(), frames);
    expect(state.streamingText).toBe('The wiki pages live in the wiki-artifacts bucket.');
    // The progress still reaches the thinking log, and the fragments do not.
    expect(state.messages.find(isThinkingBlock)?.steps.map((step) => step.message)).toEqual([
      'Searching the wiki index',
      'Composing the answer',
    ]);
  });

  it('grows the answer across polls, then replaces it when the turn completes', () => {
    // The events are READ-ONCE, so this is what the drawer actually sees:
    // three polls, each carrying only what arrived since the last one.
    let state = openState();
    for (const poll of [
      { status: 'InProgress', custom_events: [token('The wiki pages ')] },
      { status: 'InProgress', custom_events: [token('live in the bucket.')] },
    ]) {
      state = reduceChatFrames(state, framesFromChatPoll(poll, CONTEXT)).state;
    }
    expect(state.streamingText).toBe('The wiki pages live in the bucket.');

    const terminal = framesFromChatPoll(
      { status: 'Completed', result: 'The wiki pages live in the bucket.' },
      CONTEXT,
    );
    state = reduceChatFrames(state, terminal).state;
    // Cleared, because the finished answer replaces it. Leaving it would show
    // the same text twice.
    expect(state.streamingText).toBe('');
    expect(state.messages.at(-1)).toMatchObject({
      role: 'assistant',
      content: 'The wiki pages live in the bucket.',
    });
  });

  it('keeps the partial answer when the turn fails', () => {
    // DWIKI-012's criterion: an interrupted stream leaves what arrived on
    // screen. This is the one rule that makes the channel worth having when
    // the run does NOT finish.
    let state = openState();
    state = reduceChatFrames(
      state,
      framesFromChatPoll({ status: 'InProgress', custom_events: [token('half an ans')] }, CONTEXT),
    ).state;
    state = reduceChatFrames(
      state,
      framesFromChatPoll({ status: 'Stopped', message: 'stopped by the user' }, CONTEXT),
    ).state;
    expect(state.streamingText).toBe('half an ans');
    expect(state.messages.at(-1)).toMatchObject({ isError: true });
  });

  it('shows an llm_chunk it cannot read as a step instead of swallowing it', () => {
    // A fragment that is not text, or is empty, is not appended — appending
    // `undefined` to a live answer is worse than showing the envelope — but
    // the event still HAPPENED, so it takes the thinking path rather than
    // disappearing.
    const frames = framesFromChatPoll(
      {
        status: 'InProgress',
        custom_events: [
          { data: { message: JSON.stringify({ event: 'llm_chunk', data: { text: 42 } }) } },
          { data: { message: JSON.stringify({ event: 'llm_chunk', data: {} }) } },
        ],
      },
      CONTEXT,
    );
    expect(frames.map((frame) => frame.type)).toEqual([
      'agent_thinking_step',
      'agent_thinking_step',
    ]);
    const { state } = reduceChatFrames(openState(), frames);
    expect(state.streamingText).toBe('');
  });

  it('leaves a progress message that merely looks like JSON on the progress path', () => {
    // The `event` KEY is what makes a structured event, not a successful
    // parse. This is the same rule the reducer applies, and it is the rule
    // because both sides read the envelope now.
    const [frame] = framesFromChatPoll(
      { status: 'InProgress', custom_events: [{ data: { message: '{"text":"not an event"}' } }] },
      CONTEXT,
    );
    expect(frame?.type).toBe('agent_thinking_step');
  });
});

describe('isTerminalChatPoll', () => {
  it('treats only Started and InProgress as still running', () => {
    expect(isTerminalChatPoll({ status: 'Started' })).toBe(false);
    expect(isTerminalChatPoll({ status: 'InProgress' })).toBe(false);
    expect(isTerminalChatPoll({ status: 'Completed' })).toBe(true);
    expect(isTerminalChatPoll({ status: 'Error' })).toBe(true);
    expect(isTerminalChatPoll({ status: 'Stopped' })).toBe(true);
  });

  it('does not call a missing status terminal', () => {
    // A poll that failed to parse must not end the run: the loop would stop
    // and the answer would never arrive, with the spinner already gone.
    expect(isTerminalChatPoll(undefined)).toBe(false);
    expect(isTerminalChatPoll({})).toBe(false);
  });
});
