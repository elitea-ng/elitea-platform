import { migrate } from '@/db/migrations';
import { messagesScope } from '@/db/repo';
import { enforceRetention, liveBytes } from '@/db/retention';
import { Db } from '@/db/sql';
import { openNodeSqlite } from '@/test/nodeSqlite';

const DAY = 86_400_000;
const NOW = Date.UTC(2026, 9, 4);

async function seeded() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  return db;
}

async function conversation(db: Db, id: number, updatedDaysAgo: number, opts: { openedDaysAgo?: number; messages?: number; text?: string } = {}) {
  await db.run(
    'INSERT INTO conversations (project_id, id, name, updated_ms, opened_ms, row_json) VALUES (7, ?, ?, ?, ?, ?)',
    [id, `c${id}`, NOW - updatedDaysAgo * DAY, opts.openedDaysAgo === undefined ? null : NOW - opts.openedDaysAgo * DAY, '{}'],
  );
  for (let m = 1; m <= (opts.messages ?? 1); m++) {
    await db.run('INSERT INTO messages (project_id, conversation_id, id, created_ms, row_json) VALUES (7, ?, ?, ?, ?)', [
      id,
      id * 1000 + m,
      NOW - updatedDaysAgo * DAY,
      JSON.stringify({ content: opts.text ?? 'x' }),
    ]);
  }
  await db.run('INSERT INTO sync_state (scope, project_id, cursor) VALUES (?, 7, ?)', [messagesScope(7, id), 'cur']);
}

const ids = async (db: Db, table: string) =>
  (await db.all<{ id: number }>(`SELECT id FROM ${table} ORDER BY id`)).map((r) => r.id);

describe('enforceRetention', () => {
  it('drops conversations (with transcript and cursor) and notifications older than the window', async () => {
    const db = await seeded();
    await conversation(db, 1, 40, { openedDaysAgo: 1 });
    await conversation(db, 2, 5);
    await db.run("INSERT INTO notifications (project_id, id, created_ms, row_json) VALUES (7, 1, ?, '{}'), (7, 2, ?, '{}')", [
      NOW - 31 * DAY,
      NOW - DAY,
    ]);
    const result = await enforceRetention(db, { offline_retention_days: 30, offline_max_mb: 0 }, NOW);
    expect(result).toEqual({ conversations: 1, notifications: 1 });
    expect(await ids(db, 'conversations')).toEqual([2]);
    expect(await ids(db, 'messages')).toEqual([2001]);
    expect(await ids(db, 'notifications')).toEqual([2]);
    expect(await db.all('SELECT scope FROM sync_state')).toEqual([{ scope: messagesScope(7, 2) }]);
  });

  it('measures a conversation by its newest stored message too: chatting does not move the server updated_at', async () => {
    const db = await seeded();
    // Created and last renamed 40 days ago (updated_at), messaged yesterday.
    await conversation(db, 1, 40);
    await db.run("INSERT INTO messages (project_id, conversation_id, id, created_ms, row_json) VALUES (7, 1, 1999, ?, '{}')", [NOW - DAY]);
    await conversation(db, 2, 40);
    const result = await enforceRetention(db, { offline_retention_days: 30, offline_max_mb: 0 }, NOW);
    expect(result.conversations).toBe(1);
    expect(await ids(db, 'conversations')).toEqual([1]);
    // Kept as a unit: the transcript is never missing its middle.
    expect(await ids(db, 'messages')).toEqual([1001, 1999]);
  });

  it('keeps everything inside the window', async () => {
    const db = await seeded();
    await conversation(db, 1, 29);
    expect(await enforceRetention(db, { offline_retention_days: 30, offline_max_mb: 0 }, NOW)).toEqual({
      conversations: 0,
      notifications: 0,
    });
  });

  it('evicts the least recently active conversations until the store fits the size cap', async () => {
    const db = await seeded();
    const big = 'x'.repeat(4000);
    // 1 is the oldest by update but was opened today; 3 is the least recently active.
    await conversation(db, 1, 9, { openedDaysAgo: 0, messages: 200, text: big });
    await conversation(db, 2, 2, { messages: 200, text: big });
    await conversation(db, 3, 8, { messages: 200, text: big });
    expect(await liveBytes(db)).toBeGreaterThan(2 * 1024 * 1024);
    const result = await enforceRetention(db, { offline_retention_days: 90, offline_max_mb: 2 }, NOW);
    expect(result.conversations).toBeGreaterThanOrEqual(1);
    expect(await liveBytes(db)).toBeLessThanOrEqual(2 * 1024 * 1024);
    const left = await ids(db, 'conversations');
    expect(left).not.toContain(3);
    expect(left).toContain(1);
  });

  it('does nothing with retention 0 and no cap', async () => {
    const db = await seeded();
    await conversation(db, 1, 400);
    expect(await enforceRetention(db, { offline_retention_days: 0, offline_max_mb: 0 }, NOW)).toEqual({
      conversations: 0,
      notifications: 0,
    });
  });
});
