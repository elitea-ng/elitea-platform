import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { EventStreamParser } from '../stream';
import { INITIAL_DRAFT_STATE, MAX_DRAFT_HISTORY, draftReducer, draftRequestBody, draftStreamReader, networkFailure, readDraft, recordTurn, refusalFailure, refusalFailureFromText, type DraftAction, type DraftState } from './draftStream';

const complete = {
  name: 'Incident Triager',
  description: 'Sorts incidents.',
  instructions: 'Assign a severity.',
  welcome_message: '',
  conversation_starters: ['Triage this'],
  suggested_toolkits: [],
  suggested_mcp: [],
  suggested_pipelines: [],
  suggested_agents: [],
  suggested_skills: [],
};

function run(wire: string, chunk = 7): DraftState {
  let state = draftReducer(INITIAL_DRAFT_STATE, { type: 'start' });
  const reader = draftStreamReader((action: DraftAction) => {
    state = draftReducer(state, action);
  });
  const parser = new EventStreamParser();
  const bytes = new TextEncoder().encode(wire);
  for (let i = 0; i < bytes.length; i += chunk) reader.feed(parser.push(bytes.slice(i, i + chunk)));
  reader.feed(parser.end());
  reader.finish();
  return state;
}

describe('the draft stream', () => {
  it('folds tokens, then the draft, whatever the chunking', () => {
    const wire = `event: draft.token\ndata: {"text":"{\\"na"}\n\n: keepalive\n\nevent: draft.token\ndata: {"text":"me\\""}\n\nevent: draft.complete\ndata: ${JSON.stringify(complete)}\n\n`;
    for (const chunk of [1, 5, 4096]) {
      const state = run(wire, chunk);
      assert.equal(state.phase, 'review');
      if (state.phase === 'review') {
        assert.equal(state.text, '{"name"');
        assert.equal(state.draft.name, 'Incident Triager');
        assert.deepEqual(state.draft.conversation_starters, ['Triage this']);
      }
    }
  });

  it('ends on draft.error with its code and status', () => {
    const state = run('event: draft.error\ndata: {"code":"agent_draft_invalid","status":422,"message":"name (missing)"}\n\n');
    assert.deepEqual(state, { phase: 'failed', failure: { code: 'agent_draft_invalid', status: 422, message: 'name (missing)' }, text: '' });
  });

  it('turns a stream that just stops into a failure, not an endless spinner', () => {
    const state = run('event: draft.token\ndata: {"text":"{"}\n\n');
    assert.equal(state.phase, 'failed');
    if (state.phase === 'failed') assert.equal(state.failure.code, 'stream_cut');
  });

  it('refuses a draft.complete it cannot read', () => {
    const state = run('event: draft.complete\ndata: {"description":"no name"}\n\n');
    assert.equal(state.phase === 'failed' ? state.failure.code : state.phase, 'draft_malformed');
  });

  it('ignores unknown events and late frames', () => {
    let state = run(`event: draft.future\ndata: {}\n\nevent: draft.complete\ndata: ${JSON.stringify(complete)}\n\n`);
    assert.equal(state.phase, 'review');
    state = draftReducer(state, { type: 'token', text: 'late' });
    state = draftReducer(state, { type: 'ended' });
    assert.equal(state.phase, 'review');
  });
});

describe('draftRequestBody', () => {
  it('sends only the newest non-blank turns and the current draft', () => {
    const history = Array.from({ length: 30 }, (_, i) => ({ role: 'user' as const, content: i === 29 ? ' ' : `turn ${String(i)}` }));
    const body = draftRequestBody({ request: 'rename it', history, current: { name: 'Old', description: 'd', instructions: 'i' } });
    const turns = body['conversation_history'] as { content: string }[];
    assert.equal(turns.length, MAX_DRAFT_HISTORY);
    assert.equal(turns.at(-1)?.content, 'turn 28');
    assert.deepEqual(body['current_draft'], { name: 'Old', description: 'd', instructions: 'i' });
    assert.deepEqual(draftRequestBody({ request: 'x' }), { user_description: 'x' });
  });
});

describe('refusals and turns', () => {
  it('reads a refusal from its JSON body, else from the status', () => {
    assert.deepEqual(refusalFailure(503, { error: 'no gateway', code: 'llm_not_configured' }), { code: 'llm_not_configured', status: 503, message: 'no gateway' });
    assert.deepEqual(refusalFailureFromText(403, 'not json'), { code: 'http_403', status: 403, message: 'The draft could not be generated. Try again.' });
    assert.equal(refusalFailureFromText(400, '{"error":"bad"}').message, 'bad');
    assert.equal(networkFailure(new Error('offline')).message, 'offline');
    assert.equal(networkFailure('x').code, 'network');
  });

  it('records a request and the draft it produced as two turns', () => {
    const turns = recordTurn([{ role: 'user', content: 'a' }], 'make it', { name: 'N', description: 'D' });
    assert.deepEqual(turns.slice(1), [{ role: 'user', content: 'make it' }, { role: 'assistant', content: 'N: D' }]);
  });

  it('reads a one-shot draft like a streamed one', () => {
    assert.equal(readDraft(complete)?.name, 'Incident Triager');
    assert.equal(readDraft({ description: 'x' }), undefined);
  });
});
