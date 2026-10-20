import { MIGRATIONS, migrate } from '@/db/migrations';
import { deleteConversation, dropProject, upsertConversationRow, upsertMessageRow, type ConversationMessage, type ConversationSummary } from '@/db/repo';
import { enforceRetention } from '@/db/retention';
import { ftsQuery, readPinnedConversationRows, searchConversations, snippetParts } from '@/db/search';
import { Db } from '@/db/sql';
import { nodeSqliteDriver, openNodeSqlite, tempDir } from '@/test/nodeSqlite';

const row = (id: number, name: string, updatedAt: string, extra: Partial<ConversationSummary> = {}): ConversationSummary => ({
  id,
  name,
  created_at: updatedAt,
  updated_at: updatedAt,
  duration: -1,
  message_groups_count: 0,
  ...extra,
});
const message = (id: number, content: string, createdAt = '2026-10-07T10:00:00Z'): ConversationMessage => ({
  id: String(id),
  uid: `u-${id}`,
  conversation_id: '1',
  role: 'assistant',
  content,
  created_at: createdAt,
});

async function seeded(db = new Db(openNodeSqlite())) {
  await migrate(db);
  await db.transaction(async (tx) => {
    await upsertConversationRow(tx, 7, row(1, 'Q3 planning', '2026-10-07T09:00:00Z'), 1);
    await upsertConversationRow(tx, 7, row(2, 'Film club charter', '2026-10-06T09:00:00Z', { is_pinned: true }), 1);
    await upsertConversationRow(tx, 7, row(3, 'Trip to Lisbon', '2026-10-01T09:00:00Z'), 1);
    await upsertConversationRow(tx, 8, row(4, 'Q3 planning (other project)', '2026-10-07T09:00:00Z'), 1);
    await upsertMessageRow(tx, 7, 2, message(21, 'Draft the bylaws for the club, and pick a café near the station.'), 1);
    await upsertMessageRow(tx, 7, 3, message(31, 'Pastéis de nata at Manteigaria; the tram 28 goes up to the castle.', '2026-10-01T09:00:00Z'), 1);
    await upsertMessageRow(tx, 7, 3, message(32, 'Book the planning call for Friday.', '2026-10-01T09:05:00Z'), 1);
    await upsertMessageRow(tx, 8, 4, message(41, 'bylaws in another project'), 1);
  });
  return db;
}

const ids = (hits: { id: number }[]) => hits.map((hit) => hit.id);

describe('ftsQuery', () => {
  it('quotes every word and makes the last one a prefix', () => {
    expect(ftsQuery('  q3   plan ')).toBe('"q3" "plan"*');
  });
  it('treats FTS syntax and quotes as text', () => {
    expect(ftsQuery('a" OR b NEAR(c) *')).toBe('"a" "OR" "b" "NEAR(c)"*');
    expect(ftsQuery('"" * - :')).toBeNull();
    expect(ftsQuery('   ')).toBeNull();
  });
  it('keeps at most eight words', () => {
    expect(ftsQuery('a b c d e f g h i j')!.split(' ')).toHaveLength(8);
  });
});

describe('snippetParts', () => {
  it('splits the marked passage into matched and plain parts', () => {
    expect(snippetParts('…pick a \u0002café\u0003 near\n the')).toEqual([
      { text: '…pick a ', match: false },
      { text: 'café', match: true },
      { text: ' near the', match: false },
    ]);
  });
});

describe('searchConversations (FTS5 inside the offline store)', () => {
  it('finds titles, then message bodies, in this project only', async () => {
    const db = await seeded();
    const hits = await searchConversations(db, 7, 'planning');
    // Title match first (1), then the message match (3).
    expect(ids(hits)).toEqual([1, 3]);
    expect(hits[0].snippet).toBeNull();
    expect(hits[1].snippet!.filter((p) => p.match).map((p) => p.text)).toEqual(['planning']);
    expect(hits[1].name).toBe('Trip to Lisbon');
    expect(ids(await searchConversations(db, 8, 'planning'))).toEqual([4]);
  });

  it('matches as you type (prefix), ignoring case and diacritics', async () => {
    const db = await seeded();
    expect(ids(await searchConversations(db, 7, 'BYLA'))).toEqual([2]);
    expect(ids(await searchConversations(db, 7, 'cafe'))).toEqual([2]);
    expect(ids(await searchConversations(db, 7, 'pasteis nata'))).toEqual([3]);
    expect(ids(await searchConversations(db, 7, 'nata bylaws'))).toEqual([]);
  });

  it('returns the list row with the hit and one hit per conversation', async () => {
    const db = await seeded();
    const [hit] = await searchConversations(db, 7, 'the');
    expect(hit.row).toMatchObject({ id: expect.any(Number), name: expect.any(String) });
    const all = await searchConversations(db, 7, 'the');
    expect(new Set(ids(all)).size).toBe(all.length);
  });

  it('one chat with many matching messages does not crowd the others out', async () => {
    const db = await seeded();
    await db.transaction(async (tx) => {
      await upsertConversationRow(tx, 7, row(10, 'Agent transcript', '2026-10-07T08:00:00Z'), 1);
      await upsertConversationRow(tx, 7, row(11, 'Release notes', '2026-10-05T08:00:00Z'), 1);
      // More matching messages than the old ranked window (limit * 10) held.
      for (let i = 0; i < 60; i++) await upsertMessageRow(tx, 7, 10, message(1000 + i, `error error error in step ${i}`), 1);
      await upsertMessageRow(tx, 7, 11, message(1100, 'Fixed one error in the importer, among many other changes to read through.'), 1);
    });
    expect(ids(await searchConversations(db, 7, 'error', 5))).toEqual([10, 11]);
    const other = (await searchConversations(db, 7, 'error', 5)).find((hit) => hit.id === 11);
    expect(other?.snippet?.some((part) => part.match && part.text.toLowerCase() === 'error')).toBe(true);
  });

  it('follows renames and rewritten messages (the triggers keep the index in step)', async () => {
    const db = await seeded();
    await db.transaction(async (tx) => {
      await upsertConversationRow(tx, 7, row(1, 'Budget review', '2026-10-07T11:00:00Z'), 1);
      await upsertMessageRow(tx, 7, 3, message(32, 'Moved to Monday.'), 1);
    });
    expect(ids(await searchConversations(db, 7, 'budget'))).toEqual([1]);
    expect(ids(await searchConversations(db, 7, 'planning'))).toEqual([]);
    expect(ids(await searchConversations(db, 7, 'monday'))).toEqual([3]);
  });

  it('erases a removed message’s words from the index itself (FTS5 secure-delete), not just from results', async () => {
    const db = await seeded();
    for (const table of ['message_search', 'conversation_search']) {
      expect(await db.get(`SELECT v FROM ${table}_config WHERE k = 'secure-delete'`)).toEqual({ v: 1 });
    }
    const holds = async (word: string) =>
      (await db.all<{ block: Uint8Array }>('SELECT block FROM message_search_data')).some((r) => String.fromCharCode(...r.block).includes(word));
    expect(await holds('manteigaria')).toBe(true);
    await db.transaction((tx) => deleteConversation(tx, 7, 3));
    expect(await holds('manteigaria')).toBe(false);
  });

  it('forgets what the store forgets: a tombstone, retention, a lost project', async () => {
    const db = await seeded();
    await db.transaction((tx) => deleteConversation(tx, 7, 2));
    expect(ids(await searchConversations(db, 7, 'bylaws'))).toEqual([]);
    await enforceRetention(db, { offline_retention_days: 3, offline_max_mb: 0 }, Date.parse('2026-10-07T12:00:00Z'));
    expect(ids(await searchConversations(db, 7, 'lisbon'))).toEqual([]);
    expect(ids(await searchConversations(db, 7, 'castle'))).toEqual([]);
    await db.transaction((tx) => dropProject(tx, 8));
    expect(ids(await searchConversations(db, 8, 'planning'))).toEqual([]);
    for (const table of ['conversation_search', 'message_search']) {
      const left = await db.get<{ n: number }>(`SELECT count(*) AS n FROM ${table}`);
      const rows = await db.get<{ n: number }>(`SELECT count(*) AS n FROM ${table === 'conversation_search' ? 'conversations' : 'messages'}`);
      expect(left!.n).toBe(rows!.n);
    }
  });

  it('indexes rows a schema-2 file already held (the upgrade backfills)', async () => {
    const db = new Db(openNodeSqlite());
    await migrate(db, MIGRATIONS.slice(0, 2));
    await db.run("INSERT INTO conversations (project_id, id, name, row_json) VALUES (7, 1, 'Old title', '{}')");
    await db.run("INSERT INTO messages (project_id, conversation_id, id, row_json) VALUES (7, 1, 5, ?), (7, 1, 6, 'not json')", [
      JSON.stringify({ content: 'kept offline before the update' }),
    ]);
    await migrate(db);
    expect(ids(await searchConversations(db, 7, 'old'))).toEqual([1]);
    expect(ids(await searchConversations(db, 7, 'kept offline'))).toEqual([1]);
  });

  it('keeps the index inside the database file: no other file is written', async () => {
    const { readdirSync } = require('node:fs') as { readdirSync(path: string): string[] };
    const dir = tempDir('az-search-');
    const driver = nodeSqliteDriver(dir);
    const db = await seeded(new Db(await driver.open('ws.db')));
    expect(ids(await searchConversations(db, 7, 'bylaws'))).toEqual([2]);
    const names = (await db.all<{ name: string }>("SELECT name FROM sqlite_master WHERE name LIKE '%search%' ORDER BY name")).map((r) => r.name);
    expect(names).toEqual(expect.arrayContaining(['conversation_search', 'conversation_search_data', 'message_search', 'message_search_data']));
    await db.close();
    expect(readdirSync(dir).filter((name) => !/^ws\.db(-wal|-shm|-journal)?$/.test(name))).toEqual([]);
  });
});

describe('readPinnedConversationRows', () => {
  it('reads the pinned rows of the synced list', async () => {
    const db = await seeded();
    expect((await readPinnedConversationRows(db, 7)).map((r) => r.id)).toEqual([2]);
    expect(await readPinnedConversationRows(db, 8)).toEqual([]);
  });
});
