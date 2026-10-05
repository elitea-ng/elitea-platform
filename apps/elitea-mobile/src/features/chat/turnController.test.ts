import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import {
  prepareTurn,
  restorePreparedTurn,
  QUESTION_ID_RE,
  TurnInputError,
  MAX_TURN_BODY_BYTES,
} from '@/api/queries/sendMessage';
import { abortAll } from '@/lib/abortRegistry';
import { loadFixtureText } from '@/features/chat/__fixtures__/loadFixture';
import {
  FAILURE_TEXT,
  TurnController,
  clearLiveTurns,
  getLiveTurn,
  newQuestionId,
  setLiveTurn,
  type TurnDeps,
} from '@/features/chat/turnController';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: jest.fn(() => '3F1C2A9E-7B4D-4C8E-9A10-5D6E7F809A1B') }));

const ORIGIN = 'https://ai.example.com';
const CONV = '0b6c1f8e-1d2a-4f3b-8c4d-5e6f7a8b9c0d';
const QID = '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b';
const EVENTS = '/api/v2/executions/7/exec-1/events';

const request = { projectId: 7, conversationUuid: CONV, participantId: 12, userInput: 'Hi there', questionId: QID };

interface Sent {
  url: string;
  body: string;
  contentType: string | null;
}

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });
}

const started = (over: Record<string, unknown> = {}) =>
  json(200, {
    task_id: 't',
    execution_id: 'exec-1',
    command_id: 'c',
    response_message_id: 'resp-1',
    events_url: EVENTS,
    created: true,
    ...over,
  });

function harness(
  answers: ((init?: RequestInit) => Response | Promise<Response>)[],
  deps: Partial<TurnDeps> = {},
  clientOptions: { timeoutMs?: number } = {},
) {
  const sent: Sent[] = [];
  let call = 0;
  const fetch: BaseFetch = async (input, init) => {
    sent.push({
      url: input.url,
      body: await input.clone().text(),
      contentType: new Headers(init?.headers).get('Content-Type'),
    });
    const answer = answers[Math.min(call, answers.length - 1)];
    call += 1;
    return answer(init);
  };
  const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch, ...clientOptions });
  const stream = fakeStreamTransport();
  const sleeps: number[] = [];
  const onSettle = jest.fn(async () => {});
  const onRefreshNeeded = jest.fn();
  const controller = new TurnController(prepareTurn(request), {
    client,
    transport: stream.transport,
    appState: null,
    sleep: async (ms) => {
      sleeps.push(ms);
    },
    random: () => 0.5,
    onSettle,
    onRefreshNeeded,
    ...deps,
  });
  return { controller, sent, stream, sleeps, onSettle, onRefreshNeeded };
}

const flush = async () => {
  for (let i = 0; i < 10; i++) await new Promise<void>((r) => setTimeout(r, 0));
};

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  clearLiveTurns();
  setSession({ hydrated: true, serverOrigin: ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 'tok' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'error').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});

afterEach(() => jest.restoreAllMocks());

describe('question_id and body', () => {
  it('generates a lowercase canonical UUID', () => {
    const id = newQuestionId();
    expect(id).toBe(QID);
    expect(QUESTION_ID_RE.test(id)).toBe(true);
    expect(() => newQuestionId(() => 'not-a-uuid')).toThrow();
  });

  it('builds the minimal application-contract body without llm_settings or attachments', () => {
    expect(JSON.parse(prepareTurn(request).bodyJson)).toEqual({
      payload: { user_input: 'Hi there' },
      project_id: 7,
      participant_id: 12,
      conversation_uuid: CONV,
      question_id: QID,
    });
  });

  // DEF-3: a plain model turn (no agent) is the adhoc contract with the model in llm_settings.
  it('builds a model turn body with llm_settings and participant 0 allowed', () => {
    const model = { ...request, participantId: 0, model: { name: 'vllm/E2E-MOCK-MODEL', projectId: 90107 } };
    expect(JSON.parse(prepareTurn(model).bodyJson)).toEqual({
      payload: { user_input: 'Hi there' },
      project_id: 7,
      participant_id: 0,
      conversation_uuid: CONV,
      question_id: QID,
      llm_settings: { model_name: 'vllm/E2E-MOCK-MODEL', model_project_id: 90107, stream: true },
    });
    expect(JSON.parse(prepareTurn({ ...model, model: { name: 'm', projectId: null } }).bodyJson).llm_settings).toEqual({
      model_name: 'm',
      stream: true,
    });
    // An agent turn still needs a real participant; a model turn needs a model name.
    expect(() => prepareTurn({ ...request, participantId: 0 })).toThrow(TurnInputError);
    expect(() => prepareTurn({ ...model, model: { name: ' ', projectId: null } })).toThrow(TurnInputError);
    // The outbox restores the exact bytes and the model.
    const prepared = prepareTurn(model);
    expect(restorePreparedTurn(prepared.bodyJson)).toEqual({ request: { ...model }, bodyJson: prepared.bodyJson });
  });

  it('refuses an uppercase question_id, an empty message and an oversize body', () => {
    expect(() => prepareTurn({ ...request, questionId: QID.toUpperCase() })).toThrow(TurnInputError);
    expect(() => prepareTurn({ ...request, userInput: '   ' })).toThrow(TurnInputError);
    expect(() => prepareTurn({ ...request, userInput: 'x'.repeat(MAX_TURN_BODY_BYTES) })).toThrow(TurnInputError);
  });
});

describe('TurnController', () => {
  it('sends, streams the execution and settles on the terminal frame', async () => {
    const h = harness([() => started()]);
    const done = h.controller.start();
    await flush();
    expect(h.sent[0].url).toBe(
      `${ORIGIN}/api/v2/elitea_core/messages/prompt_lib/7/${CONV}?execution_contract=agent.execute.application.v1`,
    );
    expect(h.sent[0].contentType).toBe('application/json');
    expect(h.sent[0].body).toBe(h.controller.bodyJson);

    const connection = h.stream.connections[0];
    expect(connection.url).toBe(`${ORIGIN}${EVENTS}`);
    expect(connection.headers['Last-Event-ID']).toBeUndefined();
    connection.push(loadFixtureText('simple-answer'));
    await done;

    const live = h.controller.getSnapshot();
    expect(live.turn).toMatchObject({ phase: 'done', content: 'Hello, café ☕ world.' });
    expect(live.execution).toMatchObject({ executionId: 'exec-1', responseMessageId: 'resp-1', created: true });
    expect(connection.signal.aborted).toBe(true);
    expect(h.onSettle).toHaveBeenCalledTimes(1);
    expect(live.settled).toBe(true);
  });

  it('sends a model turn with the adhoc contract', async () => {
    const sent: Sent[] = [];
    const fetch: BaseFetch = async (input) => {
      sent.push({ url: input.url, body: await input.clone().text(), contentType: null });
      return started();
    };
    const client = createApiClient({ origin: ORIGIN, auth: { kind: 'session' }, fetch });
    const stream = fakeStreamTransport();
    const controller = new TurnController(
      prepareTurn({ ...request, participantId: 0, model: { name: 'vllm/E2E-MOCK-MODEL', projectId: null } }),
      { client, transport: stream.transport, appState: null, sleep: async () => {}, random: () => 0.5 },
    );
    void controller.start();
    await flush();
    expect(sent[0].url).toBe(
      `${ORIGIN}/api/v2/elitea_core/messages/prompt_lib/7/${CONV}?execution_contract=agent.execute.adhoc.v1`,
    );
    expect(JSON.parse(sent[0].body).llm_settings.model_name).toBe('vllm/E2E-MOCK-MODEL');
    controller.detach();
  });

  it('re-sends a byte-identical body after a network failure', async () => {
    const h = harness([
      () => {
        throw new TypeError('Network request failed');
      },
      () => {
        throw new TypeError('Network request failed');
      },
      () => started({ created: false }),
    ]);
    void h.controller.start();
    await flush();
    expect(h.sent).toHaveLength(3);
    expect(new Set(h.sent.map((s) => s.body)).size).toBe(1);
    expect(JSON.parse(h.sent[0].body).question_id).toBe(QID);
    expect(h.sleeps).toHaveLength(2);
    expect(h.controller.getSnapshot().execution?.created).toBe(false);
    h.controller.detach();
  });

  // DEF-5 (Agent Zefir E2E): with the device online and the workspace
  // hanging (edge accepts TCP, backend never answers), a send stayed
  // "Sending…" for minutes: every attempt waited for its deadline and was
  // retried in place. A send the workspace did not answer now goes to the
  // outbox (same question_id, same bytes) after the first attempt.
  it('hands a send the workspace never answered to the outbox after one attempt', async () => {
    const handOff = jest.fn(async () => true);
    // Never answers; like the platform fetch, it rejects only when aborted (the deadline).
    const hang = (init?: RequestInit) =>
      new Promise<Response>((_, reject) =>
        init?.signal?.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError'))),
      );
    const h = harness([hang], { handOff }, { timeoutMs: 20 });
    await h.controller.start();
    expect(h.sent).toHaveLength(1);
    expect(handOff).toHaveBeenCalledTimes(1);
    const [prepared] = handOff.mock.calls[0] as unknown as [{ bodyJson: string; request: { questionId: string } }];
    expect(prepared.bodyJson).toBe(h.controller.bodyJson);
    expect(prepared.request.questionId).toBe(QID);
    expect(h.controller.getSnapshot()).toMatchObject({ queued: true, failure: null, turn: { phase: 'sending' } });
    expect(h.controller.isActive).toBe(false);
  });

  it('hands a connection failure to the outbox too, but retries a 500 in place first', async () => {
    const handOff = jest.fn(async () => true);
    const refused = harness(
      [
        () => {
          throw new TypeError('Network request failed');
        },
      ],
      { handOff },
    );
    await refused.controller.start();
    expect(refused.sent).toHaveLength(1);
    expect(handOff).toHaveBeenCalledTimes(1);

    const busy = jest.fn(async () => true);
    const answered = harness([() => json(500, { error: 'boom' }), () => started()], { handOff: busy });
    void answered.controller.start();
    await flush();
    expect(busy).not.toHaveBeenCalled();
    expect(answered.sent).toHaveLength(2);
    answered.controller.detach();
  });

  // DEF-R3 (Agent Zefir E2E run 2): with the workspace stopped behind a
  // reachable proxy (traefik answers 502), a send retried in place ~6 times in
  // ~17 s, then failed for good; nothing was sent after the workspace came
  // back. A gateway answer now hands the send to the outbox, like no answer.
  it.each([502, 503, 504])('hands a send a proxy answered %i for to the outbox after one attempt', async (status) => {
    const handOff = jest.fn(async () => true);
    const h = harness([() => json(status, { error: 'bad gateway' })], { handOff });
    await h.controller.start();
    expect(h.sent).toHaveLength(1);
    expect(handOff).toHaveBeenCalledTimes(1);
    const [prepared] = handOff.mock.calls[0] as unknown as [{ bodyJson: string; request: { questionId: string } }];
    expect(prepared.bodyJson).toBe(h.controller.bodyJson);
    expect(prepared.request.questionId).toBe(QID);
    expect(h.controller.getSnapshot()).toMatchObject({ queued: true, failure: null });
    expect(h.controller.isActive).toBe(false);
  });

  it("passes the gateway's Retry-After to the outbox", async () => {
    const handOff = jest.fn(async () => true);
    const h = harness([() => json(503, { error: 'busy' }, { 'Retry-After': '30' })], { handOff });
    await h.controller.start();
    expect(handOff).toHaveBeenCalledWith(expect.objectContaining({ bodyJson: h.controller.bodyJson }), {
      retryAfterMs: 30_000,
    });
  });

  it('hands a send to the outbox when the in-place retries run out, instead of failing it', async () => {
    const handOff = jest.fn(async () => true);
    const h = harness([() => json(500, { error: 'boom' })], { handOff, maxSendAttempts: 3 });
    await h.controller.start();
    expect(h.sent).toHaveLength(3);
    expect(new Set(h.sent.map((s) => s.body))).toEqual(new Set([h.controller.bodyJson]));
    expect(handOff).toHaveBeenCalledTimes(1);
    expect(h.controller.getSnapshot()).toMatchObject({ queued: true, failure: null });
  });

  it('still fails a send the outbox cannot take once the in-place retries run out', async () => {
    const handOff = jest.fn(async () => false);
    const h = harness([() => json(502, { error: 'bad gateway' })], { handOff, maxSendAttempts: 2 });
    await h.controller.start();
    expect(h.sent).toHaveLength(2);
    expect(h.controller.getSnapshot()).toMatchObject({ queued: false, failure: 'send' });
    expect(h.controller.getSnapshot().turn.error).toMatchObject({ message: FAILURE_TEXT.unreachable, retryable: true });
  });

  it('keeps retrying in place when the outbox cannot take the send', async () => {
    const handOff = jest.fn(async () => false);
    const h = harness(
      [
        () => {
          throw new TypeError('Network request failed');
        },
        () => started(),
      ],
      { handOff },
    );
    void h.controller.start();
    await flush();
    expect(handOff).toHaveBeenCalledTimes(1);
    expect(h.sent).toHaveLength(2);
    expect(h.controller.getSnapshot().queued).toBe(false);
    h.controller.detach();
  });

  it('honours Retry-After on 503', async () => {
    const h = harness([() => json(503, { error: 'busy' }, { 'Retry-After': '1' }), () => started()]);
    void h.controller.start();
    await flush();
    expect(h.sleeps).toEqual([1000]);
    expect(h.sent).toHaveLength(2);
    h.controller.detach();
  });

  it('gives up sending after the attempt budget with a retryable failure', async () => {
    const h = harness([() => json(502, { error: 'bad gateway' })], { maxSendAttempts: 3 });
    await h.controller.start();
    expect(h.sent).toHaveLength(3);
    expect(h.controller.getSnapshot()).toMatchObject({
      failure: 'send',
      turn: { phase: 'failed', error: { message: FAILURE_TEXT.unreachable, retryable: true } },
    });
  });

  it('maps 422 unsupported_agent_execution to "needs the web app"', async () => {
    const h = harness([() => json(422, { error: 'unsupported_agent_execution', message: 'x' })]);
    await h.controller.start();
    expect(h.sent).toHaveLength(1);
    expect(h.controller.getSnapshot().turn.error).toEqual({ message: FAILURE_TEXT.unsupported, retryable: false });
  });

  it('treats the idempotency 409 as a non-retryable bug', async () => {
    const h = harness([() => json(409, { error: 'Agent execution request conflicts with an existing turn' })]);
    await h.controller.start();
    expect(h.sent).toHaveLength(1);
    expect(h.controller.getSnapshot().turn.error).toEqual({ message: FAILURE_TEXT.conflict, retryable: false });
  });

  it('retries a retryable 409 and shows the server message for a final one', async () => {
    const h = harness([
      () => json(409, { error: 'agent_regeneration_pending', message: 'wait', retryable: true }, { 'Retry-After': '1' }),
      () => json(409, { error: 'agent_hitl_already_resolved', message: 'Refresh first.', retryable: false }),
    ]);
    await h.controller.start();
    expect(h.sent).toHaveLength(2);
    expect(h.sleeps).toEqual([1000]);
    expect(h.controller.getSnapshot().turn.error).toEqual({ message: 'Refresh first.', retryable: false });
  });

  it.each([
    ['another origin', 'https://evil.example/api/v2/executions/7/x/events'],
    ['a protocol-relative URL', '//evil.example/api/v2/executions/7/x/events'],
    ['a dot segment', '/api/v2/../admin'],
  ])('refuses an events_url on %s and never opens a stream', async (_label, eventsUrl) => {
    const h = harness([() => started({ events_url: eventsUrl })]);
    await h.controller.start();
    expect(h.stream.transport).not.toHaveBeenCalled();
    expect(h.controller.getSnapshot().turn.error).toEqual({ message: FAILURE_TEXT.badResponse, retryable: false });
  });

  it('reattaches with the same body and replays the stream from the start', async () => {
    const h = harness([() => started(), () => started({ created: false })]);
    void h.controller.start();
    await flush();
    h.stream.connections[0].push('id: 1\nevent: execution.node_event\ndata: {"type":"agent_llm_chunk","content":"Hel"}\n\n');
    await flush();
    expect(h.controller.getSnapshot().turn.content).toBe('Hel');

    h.controller.detach();
    expect(h.stream.connections[0].signal.aborted).toBe(true);
    const done = h.controller.reattach();
    await flush();
    expect(h.sent).toHaveLength(2);
    expect(h.sent[1].body).toBe(h.sent[0].body);
    const replay = h.stream.connections[1];
    expect(replay.headers['Last-Event-ID']).toBeUndefined();
    expect(h.controller.getSnapshot().turn.content).toBe('');
    replay.push(loadFixtureText('simple-answer'));
    await done;
    expect(h.controller.getSnapshot().turn).toMatchObject({ phase: 'done', content: 'Hello, café ☕ world.' });
  });

  it('asks for a REST refill on replay_reset and reports execution.failed', async () => {
    const h = harness([() => started()]);
    const done = h.controller.start();
    await flush();
    h.stream.connections[0].push(loadFixtureText('replay-reset'));
    await done;
    expect(h.onRefreshNeeded).toHaveBeenCalledTimes(1);

    const f = harness([() => started()]);
    const failed = f.controller.start();
    await flush();
    f.stream.connections[0].push(loadFixtureText('execution-failed'));
    await failed;
    expect(f.controller.getSnapshot()).toMatchObject({
      settled: false,
      turn: { phase: 'failed', error: { retryable: true } },
    });
  });

  it('fails the turn when the stream is refused', async () => {
    const h = harness([() => started()]);
    h.stream.transport.mockImplementationOnce(async () => ({
      status: 404,
      headers: { get: () => null },
      body: null,
    }));
    await h.controller.start();
    expect(h.controller.getSnapshot()).toMatchObject({
      failure: 'stream',
      turn: { phase: 'failed', error: { message: FAILURE_TEXT.stream, retryable: true } },
    });
  });

  it('keeps live turns per conversation and detaches a replaced one', () => {
    const a = harness([() => started()]).controller;
    const b = harness([() => started()]).controller;
    const detach = jest.spyOn(a, 'detach');
    setLiveTurn('7:x', a);
    setLiveTurn('7:x', b);
    expect(detach).toHaveBeenCalled();
    expect(getLiveTurn('7:x')).toBe(b);
    clearLiveTurns();
    expect(getLiveTurn('7:x')).toBeUndefined();
  });
});
