import { EventStreamParser } from '@/api/sse/parser';
import { loadFixtureBytes, type FixtureName } from '@/features/chat/__fixtures__/loadFixture';
import { parseFrame, type Frame } from '@/features/chat/frames';
import {
  GENERIC_FAILURE,
  INITIAL_TURN,
  PAUSED_STATUS,
  isTurnTerminal,
  reduceTurn,
  type TurnState,
} from '@/features/chat/turnReducer';

function framesOf(name: FixtureName): Frame[] {
  const parser = new EventStreamParser();
  return [...parser.push(loadFixtureBytes(name)), ...parser.end()].map(parseFrame);
}

/** Folds frames the way the client does: stop at the first terminal frame. */
function play(name: FixtureName): { state: TurnState; consumed: number; total: number; history: TurnState[] } {
  const frames = framesOf(name);
  let state = INITIAL_TURN;
  const history: TurnState[] = [];
  let consumed = 0;
  for (const frame of frames) {
    state = reduceTurn(state, frame);
    history.push(state);
    consumed += 1;
    if (isTurnTerminal(frame)) break;
  }
  return { state, consumed, total: frames.length, history };
}

describe('turnReducer over fixtures', () => {
  it('accumulates deltas, then replaces with the final whole reply', () => {
    const { state, history, consumed, total } = play('simple-answer');
    expect(history.map((s) => s.content)).toContain('Hello, ');
    expect(state).toMatchObject({ phase: 'done', content: 'Hello, café ☕ world.', error: null });
    // The final agent_response is terminal: pipeline_finish after it is never needed.
    expect(consumed).toBe(total - 1);
  });

  it('shows a tool status line while a tool runs', () => {
    const { state, history } = play('tool-call');
    expect(history.some((s) => s.status === 'Using web_search…')).toBe(true);
    expect(state).toMatchObject({ phase: 'done', status: null, content: 'Found two results.' });
  });

  it('keeps going after an intermediate response and separates the paragraphs', () => {
    const { state } = play('intermediate-response');
    expect(state).toMatchObject({ phase: 'done', content: 'Step one done.\n\nFinal answer.' });
  });

  it('uses execution.failed safe_message and retryable', () => {
    const { state } = play('execution-failed');
    expect(state).toMatchObject({
      phase: 'failed',
      content: 'Partial ',
      error: { message: 'The execution deadline was exceeded.', retryable: true },
    });
  });

  it('ends on llm_error', () => {
    const { state, consumed, total } = play('llm-error');
    expect(state.phase).toBe('failed');
    expect(state.error?.message).toBe('[provider error text]');
    expect(consumed).toBe(total);
  });

  it('treats a run-level HITL pause as terminal', () => {
    const { state, consumed, total } = play('hitl-terminal');
    expect(state).toMatchObject({ phase: 'paused', status: PAUSED_STATUS });
    expect(consumed).toBe(total);
  });

  it('does NOT end the turn on a fan-out child HITL pause', () => {
    const frames = framesOf('hitl-fanout-child');
    expect(isTurnTerminal(frames[1])).toBe(false);
    const { state, consumed, total } = play('hitl-fanout-child');
    expect(consumed).toBe(total);
    expect(state).toMatchObject({ phase: 'done', content: 'Sibling output.' });
  });

  it('ends on mcp_authorization_required only when it carries authorization_requests', () => {
    const frames = framesOf('mcp-authorization');
    expect(isTurnTerminal(frames[1])).toBe(false);
    expect(isTurnTerminal(frames[2])).toBe(true);
    expect(play('mcp-authorization').state.phase).toBe('paused');
  });

  it('asks for a REST refill on replay_reset', () => {
    const { state, history } = play('replay-reset');
    expect(history[0].refreshRequests).toBe(1);
    expect(state).toMatchObject({ phase: 'done', content: 'whole tail' });
  });
});

describe('turnReducer edge cases', () => {
  const node = (type: string, extra: Record<string, unknown> = {}): Frame => ({ kind: 'node', node: { type, ...extra } });

  it('pipeline_finish uses its content only when nothing streamed', () => {
    expect(reduceTurn(INITIAL_TURN, node('pipeline_finish', { content: 'snapshot' })).content).toBe('snapshot');
    const streamed = reduceTurn(INITIAL_TURN, node('agent_llm_chunk', { content: 'live' }));
    expect(reduceTurn(streamed, node('pipeline_finish', { content: 'other' })).content).toBe('live');
  });

  it('ignores frames after the turn settled', () => {
    const done = reduceTurn(INITIAL_TURN, node('pipeline_finish'));
    expect(reduceTurn(done, node('agent_llm_chunk', { content: 'late' }))).toBe(done);
  });

  it('counts unknown and invalid frames without failing', () => {
    let s = reduceTurn(INITIAL_TURN, node('brand_new_type'));
    s = reduceTurn(s, { kind: 'invalid', event: 'execution.node_event' });
    s = reduceTurn(s, { kind: 'other', event: 'index.ingest.completed' });
    expect(s.unknownFrames).toBe(3);
    expect(s.phase).toBe('streaming');
  });

  it('ignores non-string content and the thinking channel', () => {
    const s = reduceTurn(INITIAL_TURN, node('agent_llm_chunk', { content: { x: 1 }, thinking: 'hmm' }));
    expect(s.content).toBe('');
  });

  it('falls back to a generic message for an empty failure', () => {
    expect(reduceTurn(INITIAL_TURN, node('agent_exception')).error?.message).toBe(GENERIC_FAILURE);
    expect(reduceTurn(INITIAL_TURN, { kind: 'failed', failure: {} }).error).toEqual({
      message: GENERIC_FAILURE,
      retryable: false,
    });
  });
});

describe('parseFrame', () => {
  it('validates node events at the boundary', () => {
    expect(parseFrame({ id: '1', event: 'execution.node_event', data: 'not json' }).kind).toBe('invalid');
    expect(parseFrame({ id: '1', event: 'execution.node_event', data: '{"type":5}' }).kind).toBe('invalid');
    expect(parseFrame({ id: '1', event: 'execution.node_event', data: '{"type":"x","extra":1}' })).toEqual({
      kind: 'node',
      node: { type: 'x', extra: 1 },
    });
    expect(parseFrame({ id: '1', event: 'index.ingest.completed', data: '{}' })).toEqual({
      kind: 'other',
      event: 'index.ingest.completed',
    });
  });
});
