import type { ActionOutcome } from '@/api/queries/conversationActions';
import { migrate } from '@/db/migrations';
import { dropProject, upsertConversationRow, upsertMessageRow } from '@/db/repo';
import { readPinnedConversationRows, searchConversations } from '@/db/search';
import { Db } from '@/db/sql';
import { openNodeSqlite } from '@/test/nodeSqlite';
import {
  applyOverlay,
  ConversationOpsDrainer,
  enqueueOp,
  listOps,
  opBackoffMs,
  overlayOf,
  type ConversationOp,
} from '@/sync/conversationOps';

const row = (id: number, name: string) => ({ id, name, created_at: '2026-10-07T09:00:00Z', updated_at: '2026-10-07T09:00:00Z', duration: -1, message_groups_count: 1 });

async function store() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  await db.transaction(async (tx) => {
    await upsertConversationRow(tx, 7, row(41, 'Q3 planning'), 1);
    await upsertConversationRow(tx, 7, row(42, 'Film club'), 1);
    await upsertMessageRow(tx, 7, 42, { id: '1', uid: 'u1', conversation_id: '42', role: 'user', content: 'bylaws draft', created_at: '2026-10-07T09:00:00Z' }, 1);
  });
  return db;
}

function drainer(db: Db | null, answers: ActionOutcome[] | ((op: ConversationOp) => ActionOutcome), extra: { online?: boolean; now?: number } = {}) {
  const sent: ConversationOp[] = [];
  const settled: [string, string][] = [];
  const wakes: number[] = [];
  const queue = Array.isArray(answers) ? [...answers] : null;
  const d = new ConversationOpsDrainer({
    getDb: () => db,
    send: async (op) => {
      sent.push(op);
      return queue ? (queue.shift() ?? { outcome: 'done' }) : (answers as (op: ConversationOp) => ActionOutcome)(op);
    },
    isOnline: () => extra.online ?? true,
    now: () => extra.now ?? 1_000,
    schedule: (delay) => {
      wakes.push(delay);
      return () => {};
    },
    onSettled: (op, outcome) => settled.push([op.action.kind, outcome]),
  });
  return { d, sent, settled, wakes };
}

describe('enqueueOp', () => {
  it('keeps one action per conversation and kind (the last word wins)', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'pin', pinned: true }, 1);
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'A' }, 2);
    await enqueueOp(db, 7, 41, { kind: 'pin', pinned: false }, 3);
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'B' }, 4);
    expect((await listOps(db, 7)).map((op) => op.action)).toEqual([
      { kind: 'pin', pinned: false },
      { kind: 'rename', name: 'B' },
    ]);
  });

  it('a delete drops the conversation’s pending rename and pin, and nothing is queued after it', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'A' });
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    await enqueueOp(db, 7, 41, { kind: 'pin', pinned: true });
    expect((await listOps(db, 7)).map((op) => op.action)).toEqual([{ kind: 'delete' }]);
  });

  it('is dropped with its project (403)', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    await db.transaction((tx) => dropProject(tx, 7));
    expect(await listOps(db)).toEqual([]);
  });
});

describe('overlay', () => {
  it('shows queued renames, pins and deletes over the rows', () => {
    const overlay = overlayOf([
      { conversationId: 1, action: { kind: 'rename', name: 'New' } },
      { conversationId: 2, action: { kind: 'pin', pinned: true } },
      { conversationId: 3, action: { kind: 'delete' } },
    ]);
    const rows = [
      { id: 1, name: 'Old', is_pinned: false },
      { id: 2, name: 'Two' },
      { id: 3, name: 'Gone' },
      { id: 4, name: 'Same' },
    ];
    expect(applyOverlay(rows, overlay)).toEqual([
      { id: 1, name: 'New', is_pinned: false },
      { id: 2, name: 'Two', is_pinned: true },
      { id: 4, name: 'Same' },
    ]);
  });
});

describe('ConversationOpsDrainer', () => {
  it('sends each action and brings the saved copy in line once the workspace took it', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'Budget review' }, 1);
    await enqueueOp(db, 7, 41, { kind: 'pin', pinned: true }, 2);
    await enqueueOp(db, 7, 42, { kind: 'delete' }, 3);
    const { d, sent, settled } = drainer(db, [{ outcome: 'done' }, { outcome: 'done' }, { outcome: 'done' }]);
    await d.drain();
    expect(sent.map((op) => op.action.kind)).toEqual(['rename', 'pin', 'delete']);
    expect(settled).toEqual([
      ['rename', 'done'],
      ['pin', 'done'],
      ['delete', 'done'],
    ]);
    expect(await listOps(db)).toEqual([]);
    expect(await db.all('SELECT id, name FROM conversations')).toEqual([{ id: 41, name: 'Budget review' }]);
    expect((await readPinnedConversationRows(db, 7)).map((r) => [r.id, r.name, r.is_pinned])).toEqual([[41, 'Budget review', true]]);
    // The index follows: the new title is found, the deleted chat's messages are not.
    expect((await searchConversations(db, 7, 'budget')).map((h) => h.id)).toEqual([41]);
    expect(await searchConversations(db, 7, 'bylaws')).toEqual([]);
  });

  it('a taken delete also drops the conversation’s unsent messages (they could only fail)', async () => {
    const db = await store();
    await db.run("UPDATE conversations SET uuid = 'uuid-41' WHERE id = 41");
    const outbox = (questionId: string, conversationId: number, ref: string, uuid: string) =>
      db.run(
        `INSERT INTO outbox (question_id, project_id, conversation_ref, conversation_uuid, body_json, user_text, created_ms)
         VALUES (?, 7, ?, ?, '{}', 'hi', ?)`,
        [questionId, ref, uuid, conversationId],
      );
    await outbox('q-by-id', 41, '41', 'uuid-41');
    await outbox('q-by-uuid', 41, 'uuid-41', 'uuid-41');
    await outbox('q-other', 42, '42', 'uuid-42');
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    const { d } = drainer(db, [{ outcome: 'done' }]);
    await d.drain();
    expect(await db.all('SELECT question_id FROM outbox')).toEqual([{ question_id: 'q-other' }]);
  });

  it('keeps an unanswered action with backoff and wakes for it', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    const { d, settled, wakes } = drainer(db, [{ outcome: 'retry' }]);
    await d.drain();
    const [op] = await listOps(db);
    expect(op).toMatchObject({ attempts: 1, nextAttemptMs: 1_000 + opBackoffMs(1) });
    expect(settled).toEqual([]);
    expect(wakes).toEqual([opBackoffMs(1)]);
    // Not due yet: nothing is sent.
    const again = drainer(db, [{ outcome: 'done' }]);
    await again.d.drain();
    expect(again.sent).toEqual([]);
  });

  it('honours Retry-After and caps the backoff', () => {
    expect(opBackoffMs(1)).toBe(2000);
    expect(opBackoffMs(3)).toBe(8000);
    expect(opBackoffMs(30)).toBe(600_000);
    expect(opBackoffMs(1, 30_000)).toBe(30_000);
  });

  it('drops a refused action and reports it, leaving the saved copy as the server has it', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'Nope' });
    const { d, settled } = drainer(db, [{ outcome: 'rejected', status: 403 }]);
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    await d.drain();
    expect(settled).toEqual([['rename', 'rejected']]);
    expect(await listOps(db)).toEqual([]);
    expect(await db.get('SELECT name FROM conversations WHERE id = 41')).toEqual({ name: 'Q3 planning' });
  });

  it('a rename or pin answered 404 (gone) forgets the conversation instead of saving a value the server never took', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'Never applied' }, 1);
    await enqueueOp(db, 7, 42, { kind: 'pin', pinned: true }, 2);
    const { d, settled } = drainer(db, [{ outcome: 'gone' }, { outcome: 'gone' }]);
    await d.drain();
    expect(settled).toEqual([
      ['rename', 'gone'],
      ['pin', 'gone'],
    ]);
    expect(await listOps(db)).toEqual([]);
    // Deleted on the web, or access lost: either way the caller cannot see it any more.
    expect(await db.all('SELECT id FROM conversations')).toEqual([]);
    expect(await searchConversations(db, 7, 'never')).toEqual([]);
    expect(await readPinnedConversationRows(db, 7)).toEqual([]);
  });

  it('sends nothing while the device is offline (the reconnect drains)', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    const { d, sent } = drainer(db, [{ outcome: 'done' }], { online: false });
    await d.drain();
    expect(sent).toEqual([]);
    expect(await listOps(db)).toHaveLength(1);
  });

  it('a newer action queued while the older one was in flight stays queued', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'rename', name: 'First' }, 1);
    let calls = 0;
    const { d } = drainer(db, () => {
      calls++;
      return { outcome: 'done' };
    });
    const realSend = (d as unknown as { deps: { send: (op: ConversationOp) => Promise<ActionOutcome> } }).deps;
    const send = realSend.send;
    realSend.send = async (op) => {
      if (op.action.kind === 'rename' && op.action.name === 'First') await enqueueOp(db, 7, 41, { kind: 'rename', name: 'Second' }, 2);
      return send(op);
    };
    await d.drain();
    expect(calls).toBe(1);
    expect((await listOps(db)).map((op) => op.action)).toEqual([{ kind: 'rename', name: 'Second' }]);
    // The saved copy is not set to the superseded name.
    expect(await db.get('SELECT name FROM conversations WHERE id = 41')).toEqual({ name: 'Q3 planning' });
  });

  it('notify resolves once every listener finished reloading', async () => {
    const { d } = drainer(null, []);
    const seen: string[] = [];
    d.subscribe(async () => {
      await new Promise((resolve) => setTimeout(resolve, 5));
      seen.push('reloaded');
    });
    d.subscribe(() => {
      seen.push('sync');
    });
    await d.notify();
    expect(seen).toEqual(['sync', 'reloaded']);
  });

  it('holds while the update screen is up: nothing is sent, and a 426 burns no attempt or backoff', async () => {
    const db = await store();
    await enqueueOp(db, 7, 41, { kind: 'delete' });
    let blocked = true;
    const sent: ConversationOp[] = [];
    const wakes: number[] = [];
    const d = new ConversationOpsDrainer({
      getDb: () => db,
      send: async (op) => {
        sent.push(op);
        blocked = true; // the send itself got the 426 (the middleware sets upgradeRequired)
        return { outcome: 'retry' };
      },
      isBlocked: () => blocked,
      now: () => 1_000,
      schedule: (delay) => {
        wakes.push(delay);
        return () => {};
      },
    });
    await d.drain();
    expect(sent).toEqual([]);
    blocked = false;
    await d.drain();
    expect(sent).toHaveLength(1);
    const [op] = await listOps(db);
    expect(op).toMatchObject({ attempts: 0, nextAttemptMs: 0 });
    expect(wakes).toEqual([]);
  });

  it('does nothing while the store is closed', async () => {
    const { d, sent } = drainer(null, [{ outcome: 'done' }]);
    await d.drain();
    expect(sent).toEqual([]);
  });
});
