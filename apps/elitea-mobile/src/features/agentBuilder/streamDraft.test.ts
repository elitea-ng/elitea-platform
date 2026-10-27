import { CLIENT_VERSION_HEADER } from '@/api/clientVersion';
import { INITIAL_DRAFT_STATE, draftReducer, type DraftAction, type DraftState } from '@elitea/client-core/agent-builder';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { fakeStreamTransport, type ScriptedAnswer } from '@/test/fakeStreamTransport';

import { streamAgentDraft } from './streamDraft';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const ORIGIN = 'https://ai.example.com';

const DRAFT = {
  name: 'Triager',
  description: 'Triages incidents',
  instructions: 'Sort by severity.',
  welcome_message: 'What broke?',
  conversation_starters: ['Triage this'],
  suggested_toolkits: [],
  suggested_mcp: [],
  suggested_pipelines: [],
  suggested_agents: [],
  suggested_skills: [],
};

async function run(answer: ScriptedAnswer, frames: string[] = []) {
  const { transport, connections } = fakeStreamTransport(() => answer);
  let state: DraftState = draftReducer(INITIAL_DRAFT_STATE, { type: 'start' });
  const dispatch = (action: DraftAction) => {
    state = draftReducer(state, action);
  };
  const done = streamAgentDraft({
    projectId: 7,
    input: { request: 'An incident triager', history: [{ role: 'user', content: 'before' }] },
    signal: new AbortController().signal,
    dispatch,
    transport,
  });
  await Promise.resolve();
  await Promise.resolve();
  const connection = connections[0];
  if ((answer.status ?? 200) === 200 && connection) {
    for (const frame of frames) connection.push(frame);
    connection.end();
  }
  await done;
  return { state, transport, connection };
}

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
});

describe('streamAgentDraft', () => {
  it('POSTs the request with the stream headers and folds the frames into a draft to review', async () => {
    const { state, transport } = await run({}, [
      `event: draft.token\ndata: ${JSON.stringify({ text: '{"name"' })}\n\n`,
      `event: draft.complete\ndata: ${JSON.stringify(DRAFT)}\n\n`,
    ]);
    const [url, init] = transport.mock.calls[0]!;
    expect(url).toBe(`${ORIGIN}/api/v2/elitea_core/generate_application_draft_stream/prompt_lib/7`);
    expect(init.method).toBe('POST');
    expect(init.headers).toMatchObject({ Accept: 'text/event-stream', Authorization: 'Bearer tok', 'Content-Type': 'application/json' });
    expect(init.headers[CLIENT_VERSION_HEADER]).toBeTruthy();
    expect(JSON.parse(init.body ?? '{}')).toEqual({ user_description: 'An incident triager', conversation_history: [{ role: 'user', content: 'before' }] });
    expect(state).toMatchObject({ phase: 'review', text: '{"name"', draft: { name: 'Triager' } });
  });

  it('shows the stream’s draft.error', async () => {
    const { state } = await run({}, [`event: draft.error\ndata: ${JSON.stringify({ code: 'agent_draft_invalid', status: 422, message: 'No name.' })}\n\n`]);
    expect(state).toMatchObject({ phase: 'failed', failure: { code: 'agent_draft_invalid', message: 'No name.' } });
  });

  it('names a refusal before the stream opened by its JSON error', async () => {
    const { state } = await run({ status: 503, body: JSON.stringify({ error: 'the LLM gateway is not configured', code: 'llm_gateway_not_configured' }) });
    expect(state).toMatchObject({ phase: 'failed', failure: { status: 503, code: 'llm_gateway_not_configured', message: 'the LLM gateway is not configured' } });
  });

  it('turns a stream that closed early into the cut-off failure', async () => {
    const { state } = await run({}, [`event: draft.token\ndata: ${JSON.stringify({ text: '{' })}\n\n`]);
    expect(state).toMatchObject({ phase: 'failed', failure: { code: 'stream_cut' } });
  });
});
