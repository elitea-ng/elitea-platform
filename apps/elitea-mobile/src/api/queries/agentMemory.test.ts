import {
  AgentMemoryError,
  agentMemoryPageOf,
  canDeleteEntry,
  deleteAgentMemoryEntry,
  fetchAgentMemoryPage,
  nextAgentMemoryOffset,
  setAgentMemoryOptOut,
  withoutEntry,
  type AgentMemoryPage,
} from '@/api/queries/agentMemory';
import { __resetSessionForTests, setCredential, setSession } from '@/state/session';
import { FAKE_ORIGIN, fakeServer } from '@/test/fakeServer';

jest.mock('expo-secure-store', () => require('@/test/fakeSecureStore').fakeSecureStoreModule());

beforeEach(() => {
  __resetSessionForTests();
  setSession({ serverOrigin: FAKE_ORIGIN, projectId: 7 });
  setCredential({ kind: 'pat', token: 't' });
});

const ROUTE = '/elitea_core/agent_memory/prompt_lib/7/41';

const entry = (id: number, extra: Record<string, unknown> = {}) => ({
  id,
  application_id: 41,
  kind: 'fact',
  content: `Note ${id}`,
  subject_user_id: null,
  pinned: false,
  status: 'active',
  version: 1,
  conversation_uuid: null,
  message_id: null,
  execution_id: null,
  written_by_kind: 'agent',
  written_by_user_id: null,
  created_at: '2026-11-01T10:00:00Z',
  updated_at: '2026-11-01T10:00:00Z',
  ...extra,
});

const list = (rows: unknown[], extra: Record<string, unknown> = {}) => ({
  rows,
  total: rows.length,
  people: [],
  can_manage: false,
  can_delete: false,
  can_approve: false,
  opted_out: false,
  ...extra,
});

describe('agentMemoryPageOf', () => {
  it('reads rows, people and the flags, and drops malformed or repeated rows', () => {
    const page = agentMemoryPageOf(
      list([entry(1, { subject_user_id: 5 }), { id: 'x' }, entry(1), entry(2, { pinned: true })], {
        total: 9,
        people: [{ id: 5, name: ' Ada ' }, { id: 6, name: '' }, { nope: true }],
        can_delete: true,
        opted_out: true,
      }),
      50,
    );
    expect(page.rows.map((r) => r.id)).toEqual([1, 2]);
    expect(page.rows[0].subjectUserId).toBe(5);
    expect(page.rows[1].pinned).toBe(true);
    expect(page.people).toEqual({ 5: 'Ada' });
    expect(page).toMatchObject({ total: 9, canDelete: true, optedOut: true, offset: 50, received: 4 });
  });

  it('refuses a body that is not the list', () => {
    expect(() => agentMemoryPageOf({ rows: [] }, 0)).toThrow(AgentMemoryError);
  });
});

describe('paging', () => {
  const page = (offset: number, ids: number[], total: number): AgentMemoryPage => ({
    rows: ids.map((id) => agentMemoryPageOf(list([entry(id)]), 0).rows[0]),
    total,
    people: {},
    canDelete: false,
    optedOut: false,
    offset,
    received: ids.length,
  });

  it('asks for the next page until every row is read', () => {
    expect(nextAgentMemoryOffset(page(0, [1, 2], 3))).toBe(2);
    expect(nextAgentMemoryOffset(page(2, [3], 3))).toBeUndefined();
    expect(nextAgentMemoryOffset(page(0, [], 3))).toBeUndefined();
  });

  it('a delete steps the later pages back so the next page skips nothing', () => {
    const pages = withoutEntry([page(0, [1, 2], 5), page(2, [3, 4], 5)], 1);
    expect(pages[0].rows.map((r) => r.id)).toEqual([2]);
    expect(pages.map((p) => p.total)).toEqual([4, 4]);
    expect(pages[1].offset).toBe(1);
    // Three rows read of four left: the next page starts at the server's offset 3.
    expect(nextAgentMemoryOffset(pages[1])).toBe(3);
  });
});

describe('canDeleteEntry', () => {
  it('allows a note about the caller, or any note with can_delete', () => {
    expect(canDeleteEntry({ subjectUserId: 5 }, false, 5)).toBe(true);
    expect(canDeleteEntry({ subjectUserId: 6 }, false, 5)).toBe(false);
    expect(canDeleteEntry({ subjectUserId: null }, false, 5)).toBe(false);
    expect(canDeleteEntry({ subjectUserId: null }, true, null)).toBe(true);
  });
});

describe('the routes', () => {
  it('lists a page with limit and offset', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 200, body: list([entry(3)], { total: 51 }) });
    const page = await fetchAgentMemoryPage(7, 41, 50, server.client);
    expect(page.rows[0].content).toBe('Note 3');
    const url = server.requests[0].url;
    expect(url.searchParams.get('limit')).toBe('50');
    expect(url.searchParams.get('offset')).toBe('50');
  });

  it("reads the router's 404 (`{error: 'not found'}`, no code) as a server without the routes", async () => {
    const server = fakeServer();
    // What elitea-main's router answers for an unknown route (router.go r.NotFound, apierr.WriteStatus).
    server.on('GET', ROUTE, { status: 404, body: { error: 'not found' } });
    await expect(fetchAgentMemoryPage(7, 41, 0, server.client)).rejects.toMatchObject({ reason: 'not_available' });
  });

  it("keeps the memory handler's own 404 (with a code) as a refusal", async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, { status: 404, body: { error: 'The agent does not exist.', code: 'not_found' } });
    await expect(fetchAgentMemoryPage(7, 41, 0, server.client)).rejects.toMatchObject({ reason: 'refused', code: 'not_found' });
  });

  it("shows a refusal in the server's own words", async () => {
    const server = fakeServer();
    server.on('DELETE', `${ROUTE}/9`, { status: 403, body: { error: 'Only a manager may delete this note.', code: 'memory_change_forbidden' } });
    await expect(deleteAgentMemoryEntry(7, 41, 9, server.client)).rejects.toMatchObject({
      reason: 'refused',
      message: 'Only a manager may delete this note.',
      code: 'memory_change_forbidden',
      status: 403,
    });
  });

  it('deletes one note', async () => {
    const server = fakeServer();
    server.on('DELETE', `${ROUTE}/9`, { status: 204 });
    await deleteAgentMemoryEntry(7, 41, 9, server.client);
    expect(server.requests[0].method).toBe('DELETE');
  });

  it('stores the opt-out and answers the stored choice', async () => {
    const server = fakeServer();
    server.on('PUT', '/elitea_core/agent_memory_opt_out/prompt_lib/7/41', { status: 200, body: { opted_out: true } });
    await expect(setAgentMemoryOptOut(7, 41, true, server.client)).resolves.toBe(true);
    expect(JSON.parse(server.requests[0].body ?? '{}')).toEqual({ opted_out: true });
  });

  it('a network failure says it needs a connection', async () => {
    const server = fakeServer();
    server.on('GET', ROUTE, 'network_error');
    await expect(fetchAgentMemoryPage(7, 41, 0, server.client)).rejects.toMatchObject({ reason: 'offline' });
  });
});
