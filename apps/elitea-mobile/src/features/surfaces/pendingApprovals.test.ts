import { migrate } from '@/db/migrations';
import { Db } from '@/db/sql';
import { readPendingApprovals } from '@/features/surfaces/pendingApprovals';
import { conversationRow, messageRow } from '@/sync/__fixtures__/wp6';
import { openNodeSqlite } from '@/test/nodeSqlite';

/**
 * Transcript rows in the server's delta shape (src/sync/__fixtures__/wp6.ts):
 * group id as a string, `uid`, `role`, `metadata`. A paused answer records
 * its pause in the metadata (`hitl_interrupts` / `authorization_requests` /
 * `output_limit_reached`, the keys `pauseFromGroup` rebuilds a pause from).
 */

const P = 7;
const approval = {
  thread_id: 'e9322c94-054b-49f9-bb1b-c22195969e93',
  hitl_interrupts: [
    {
      interrupt_id: 'int-1',
      guardrail_type: 'sensitive_tool',
      tool_call_id: 'call-1',
      tool_name: 'mock_tool_status',
      available_actions: ['approve', 'reject'],
    },
  ],
};

async function store() {
  const db = new Db(openNodeSqlite());
  await migrate(db);
  return db;
}

async function conversation(db: Db, id: number, updatedMs: number, uuid: string | null, rows: Record<string, unknown>[]) {
  await db.run('INSERT INTO conversations (project_id, id, uuid, name, updated_ms, row_json) VALUES (?, ?, ?, ?, ?, ?)', [
    P,
    id,
    uuid,
    `Conversation ${id}`,
    updatedMs,
    JSON.stringify(conversationRow(id)),
  ]);
  for (const row of rows) {
    await db.run('INSERT INTO messages (project_id, conversation_id, id, uid, row_json) VALUES (?, ?, ?, ?, ?)', [
      P,
      id,
      Number(row.id),
      row.uid as string,
      JSON.stringify(row),
    ]);
  }
}

it('counts the conversations whose newest answer waits for a decision, newest first', async () => {
  const db = await store();
  await conversation(db, 1, 100, 'aaaaaaaa-0000-4000-8000-000000000001', [messageRow(11), messageRow(12, { metadata: approval })]);
  // An answered pause: a newer answer follows it.
  await conversation(db, 2, 200, null, [messageRow(21), messageRow(22, { metadata: approval }), messageRow(23), messageRow(24)]);
  // A toolkit sign-in (authorization request) waits too.
  await conversation(db, 3, 300, null, [
    messageRow(31),
    messageRow(32, { metadata: { authorization_requests: [{ interrupt_id: 'auth-1', tool_call_id: 'c', guardrail_type: 'mcp_auth', server_url: 'https://mcp.example.test' }] } }),
  ]);
  // The output-limit offer is not a decision.
  await conversation(db, 4, 400, null, [messageRow(41), messageRow(42, { metadata: { output_limit_reached: true, thread_id: 't' } })]);
  // Still streaming: not a pause yet.
  await conversation(db, 5, 500, null, [messageRow(51), messageRow(52, { metadata: approval, is_streaming: true })]);

  expect(await readPendingApprovals(db, P)).toEqual([
    { ref: '3', name: 'Conversation 3' },
    { ref: 'aaaaaaaa-0000-4000-8000-000000000001', name: 'Conversation 1' },
  ]);
  expect(await readPendingApprovals(db, 8)).toEqual([]);
});

it('skips a row it cannot read', async () => {
  const db = await store();
  await conversation(db, 1, 100, null, [messageRow(11)]);
  await db.run("UPDATE messages SET row_json = 'not json' WHERE id = 11");
  expect(await readPendingApprovals(db, P)).toEqual([]);
});
