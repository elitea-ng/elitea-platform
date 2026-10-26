import type { components } from '@/api/generated/elitea.gen';
import { createApiClient } from '@/api/client';
import type { BaseFetch } from '@/api/middleware';
import { prepareTurn } from '@/api/queries/sendMessage';
import { editSteering, handOffSteering, listSteering, queueSteering, removeSteering } from '@/api/queries/steering';
import { parseFrame } from '@/features/chat/frames';
import { steeringRows } from '@/features/chat/SteeringRows';
import { TurnController } from '@/features/chat/turnController';
import { steerOutcomeText } from '@/features/chat/useTurn';
import { steeringWaitText } from '@/features/chat/useSteeringWaitText';
import { abortAll } from '@/lib/abortRegistry';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN } from '@/test/fakeServer';
import { fakeStreamTransport } from '@/test/fakeStreamTransport';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());
jest.mock('expo-crypto', () => ({ randomUUID: jest.fn(() => '3F1C2A9E-7B4D-4C8E-9A10-5D6E7F809A1B') }));

type SteeringMessage = components['schemas']['SteeringMessage'];
// `retry_after_ms` is typed here until the generated `SteeringError` has it.
type SteeringError = components['schemas']['SteeringError'] & { retry_after_ms?: number };

const P = 7;
const CONV = '9bb96505-d0fb-4473-be67-4884de5047d3';
// The `SteeringMessage` example of services/elitea-main/api/openapi/v2.yaml.
const EXAMPLE: SteeringMessage = {
  id: '7f0c3c9a-2d4e-4b8f-9a51-0d6c2f1e8b11',
  response_message_id: '3b9d2a40-7c55-4e1a-8f0b-6a2c9e4d1f20',
  author_id: 1,
  content: 'Skip the tests for now and only fix the build.',
  state: 'pending',
  revision: 1,
  created_at: '2026-10-26T10:15:02Z',
  updated_at: '2026-10-26T10:15:02Z',
  injected_at: null,
  message_id: null,
};
const ANSWER = EXAMPLE.response_message_id;
// The handOffChatExecutionSteering example of v2.yaml: the same message, now `handed_off`.
const HANDED: SteeringMessage = { ...EXAMPLE, state: 'handed_off', updated_at: '2026-10-26T10:15:40Z' };
// The 429 `rate_limited` example of v2.yaml.
const RATE_LIMITED: SteeringError = { error: 'rate_limited', message: 'Too many steering messages. Retry after the interval in Retry-After.', retry_after_ms: 2000 };
const ROUTE = `/api/v2/elitea_core/task/prompt_lib/${P}/${ANSWER}/steering`;
const HANDOFF = `${ROUTE}/handoff`;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const flush = async () => {
  for (let i = 0; i < 10; i++) await new Promise<void>((r) => setTimeout(r, 0));
};
const sse = (event: string, data: unknown, id: number) => `id: ${id}\nevent: ${event}\ndata: ${JSON.stringify(data)}\n\n`;

interface Seen {
  method: string;
  path: string;
  key: string | null;
  body: string | null;
}

/** A workspace that admits the send and answers the steering routes from `steering`. */
function workspace(steering: (method: string, path: string) => Response) {
  const seen: Seen[] = [];
  const fetch: BaseFetch = async (input) => {
    const path = new URL(input.url).pathname;
    const body = input.method === 'GET' ? null : await input.clone().text();
    seen.push({ method: input.method, path, key: input.headers.get('Idempotency-Key'), body });
    if (path.startsWith(ROUTE)) return steering(input.method, path);
    return json({ execution_id: 'e1', events_url: `/api/v2/executions/${P}/e1/events`, response_message_id: ANSWER, created: true });
  };
  return { client: createApiClient({ origin: FAKE_ORIGIN, auth: { kind: 'session' }, fetch }), seen };
}

function controller(steering: (method: string, path: string) => Response, sleep: (ms: number) => Promise<void> = async () => {}) {
  const { client, seen } = workspace(steering);
  const stream = fakeStreamTransport();
  const onLeftover = jest.fn();
  const c = new TurnController(
    prepareTurn({ projectId: P, conversationUuid: CONV, participantId: 3, userInput: 'build it', questionId: '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b' }),
    { client, transport: stream.transport, appState: null, sleep, random: () => 0.5, onSettle: async () => {}, onLeftover },
  );
  return { c, stream, seen, onLeftover };
}

beforeEach(() => {
  __resetSessionForTests();
  abortAll();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: P });
  setCredential({ kind: 'pat', token: 't' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
  jest.spyOn(console, 'debug').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('steering calls (client contract 1.26)', () => {
  it('POST sends the Idempotency-Key and the content; refusals answer their error code', async () => {
    const answers = [json(EXAMPLE, 202), json({ error: 'turn_not_running' }, 409), json({ error: 'rate_limited' }, 429)];
    const { client, seen } = workspace(() => answers.shift()!);
    await expect(queueSteering(P, ANSWER, EXAMPLE.content, 'key-1', client)).resolves.toMatchObject({ ok: true, item: { id: EXAMPLE.id, state: 'pending' } });
    await expect(queueSteering(P, ANSWER, 'x', 'key-2', client)).resolves.toEqual({ ok: false, code: 'turn_not_running', status: 409 });
    await expect(queueSteering(P, ANSWER, 'y', 'key-3', client)).resolves.toEqual({ ok: false, code: 'rate_limited', status: 429, retryAfterMs: 2000 });
    expect(seen[0]).toMatchObject({ method: 'POST', path: ROUTE, key: 'key-1', body: JSON.stringify({ content: EXAMPLE.content }) });
  });

  it('429 rate_limited: retryAfterMs is the body retry_after_ms, else Retry-After, else 2 s', async () => {
    const limited = (body: SteeringError, headers: Record<string, string> = {}) =>
      new Response(JSON.stringify(body), { status: 429, headers: { 'Content-Type': 'application/json', ...headers } });
    const answers = [limited({ ...RATE_LIMITED, retry_after_ms: 3500 }, { 'Retry-After': '9' }), limited({ error: 'rate_limited' }, { 'Retry-After': '4' }), limited({ error: 'rate_limited' })];
    const { client } = workspace(() => answers.shift()!);
    await expect(queueSteering(P, ANSWER, 'a', 'k', client)).resolves.toMatchObject({ code: 'rate_limited', retryAfterMs: 3500 });
    await expect(queueSteering(P, ANSWER, 'a', 'k', client)).resolves.toMatchObject({ code: 'rate_limited', retryAfterMs: 4000 });
    await expect(queueSteering(P, ANSWER, 'a', 'k', client)).resolves.toMatchObject({ code: 'rate_limited', retryAfterMs: 2000 });
  });

  it('GET reads the list; PATCH and DELETE address one message', async () => {
    const { client, seen } = workspace((method) =>
      method === 'GET'
        ? json({ items: [{ ...EXAMPLE, state: 'injected', message_id: '9a1e6b02-55c3-4d7e-b0f4-2e8c7d6a3b90' }] })
        : method === 'PATCH'
          ? json({ ...EXAMPLE, content: 'Fix the build first, then run the unit tests.', revision: 2 })
          : new Response(null, { status: 204 }),
    );
    await expect(listSteering(P, ANSWER, client)).resolves.toMatchObject([{ state: 'injected', messageId: '9a1e6b02-55c3-4d7e-b0f4-2e8c7d6a3b90' }]);
    await expect(editSteering(P, ANSWER, EXAMPLE.id, 'new', client)).resolves.toMatchObject({ ok: true, item: { revision: 2 } });
    await expect(removeSteering(P, ANSWER, EXAMPLE.id, client)).resolves.toEqual({ ok: true, item: undefined });
    expect(seen.map((s) => `${s.method} ${s.path}`)).toEqual([`GET ${ROUTE}`, `PATCH ${ROUTE}/${EXAMPLE.id}`, `DELETE ${ROUTE}/${EXAMPLE.id}`]);
  });

  it('the hand-off POST sends the Idempotency-Key and no body, and answers the handed_off messages', async () => {
    const { client, seen } = workspace(() => json({ items: [HANDED] }));
    await expect(handOffSteering(P, ANSWER, 'key-h', client)).resolves.toMatchObject([{ id: EXAMPLE.id, state: 'handed_off', content: EXAMPLE.content }]);
    expect(seen[0]).toMatchObject({ method: 'POST', path: HANDOFF, key: 'key-h' });
    expect(seen[0]?.body ?? '').toBe('');
  });

  it('refuses an answer id that is not a uuid before any request', async () => {
    const { client, seen } = workspace(() => json({}));
    await expect(queueSteering(P, 'resp-1', 'x', 'k', client)).resolves.toMatchObject({ ok: false });
    expect(seen).toHaveLength(0);
  });
});

describe('steer frames', () => {
  it('folds steer.queued / steer.injected / steer.dropped; a dropped message is removed', () => {
    expect(parseFrame({ event: 'steer.queued', data: JSON.stringify(EXAMPLE), id: '1' })).toMatchObject({ kind: 'steer', item: { state: 'pending' } });
    expect(parseFrame({ event: 'steer.dropped', data: JSON.stringify({ ...EXAMPLE, reason: 'removed' }), id: '2' })).toMatchObject({
      kind: 'steer',
      item: { state: 'removed' },
    });
    expect(parseFrame({ event: 'steer.dropped', data: JSON.stringify({ ...HANDED, reason: 'handed_off' }), id: '4' })).toMatchObject({
      kind: 'steer',
      item: { state: 'removed' },
    });
    expect(parseFrame({ event: 'steer.injected', data: '{}', id: '3' })).toEqual({ kind: 'invalid', event: 'steer.injected' });
  });

  it('draws pending and delivered messages, and hides a delivered one the transcript holds', () => {
    const base = { responseMessageId: ANSWER, authorId: 1, revision: 1, createdAt: '' };
    const rows = steeringRows(
      [
        { ...base, id: 'p', content: 'a', state: 'pending', messageId: null },
        { ...base, id: 'i', content: 'b', state: 'injected', messageId: 'm-i' },
        { ...base, id: 't', content: 'c', state: 'turn_ended', messageId: null },
        { ...base, id: 'h', content: 'd', state: 'handed_off', messageId: null },
      ],
      new Set(['m-i']),
    );
    expect(rows.map((r) => r.id)).toEqual(['p']);
  });
});

describe('TurnController steering', () => {
  async function streaming(steering: (method: string, path: string) => Response, sleep?: (ms: number) => Promise<void>) {
    const t = controller(steering, sleep);
    void t.c.start();
    await flush();
    expect(t.c.canSteer).toBe(true);
    return t;
  }

  it('queues against the running answer, folds the frames, hands off the unread own messages and sends ONLY what the hand-off returned', async () => {
    const second = { ...EXAMPLE, id: '0d4b5f8e-1c2a-4e3b-9f00-112233445566', content: 'And keep the logs.' };
    const posted = [json(EXAMPLE, 202), json(second, 202)];
    const { c, stream, seen, onLeftover } = await streaming((method, path) => {
      if (path === HANDOFF) return json({ items: [{ ...second, state: 'handed_off' }, { ...HANDED, id: '5e6f7a8b-9c0d-4e1f-a2b3-c4d5e6f7a8b9', content: 'From the other phone.' }] });
      if (method === 'POST') return posted.shift()!;
      if (method === 'GET') return json({ items: [{ ...EXAMPLE, state: 'injected', message_id: '9a1e6b02-55c3-4d7e-b0f4-2e8c7d6a3b90' }, { ...second, state: 'turn_ended' }] });
      return new Response(null, { status: 204 });
    });
    await expect(c.steer(EXAMPLE.content)).resolves.toBe('queued');
    await expect(c.steer(second.content)).resolves.toBe('queued');
    expect(seen.filter((s) => s.method === 'POST' && s.path === ROUTE).map((s) => s.key)).toEqual([
      '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b',
      '3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b',
    ]);
    expect(c.getSnapshot().ownSteering).toEqual([EXAMPLE.id, second.id]);
    stream.connections[0].push(sse('steer.injected', { ...EXAMPLE, state: 'injected', message_id: '9a1e6b02-55c3-4d7e-b0f4-2e8c7d6a3b90' }, 1));
    await flush();
    expect(c.getSnapshot().turn.steering.map((s) => s.state)).toEqual(['injected', 'pending']);
    stream.connections[0].push(sse('execution.node_event', { type: 'agent_response', content: 'done', response_metadata: { finish_reason: 'stop' } }, 2));
    await flush();
    expect(seen.filter((s) => s.path === HANDOFF).map((s) => s.key)).toEqual(['3f1c2a9e-7b4d-4c8e-9a10-5d6e7f809a1b']);
    expect(onLeftover).toHaveBeenCalledTimes(1);
    expect(onLeftover).toHaveBeenCalledWith('And keep the logs.\n\nFrom the other phone.');
    expect(seen.filter((s) => s.method === 'DELETE')).toEqual([]);
    expect(c.getSnapshot().ownSteering).toEqual([]);
  });

  it('sends nothing when the hand-off answers an empty list (the server took the messages first)', async () => {
    const { c, stream, seen, onLeftover } = await streaming((method, path) => {
      if (path === HANDOFF) return json({ items: [] });
      if (method === 'POST') return json(EXAMPLE, 202);
      return json({ items: [{ ...EXAMPLE, state: 'turn_ended' }] });
    });
    await expect(c.steer(EXAMPLE.content)).resolves.toBe('queued');
    stream.connections[0].push(sse('execution.node_event', { type: 'pipeline_finish', content: 'done' }, 1));
    await flush();
    expect(seen.filter((s) => s.path === HANDOFF)).toHaveLength(1);
    expect(onLeftover).not.toHaveBeenCalled();
    expect(c.getSnapshot().ownSteering).toEqual([]);
  });

  it('a failed hand-off goes again with the SAME Idempotency-Key, and the text is sent one time', async () => {
    const handOffs = [json({ error: 'steering_invalid' }, 502), json({ items: [HANDED] })];
    const waits: number[] = [];
    const { c, stream, seen, onLeftover } = await streaming(
      (method, path) => {
        if (path === HANDOFF) return handOffs.shift()!;
        if (method === 'POST') return json(EXAMPLE, 202);
        return json({ items: [{ ...EXAMPLE, state: 'turn_ended' }] });
      },
      async (ms) => {
        waits.push(ms);
      },
    );
    await expect(c.steer(EXAMPLE.content)).resolves.toBe('queued');
    stream.connections[0].push(sse('execution.node_event', { type: 'pipeline_finish', content: 'done' }, 1));
    await flush();
    const keys = seen.filter((s) => s.path === HANDOFF).map((s) => s.key);
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    expect(waits).toEqual([500]);
    expect(onLeftover).toHaveBeenCalledTimes(1);
    expect(onLeftover).toHaveBeenCalledWith(EXAMPLE.content);
  });

  it('reads the list again while an own message is still pending, then hands it off', async () => {
    const lists = [json({ items: [EXAMPLE] }), json({ items: [{ ...EXAMPLE, state: 'turn_ended' }] })];
    const waits: number[] = [];
    const { c, stream, onLeftover } = await streaming(
      (method, path) => {
        if (path === HANDOFF) return json({ items: [HANDED] });
        if (method === 'POST') return json(EXAMPLE, 202);
        return lists.shift() ?? json({ items: [] });
      },
      async (ms) => {
        waits.push(ms);
      },
    );
    await expect(c.steer(EXAMPLE.content)).resolves.toBe('queued');
    stream.connections[0].push(sse('execution.node_event', { type: 'pipeline_finish', content: 'done' }, 1));
    await flush();
    expect(waits).toEqual([1500]);
    expect(onLeftover).toHaveBeenCalledWith(EXAMPLE.content);
  });

  it('steering_not_supported: the text is not sent and steering stops for this turn', async () => {
    const { c } = await streaming(() => json({ error: 'steering_not_supported' }, 409));
    await expect(c.steer('hello')).resolves.toBe('not_supported');
    expect(c.canSteer).toBe(false);
    expect(steerOutcomeText('not_supported')).toMatch(/can't read new messages/);
    c.detach();
  });

  it('turn_not_running: the text goes as the next message when the turn ends', async () => {
    const { c, stream, onLeftover } = await streaming((method) => (method === 'POST' ? json({ error: 'turn_not_running' }, 409) : json({ items: [] })));
    await expect(c.steer('too late')).resolves.toBe('after_turn');
    expect(onLeftover).not.toHaveBeenCalled();
    stream.connections[0].push(sse('execution.node_event', { type: 'pipeline_finish', content: 'done' }, 1));
    await flush();
    expect(onLeftover).toHaveBeenCalledWith('too late');
  });

  it('steering_queue_full keeps the text with the user', async () => {
    const { c } = await streaming(() => json({ error: 'steering_queue_full' }, 409));
    await expect(c.steer('one more')).resolves.toBe('queue_full');
    expect(steerOutcomeText('queue_full')).toMatch(/most waiting messages/);
    c.detach();
  });

  it('rate_limited: waits retry_after_ms, shows the wait, then sends again with the SAME Idempotency-Key', async () => {
    const posted = [json({ ...RATE_LIMITED, retry_after_ms: 3000 }, 429), json(EXAMPLE, 202)];
    const waits: number[] = [];
    let shownAt: number | null | undefined;
    const t = await streaming(
      (method) => (method === 'POST' ? posted.shift()! : json({ items: [] })),
      async (ms) => {
        waits.push(ms);
        shownAt = t.c.getSnapshot().steeringRetryAt;
      },
    );
    const before = Date.now();
    await expect(t.c.steer(EXAMPLE.content)).resolves.toBe('queued');
    expect(waits).toEqual([3000]);
    expect(shownAt).toBeGreaterThanOrEqual(before + 3000);
    expect(t.c.getSnapshot().steeringRetryAt).toBeNull();
    const keys = t.seen.filter((s) => s.method === 'POST' && s.path === ROUTE).map((s) => s.key);
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
    expect(t.c.getSnapshot().ownSteering).toEqual([EXAMPLE.id]);
    t.c.detach();
  });

  it('rate_limited after every retry: the text stays with the user, and each try used the same key', async () => {
    const t = await streaming((method) => (method === 'POST' ? json({ error: 'rate_limited' }, 429) : json({ items: [] })));
    await expect(t.c.steer('busy')).resolves.toBe('rate_limited');
    const keys = t.seen.filter((s) => s.method === 'POST' && s.path === ROUTE).map((s) => s.key);
    expect(keys).toHaveLength(4);
    expect(new Set(keys).size).toBe(1);
    expect(steerOutcomeText('rate_limited')).toMatch(/Too many messages/);
    t.c.detach();
  });

  it('the composer hint counts the wait down in whole seconds', () => {
    expect(steeringWaitText(10_000, 7_000)).toMatch(/send again in 3 s/);
    expect(steeringWaitText(10_000, 8_100)).toMatch(/send again in 2 s/);
    expect(steeringWaitText(10_000, 10_000)).toBeNull();
    expect(steeringWaitText(null, 0)).toBeNull();
  });
});
