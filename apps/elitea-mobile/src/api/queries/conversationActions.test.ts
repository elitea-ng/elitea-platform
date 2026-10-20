import { cleanConversationName, CONVERSATION_NAME_MAX, sendConversationAction } from '@/api/queries/conversationActions';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

beforeEach(() => {
  __resetSessionForTests();
  setSession({ hydrated: true, serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'oauth', token: 'elnat_test' });
  jest.spyOn(console, 'warn').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

const CONV = '/elitea_core/conversation/prompt_lib/7/41';
const PIN = '/social/pin/prompt_lib/7/conversation/41';

describe('sendConversationAction', () => {
  it('renames with PUT and the name alone (contract 1.4)', async () => {
    const server = fakeServer();
    server.on('PUT', CONV, { status: 200, body: { id: 41, name: 'Budget' } });
    expect(await sendConversationAction(7, 41, { kind: 'rename', name: '  Budget  ' }, server.client)).toEqual({ outcome: 'done' });
    expect(server.requests[0].method).toBe('PUT');
    expect(JSON.parse(server.requests[0].body!)).toEqual({ name: 'Budget' });
  });

  it('pins with POST and unpins with DELETE on the shared conversation pin', async () => {
    const server = fakeServer();
    server.on('POST', PIN, { status: 200, body: { ok: true } });
    server.on('DELETE', PIN, { status: 200, body: { ok: true } });
    expect(await sendConversationAction(7, 41, { kind: 'pin', pinned: true }, server.client)).toEqual({ outcome: 'done' });
    expect(await sendConversationAction(7, 41, { kind: 'pin', pinned: false }, server.client)).toEqual({ outcome: 'done' });
    expect(server.requests.map((r) => `${r.method} ${r.url.pathname}`)).toEqual([`POST /api/v2${PIN}`, `DELETE /api/v2${PIN}`]);
  });

  it('a delete of a conversation already gone is done; a rename of one is gone', async () => {
    const server = fakeServer();
    server.on('DELETE', CONV, { status: 404, body: { error: 'not found' } });
    server.on('PUT', CONV, { status: 404, body: { error: 'not found' } });
    expect(await sendConversationAction(7, 41, { kind: 'delete' }, server.client)).toEqual({ outcome: 'done' });
    expect(await sendConversationAction(7, 41, { kind: 'rename', name: 'x' }, server.client)).toEqual({ outcome: 'gone' });
  });

  it('retries no answer, 429 (with Retry-After) and 5xx; refuses 403 for good', async () => {
    const server = fakeServer();
    server.on('DELETE', CONV, 'network_error', { status: 429, headers: { 'Retry-After': '7' }, body: { error: 'slow down' } }, { status: 503, body: {} }, { status: 403, body: { error: 'forbidden' } });
    expect((await sendConversationAction(7, 41, { kind: 'delete' }, server.client)).outcome).toBe('retry');
    expect(await sendConversationAction(7, 41, { kind: 'delete' }, server.client)).toEqual({ outcome: 'retry', retryAfterMs: 7000 });
    expect((await sendConversationAction(7, 41, { kind: 'delete' }, server.client)).outcome).toBe('retry');
    expect(await sendConversationAction(7, 41, { kind: 'delete' }, server.client)).toEqual({ outcome: 'rejected', status: 403 });
  });

  it('never sends a blank name', async () => {
    const server = fakeServer();
    expect(await sendConversationAction(7, 41, { kind: 'rename', name: ' \n ' }, server.client)).toEqual({ outcome: 'rejected' });
    expect(server.requests).toEqual([]);
  });
});

describe('cleanConversationName', () => {
  it('trims, folds line breaks and caps the length', () => {
    expect(cleanConversationName('  Trip\nto Lisbon ')).toBe('Trip to Lisbon');
    expect(cleanConversationName('   ')).toBeNull();
    expect(cleanConversationName('x'.repeat(400))).toHaveLength(CONVERSATION_NAME_MAX);
  });

  // Client contract 1.4: the server answers 400 (and the queued rename is dropped) for any
  // control character (Go unicode.IsControl: U+0000-U+001F, U+007F-U+009F) and for more than
  // 256 code points.
  it('replaces every control character the server refuses', () => {
    const controls = [...Array(0x20).keys(), ...Array.from({ length: 0x21 }, (_, i) => 0x7f + i)].map((c) => String.fromCharCode(c));
    const name = cleanConversationName(`Plan${controls.join('')}B\u000bC\u0085D`);
    expect(name).toBe('Plan B C D');
    expect(name).not.toMatch(/[\u0000-\u001f\u007f-\u009f]/);
  });

  it('counts code points like the server and never splits a surrogate pair', () => {
    const name = cleanConversationName('🙂'.repeat(300));
    expect(Array.from(name ?? '')).toHaveLength(CONVERSATION_NAME_MAX);
    expect(name).toBe('🙂'.repeat(CONVERSATION_NAME_MAX));
    const odd = cleanConversationName('a' + '🙂'.repeat(300));
    expect(odd).not.toMatch(/[\ud800-\udbff]$/);
    expect(Array.from(odd ?? '')).toHaveLength(CONVERSATION_NAME_MAX);
  });
});
