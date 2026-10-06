import { cancelNotActive } from '@/api/__fixtures__/chat13';
import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { prepareTurn } from '@/api/queries/sendMessage';
import { StopError, stopErrorText, stopTurn } from '@/api/queries/stopTurn';
import { parseFrame } from '@/features/chat/frames';
import { loadFixtureBytes } from '@/features/chat/__fixtures__/loadFixture';
import { TurnController } from '@/features/chat/turnController';
import { INITIAL_TURN, reduceTurn } from '@/features/chat/turnReducer';
import { EventStreamParser } from '@/api/sse/parser';
import { abortAll } from '@/lib/abortRegistry';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: jest.fn(() => '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b') }));

// The answer of the captured cancelled.sse (zefir-wave, main 22403f640).
const ANSWER = 'aa5ea578-4aae-55a8-a361-08dae0136a9e';
const P = 90501;
const CONV = '9bb96505-d0fb-4473-be67-4884de5047d3';
const ROUTE = `/elitea_core/task/prompt_lib/${P}/${ANSWER}`;
const flush = async () => {
  for (let i = 0; i < 10; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('stopTurn (cancelChatExecution, captured behaviour)', () => {
  it('204 is stopped, also for the same caller again; 409 is "not running"', async () => {
    const server = fakeServer();
    server.on('DELETE', ROUTE, { status: 204 }, { status: 204 }, { status: 409, body: cancelNotActive });
    await expect(stopTurn(P, ANSWER, server.client)).resolves.toBe('stopped');
    await expect(stopTurn(P, ANSWER, server.client)).resolves.toBe('stopped');
    await expect(stopTurn(P, ANSWER, server.client)).resolves.toBe('not_running');
    expect(server.requests.map((r) => `${r.method} ${r.url.pathname}`)).toEqual(Array(3).fill(`DELETE /api/v2${ROUTE}`));
  });

  it('maps 403, an older server and gateway failures', async () => {
    const server = fakeServer();
    server.on('DELETE', ROUTE, { status: 403, body: {} }, { status: 405, body: {} }, { status: 504, body: {} }, 'network_error');
    for (const reason of ['not_allowed', 'not_available', 'retry', 'retry']) {
      await expect(stopTurn(P, ANSWER, server.client)).rejects.toMatchObject({ reason });
    }
    expect(stopErrorText(new StopError('retry'))).toMatch(/Couldn't reach/);
  });

  it('refuses an id that is not a uuid before any request', async () => {
    const server = fakeServer();
    await expect(stopTurn(P, 'resp-1', server.client)).rejects.toMatchObject({ reason: 'invalid' });
    expect(server.requests).toHaveLength(0);
  });
});

describe('a stopped run settles, keeping its partial text', () => {
  it('the captured CANCELLED stream folds to a finished, stopped answer (not a failure)', () => {
    let turn = INITIAL_TURN;
    const parser = new EventStreamParser();
    for (const event of [...parser.push(loadFixtureBytes('cancelled')), ...parser.end()]) turn = reduceTurn(turn, parseFrame(event));
    expect(turn).toMatchObject({ phase: 'done', stopped: true, error: null });
    // The worker had written two chunks when the stop landed.
    expect(turn.content).toBe('MOCK: [[mock:slow]] stop ');
  });

  function controller(fetch: BaseFetch) {
    const client = createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch });
    const stream = fakeStreamTransport();
    const onSettle = jest.fn(async () => {});
    const c = new TurnController(
      prepareTurn({ projectId: P, conversationUuid: CONV, participantId: 3, userInput: 'stop me', questionId: '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }),
      { client, transport: stream.transport, appState: null, sleep: async () => {}, random: () => 0.5, onSettle },
    );
    return { c, stream, onSettle };
  }

  it('Stop: DELETE for the admitted answer, the stream is closed, the transcript re-read, the turn settled as stopped', async () => {
    const calls: string[] = [];
    const fetch: BaseFetch = async (input) => {
      calls.push(`${input.method} ${new URL(input.url).pathname}`);
      if (input.method === 'DELETE') return new Response(null, { status: 204 });
      return new Response(
        JSON.stringify({ execution_id: 'e1', events_url: `/api/v2/executions/${P}/e1/events`, response_message_id: ANSWER, created: true }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    };
    const { c, stream, onSettle } = controller(fetch);
    void c.start();
    await flush();
    // Not stoppable before the first frame? It is: the answer id is known once admitted.
    stream.connections[0].push('id: 1\nevent: execution.node_event\ndata: {"type":"agent_llm_chunk","content":"MOCK: partial "}\n\n');
    await flush();
    expect(c.getSnapshot().turn.phase).toBe('streaming');
    await expect(c.stop()).resolves.toBe('stopped');
    expect(calls).toContain(`DELETE /api/v2${ROUTE}`);
    expect(stream.connections[0].signal.aborted).toBe(true);
    expect(onSettle).toHaveBeenCalledTimes(1);
    expect(c.getSnapshot()).toMatchObject({ settled: true, turn: { phase: 'done', stopped: true, content: 'MOCK: partial ' } });
  });

  it('409 (the answer had ended): nothing changes, its own stream settles it', async () => {
    const fetch: BaseFetch = async (input) =>
      input.method === 'DELETE'
        ? new Response(JSON.stringify(cancelNotActive), { status: 409, headers: { 'Content-Type': 'application/json' } })
        : new Response(JSON.stringify({ execution_id: 'e1', events_url: `/api/v2/executions/${P}/e1/events`, response_message_id: ANSWER }), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          });
    const { c, stream } = controller(fetch);
    void c.start();
    await flush();
    await expect(c.stop()).resolves.toBe('not_running');
    expect(stream.connections[0].signal.aborted).toBe(false);
    expect(c.getSnapshot().settled).toBe(false);
    c.detach();
  });

  it('is not possible while the send is still in flight', async () => {
    // Never answers; gives up only when the request is aborted.
    const fetch: BaseFetch = (input) =>
      new Promise<Response>((_, reject) => input.signal.addEventListener('abort', () => reject(new Error('aborted'))));
    const { c } = controller(fetch);
    void c.start();
    await flush();
    await expect(c.stop()).resolves.toBe('not_ready');
    c.detach();
  });
});
