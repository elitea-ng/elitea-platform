import { ApiError } from '@/api/errors';
import { prepareTurn, restorePreparedTurn, startTurn, TurnInputError } from '@/api/queries/sendMessage';
import { migrate } from '@/db/migrations';
import { dropProject } from '@/db/repo';
import { Db } from '@/db/sql';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { OutboxDrainer, enqueue, listOutbox, outboxBackoffMs, outboxStatus } from '@/sync/outbox';
import { FAKE_ORIGIN, fakeServer, type Answer } from '@/test/fakeServer';
import { openNodeSqlite } from '@/test/nodeSqlite';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

const UUID = '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c';
const Q1 = '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f';
const Q2 = '1c8f6e4b-2d3a-4f7b-9c0e-1b2c3d4e5f60';
const SEND = `/elitea_core/messages/prompt_lib/7/${UUID}`;
const started = (execution: string, created = true): Answer => ({
  status: 200,
  body: {
    task_id: execution,
    execution_id: execution,
    command_id: 'cmd',
    response_message_id: 'resp',
    events_url: `/api/v2/executions/7/${execution}/events`,
    created,
  },
});

const turn = (questionId: string, text = 'hello') =>
  prepareTurn({ projectId: 7, conversationUuid: UUID, participantId: 2, questionId, userInput: text });

async function setup(now = { t: 1_000_000 }) {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  const server = fakeServer();
  const onSent = jest.fn();
  const wakes: number[] = [];
  const drainer = new OutboxDrainer({
    getDb: () => db,
    start: (prepared) => startTurn(prepared, { client: server.client }),
    now: () => now.t,
    onSent,
    schedule: (delay) => {
      wakes.push(delay);
      return () => {};
    },
  });
  return { db, server, drainer, onSent, now, wakes };
}

const sends = (server: ReturnType<typeof fakeServer>) => server.requests.filter((r) => r.method === 'POST');

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('restorePreparedTurn', () => {
  it('returns the stored bytes untouched', () => {
    const bodyJson = turn(Q1).bodyJson.replace('{', '{ '); // different spacing than JSON.stringify would produce
    const restored = restorePreparedTurn(bodyJson);
    expect(restored.bodyJson).toBe(bodyJson);
    expect(restored.request.questionId).toBe(Q1);
  });

  it('restores a row queued by an earlier build (the overlay ApplicationTurnRequest body) byte for byte', () => {
    // Exactly what the pre-1.1 build stored: participant_id always present.
    const legacy =
      '{"payload":{"user_input":"hello"},"project_id":7,"participant_id":3,' +
      `"conversation_uuid":"${UUID}","question_id":"${Q1}"}`;
    const restored = restorePreparedTurn(legacy);
    expect(restored.bodyJson).toBe(legacy);
    expect(restored.request).toMatchObject({ projectId: 7, participantId: 3, userInput: 'hello', model: null });
  });

  it('restores a ChatSendRequest body without participant_id as a model turn (participant 0)', () => {
    const body =
      '{"payload":{"user_input":"hi"},"project_id":7,' +
      `"conversation_uuid":"${UUID}","question_id":"${Q1}",` +
      '"llm_settings":{"model_name":"gpt-5","stream":true}}';
    const restored = restorePreparedTurn(body);
    expect(restored.bodyJson).toBe(body);
    expect(restored.request).toMatchObject({ participantId: 0, model: { name: 'gpt-5', projectId: null } });
  });

  it('refuses a stored body that a fresh send would refuse (upper-case question_id)', () => {
    const upper = turn(Q1).bodyJson.replace(Q1, Q1.toUpperCase());
    expect(() => restorePreparedTurn(upper)).toThrow(TurnInputError);
    expect(() => restorePreparedTurn('{')).toThrow(TurnInputError);
  });
});

describe('enqueue', () => {
  it('stores the exact body once per question_id, queued', async () => {
    const { db } = await setup();
    await enqueue(db, turn(Q1), '42', 5);
    await enqueue(db, turn(Q1, 'other text'), '42', 6);
    const items = await listOutbox(db, 7, UUID);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ questionId: Q1, state: 'queued', userText: 'hello', bodyJson: turn(Q1).bodyJson });
  });

  it('is dropped with its project', async () => {
    const { db } = await setup();
    await enqueue(db, turn(Q1), '42');
    await db.transaction((tx) => dropProject(tx, 7));
    expect(await listOutbox(db, 7, UUID)).toEqual([]);
  });
});

describe('OutboxDrainer', () => {
  it('sends the stored bytes and removes the item once admitted', async () => {
    const { db, server, drainer, onSent } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, started('exec-1'));
    expect(await drainer.drain()).toEqual({ sent: 1, failed: 0, waiting: 0 });
    const [request] = sends(server);
    expect(request.body).toBe(turn(Q1).bodyJson);
    expect(request.url.searchParams.get('execution_contract')).toBe('agent.execute.application.v1');
    expect(await listOutbox(db, 7, UUID)).toEqual([]);
    expect(onSent).toHaveBeenCalledWith(expect.objectContaining({ questionId: Q1 }), expect.objectContaining({ executionId: 'exec-1' }));
  });

  it('a lost answer is retried with the same question_id and body; the replay (created:false) counts as sent', async () => {
    const { db, server, drainer, now } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, 'network_error', { status: 503, body: { error: 'busy' }, headers: { 'Retry-After': '7' } }, started('exec-1', false));
    expect(await drainer.drain()).toMatchObject({ sent: 0, waiting: 1 });
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ state: 'queued', attempts: 1, nextAttemptMs: now.t + 2000 });
    now.t += 2000;
    await drainer.drain();
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ attempts: 2, nextAttemptMs: now.t + 7000 }); // Retry-After wins
    now.t += 7000;
    expect(await drainer.drain()).toMatchObject({ sent: 1 });
    const bodies = sends(server).map((r) => r.body);
    expect(bodies).toEqual([turn(Q1).bodyJson, turn(Q1).bodyJson, turn(Q1).bodyJson]);
    expect(new Set(bodies.map((b) => JSON.parse(b!).question_id))).toEqual(new Set([Q1]));
  });

  it('409 idempotency conflict: never resent, shown as failed', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, { status: 409, body: { error: 'Agent execution request conflicts with an existing turn' } });
    expect(await drainer.drain()).toMatchObject({ failed: 1 });
    const [item] = await listOutbox(db, 7, UUID);
    expect(item).toMatchObject({ state: 'failed', lastError: 'idempotency_conflict' });
    await drainer.drain();
    expect(sends(server)).toHaveLength(1);
    expect(outboxStatus(item)).toEqual({ text: expect.stringContaining('different message'), error: true });
  });

  it('a retryable 409 waits; a final one fails', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, { status: 409, body: { error: 'agent_regeneration_pending', retryable: true } });
    await drainer.drain();
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ state: 'queued', attempts: 1 });
    server.on('POST', SEND, { status: 409, body: { error: 'agent_hitl_already_resolved', retryable: false } });
    await db.run('UPDATE outbox SET next_attempt_ms = 0');
    await drainer.drain();
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ state: 'failed', lastError: 'conflict' });
  });

  it('keeps the written order: a later message waits while an earlier one backs off', async () => {
    const { db, server, drainer, now, wakes } = await setup();
    await enqueue(db, turn(Q1, 'first'), '42', 1);
    await enqueue(db, turn(Q2, 'second'), '42', 2);
    server.on('POST', SEND, 'network_error', started('exec-1'), started('exec-2'));
    expect(await drainer.drain()).toEqual({ sent: 0, failed: 0, waiting: 2 });
    expect(sends(server)).toHaveLength(1);
    expect(wakes.at(-1)).toBe(2000);
    now.t += 2000;
    expect(await drainer.drain()).toMatchObject({ sent: 2 });
    expect(sends(server).map((r) => JSON.parse(r.body!).payload.user_input)).toEqual(['first', 'first', 'second']);
  });

  it('holds everything while signed out or upgrade-required (not the item’s fault)', async () => {
    const { db } = await setup();
    await enqueue(db, turn(Q1), '42');
    const held = new OutboxDrainer({
      getDb: () => db,
      start: async () => {
        throw new ApiError('session_invalid', 'signed out', { reason: 'no_credential' });
      },
    });
    expect(await held.drain()).toMatchObject({ waiting: 1 });
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ state: 'queued', attempts: 0 });
  });

  // F1: a 426 (or any hold) must not re-arm a 0 ms wake, or the app resends in a tight loop.
  it('schedules no wake after a hold (no resend loop while upgrade-required)', async () => {
    const { db, server, drainer, wakes } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, { status: 426, body: { error: 'upgrade_required', min_client_version: '9.0.0' } });
    expect(await drainer.drain()).toMatchObject({ waiting: 1 });
    expect(sends(server)).toHaveLength(1);
    expect((await listOutbox(db, 7, UUID))[0]).toMatchObject({ state: 'queued', attempts: 0 });
    expect(wakes).toEqual([]);
  });

  // F2: the drain works from a snapshot; an item deleted mid-drain must not be sent.
  it('does not send an item the user deleted while an earlier one was in flight', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1, 'first'), '42', 1);
    await enqueue(db, turn(Q2, 'second'), '42', 2);
    const sent: string[] = [];
    const racing = new OutboxDrainer({
      getDb: () => db,
      start: async (prepared) => {
        sent.push(prepared.request.questionId);
        if (prepared.request.questionId === Q1) await drainer.remove(Q2); // the user taps Delete on item 2
        return startTurn(prepared, { client: server.client });
      },
    });
    server.on('POST', SEND, started('exec-1'));
    expect(await racing.drain()).toMatchObject({ sent: 1 });
    expect(sent).toEqual([Q1]);
    expect(await listOutbox(db, 7, UUID)).toEqual([]);
  });

  it('an item left "sending" by a crash is sent again (same bytes, so at most one turn)', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1), '42');
    await db.run("UPDATE outbox SET state = 'sending'");
    server.on('POST', SEND, started('exec-1', false));
    expect(await drainer.drain()).toMatchObject({ sent: 1 });
  });

  it('a refused send (400/403/422) fails; an unreadable stored body fails without a request', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1), '42', 1);
    await enqueue(db, turn(Q2), '42', 2);
    await db.run('UPDATE outbox SET body_json = ? WHERE question_id = ?', ['{"broken":true}', Q2]);
    server.on('POST', SEND, { status: 422, body: { error: 'unsupported_agent_execution' } });
    expect(await drainer.drain()).toMatchObject({ failed: 2 });
    const items = await listOutbox(db, 7, UUID);
    expect(items.map((i) => i.lastError)).toEqual(['unsupported_agent', 'invalid']);
    expect(sends(server)).toHaveLength(1);
  });

  it('retryNow makes a queued item due at once; remove deletes it', async () => {
    const { db, server, drainer } = await setup();
    await enqueue(db, turn(Q1), '42');
    server.on('POST', SEND, 'network_error');
    await drainer.drain();
    server.on('POST', SEND, started('exec-1'));
    await drainer.retryNow(Q1);
    await drainer.drain();
    expect(await listOutbox(db, 7, UUID)).toEqual([]);
    await enqueue(db, turn(Q2), '42');
    await drainer.remove(Q2);
    expect(await listOutbox(db, 7, UUID)).toEqual([]);
  });

  it('does nothing while the store is closed', async () => {
    const drainer = new OutboxDrainer({ getDb: () => null });
    expect(await drainer.drain()).toEqual({ sent: 0, failed: 0, waiting: 0 });
  });
});

describe('outbox status and backoff', () => {
  it('describes each state', () => {
    expect(outboxStatus({ state: 'queued', lastError: null, attempts: 0 }).text).toMatch(/back online/);
    expect(outboxStatus({ state: 'queued', lastError: null, attempts: 2 }).text).toMatch(/Retrying/);
    expect(outboxStatus({ state: 'sending', lastError: null, attempts: 0 })).toEqual({ text: 'Sending…', error: false });
    expect(outboxStatus({ state: 'failed', lastError: 'rejected', attempts: 1 }).error).toBe(true);
  });

  it('backs off 2 s, 4 s, … up to 5 min; Retry-After wins within bounds', () => {
    expect([1, 2, 3, 10].map((n) => outboxBackoffMs(n))).toEqual([2000, 4000, 8000, 300_000]);
    expect(outboxBackoffMs(1, 30_000)).toBe(30_000);
    expect(outboxBackoffMs(1, 0)).toBe(1000);
  });
});
