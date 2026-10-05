import { ApiError } from '@/api/errors';
import { migrate } from '@/db/migrations';
import { Db, type SqlExecutor } from '@/db/sql';
import { DeltaShapeError, type DeltaPage } from '@/sync/deltaApi';
import { FULL_SYNC_CURSOR, syncScope, type ScopeSpec } from '@/sync/engine';
import { openNodeSqlite } from '@/test/nodeSqlite';

/** A minimal list over the notifications table: rows `{id, v}`. */
type Row = { id: number; v: string };

async function setup() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  return db;
}

function spec(fetch: (cursor: string) => Promise<DeltaPage<Row>>, hooks: { failApplyOn?: string } = {}): ScopeSpec<Row> & { calls: string[] } {
  const calls: string[] = [];
  return {
    calls,
    scope: 'test:7',
    projectId: 7,
    fetch: (cursor) => {
      calls.push(cursor);
      return fetch(cursor);
    },
    apply: async (tx: SqlExecutor, page, gen) => {
      for (const t of page.tombstones) await tx.run('DELETE FROM notifications WHERE project_id = 7 AND id = ?', [t.id]);
      for (const row of page.rows) {
        await tx.run(
          `INSERT INTO notifications (project_id, id, row_json, sync_gen) VALUES (7, ?, ?, ?)
           ON CONFLICT (project_id, id) DO UPDATE SET row_json = excluded.row_json, sync_gen = excluded.sync_gen`,
          [row.id, row.v, gen],
        );
        if (hooks.failApplyOn === row.v) throw new Error('crash between statements');
      }
    },
    sweep: async (tx, gen) => {
      await tx.run('DELETE FROM notifications WHERE project_id = 7 AND sync_gen < ?', [gen]);
    },
  };
}

const page = (rows: Row[], next: string, hasMore = false, tombstoneIds: number[] = []): DeltaPage<Row> => ({
  rows,
  tombstones: tombstoneIds.map((id) => ({ id, uuid: null, reason: 'deleted', deleted_at: '2026-10-04T12:00:00Z' })),
  nextCursor: next,
  hasMore,
});

const stored = async (db: Db) =>
  (await db.all<{ id: number; row_json: string }>('SELECT id, row_json FROM notifications ORDER BY id')).map((r) => `${r.id}=${r.row_json}`);
const cursorOf = async (db: Db) => (await db.get<{ cursor: string }>("SELECT cursor FROM sync_state WHERE scope = 'test:7'"))?.cursor;

const http = (status: number, code?: string) => new ApiError('http', `HTTP ${status}`, { status, code });

beforeEach(() => {
  jest.spyOn(console, 'warn').mockImplementation(() => {});
  jest.spyOn(console, 'info').mockImplementation(() => {});
});
afterEach(() => jest.restoreAllMocks());

describe('syncScope', () => {
  it('starts from 0, follows has_more and stores each cursor with its page', async () => {
    const db = await setup();
    const pages: Record<string, DeltaPage<Row>> = {
      [FULL_SYNC_CURSOR]: page([{ id: 1, v: 'a' }], 'c1', true),
      c1: page([{ id: 2, v: 'b' }], 'c2', false),
    };
    const s = spec(async (c) => pages[c]);
    expect(await syncScope(db, s)).toMatchObject({ status: 'ok', pages: 2, rows: 2 });
    expect(s.calls).toEqual(['0', 'c1']);
    expect(await stored(db)).toEqual(['1=a', '2=b']);
    expect(await cursorOf(db)).toBe('c2');
  });

  it('upserts by id: a settle-window repeat changes nothing, a newer copy replaces the row', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }], 'c1')));
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }, { id: 2, v: 'b' }], 'c2')));
    await syncScope(db, spec(async () => page([{ id: 2, v: 'b2' }], 'c3')));
    expect(await stored(db)).toEqual(['1=a', '2=b2']);
  });

  it('applies tombstones as deletes', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }, { id: 2, v: 'b' }], 'c1')));
    await syncScope(db, spec(async () => page([], 'c2', false, [1])));
    expect(await stored(db)).toEqual(['2=b']);
  });

  it('a crash while applying a page keeps the previous page and its cursor, and resumes there', async () => {
    const db = await setup();
    const pages: Record<string, DeltaPage<Row>> = {
      [FULL_SYNC_CURSOR]: page([{ id: 1, v: 'a' }], 'c1', true),
      c1: page([{ id: 2, v: 'b' }, { id: 3, v: 'boom' }], 'c2', false),
    };
    await expect(syncScope(db, spec(async (c) => pages[c], { failApplyOn: 'boom' }))).rejects.toThrow('crash');
    expect(await stored(db)).toEqual(['1=a']);
    expect(await cursorOf(db)).toBe('c1');
    const resumed = spec(async (c) => pages[c]);
    await syncScope(db, resumed);
    expect(resumed.calls).toEqual(['c1']);
    expect(await stored(db)).toEqual(['1=a', '2=b', '3=boom']);
  });

  it('410: resyncs from 0 and sweeps what the full pass did not return', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }, { id: 2, v: 'b' }], 'old')));
    const s = spec(async (c) => {
      if (c === 'old') throw http(410, 'sync_cursor_expired');
      return page([{ id: 2, v: 'b' }, { id: 3, v: 'c' }], 'fresh');
    });
    expect(await syncScope(db, s)).toMatchObject({ status: 'ok', resynced: true });
    expect(s.calls).toEqual(['old', '0']);
    expect(await stored(db)).toEqual(['2=b', '3=c']);
    expect(await cursorOf(db)).toBe('fresh');
  });

  it('a full resync interrupted offline keeps the old rows readable, then completes', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }, { id: 2, v: 'b' }], 'old')));
    let online = true;
    const fetch = async (c: string) => {
      if (c === 'old') throw http(410, 'sync_cursor_expired');
      if (c === 'p2' && !online) throw new ApiError('network', 'offline');
      return c === '0' ? page([{ id: 2, v: 'b' }], 'p2', true) : page([{ id: 4, v: 'd' }], 'done');
    };
    online = false;
    expect(await syncScope(db, spec(fetch))).toEqual({ status: 'offline' });
    expect(await stored(db)).toEqual(['1=a', '2=b']); // nothing swept yet
    online = true;
    expect(await syncScope(db, spec(fetch))).toMatchObject({ status: 'ok' });
    expect(await stored(db)).toEqual(['2=b', '4=d']);
  });

  it('400 invalid_sync_cursor is a full resync too; other 400s are errors', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }], 'foreign')));
    const s = spec(async (c) => {
      if (c === 'foreign') throw http(400, 'invalid_sync_cursor');
      return page([], 'fresh');
    });
    expect(await syncScope(db, s)).toMatchObject({ status: 'ok', resynced: true });
    expect(await stored(db)).toEqual([]);
    const bad = await syncScope(db, spec(async () => Promise.reject(http(400, 'invalid_limit'))));
    expect(bad.status).toBe('error');
    expect(await cursorOf(db)).toBe('fresh');
  });

  it('does not loop when the full resync itself is refused', async () => {
    const db = await setup();
    const s = spec(async () => Promise.reject(http(410, 'sync_cursor_expired')));
    expect((await syncScope(db, s)).status).toBe('error');
    expect(s.calls).toEqual(['0']);
  });

  it('reports 403, 404, no answer and a malformed page without moving the cursor', async () => {
    const db = await setup();
    await syncScope(db, spec(async () => page([{ id: 1, v: 'a' }], 'c1')));
    expect(await syncScope(db, spec(async () => Promise.reject(http(403))))).toEqual({ status: 'forbidden' });
    expect(await syncScope(db, spec(async () => Promise.reject(http(404))))).toEqual({ status: 'not_found' });
    expect(await syncScope(db, spec(async () => Promise.reject(new ApiError('timeout', 't'))))).toEqual({ status: 'offline' });
    expect((await syncScope(db, spec(async () => Promise.reject(new DeltaShapeError('x'))))).status).toBe('error');
    expect(await cursorOf(db)).toBe('c1');
    expect(await stored(db)).toEqual(['1=a']);
  });

  it('stops when has_more comes back with the same cursor, and after maxPages', async () => {
    const db = await setup();
    const stuck = spec(async () => page([{ id: 1, v: 'a' }], '0', true));
    expect(await syncScope(db, stuck)).toMatchObject({ status: 'ok', pages: 1 });
    let n = 0;
    const endless = spec(async () => page([{ id: ++n, v: 'x' }], `c${n}`, true));
    expect(await syncScope(db, endless, { maxPages: 3 })).toMatchObject({ pages: 3 });
  });
});
