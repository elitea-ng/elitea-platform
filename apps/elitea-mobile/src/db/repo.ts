import type { components } from '@/api/generated/elitea.gen';
import type { Db, SqlExecutor, SqlValue } from '@/db/sql';
import { parseServerTime } from '@/lib/time';

/**
 * Row-level reads and writes of the offline store. Rows are kept in the
 * server's own JSON shape (`row_json`) next to the few columns the store
 * queries by; nothing here interprets message content.
 */

export type ConversationSummary = components['schemas']['ConversationSummary'];
export type ConversationDetail = components['schemas']['ConversationDetail'];
export type ConversationMessage = components['schemas']['ConversationMessage'];
export type Notification = components['schemas']['Notification'];

/** sync_gen of a conversation known only from a thread read (never swept by a list resync). */
export const DETAIL_ONLY_GEN = -1;

/** The sync scope of a project's conversation list (the app always lists with `mine=true`). */
export function conversationsScope(projectId: number): string {
  return `conv:${projectId}:mine`;
}

/** The sync scope of one conversation's transcript. */
export function messagesScope(projectId: number, conversationId: number): string {
  return `msg:${projectId}:${conversationId}`;
}

/** The sync scope of the caller's notifications, read through a project. */
export function notificationsScope(projectId: number): string {
  return `notif:${projectId}`;
}

/** True once a sync pass of `scope` has completed on this device (a saved copy exists, even an empty one). */
export async function hasSynced(db: Db, scope: string): Promise<boolean> {
  const row = await db.get<{ synced_ms: number | null }>('SELECT synced_ms FROM sync_state WHERE scope = ?', [scope]);
  return row !== null && row.synced_ms !== null;
}

/** Deletes conversations matching `where`, with their transcripts and transcript cursors. */
export async function deleteConversations(tx: SqlExecutor, where: string, params: readonly SqlValue[]): Promise<number> {
  const doomed = await tx.all<{ project_id: number; id: number }>(
    `SELECT project_id, id FROM conversations WHERE ${where}`,
    params,
  );
  for (const row of doomed) {
    await tx.run('DELETE FROM messages WHERE project_id = ? AND conversation_id = ?', [row.project_id, row.id]);
    await tx.run('DELETE FROM sync_state WHERE scope = ?', [messagesScope(row.project_id, row.id)]);
    await tx.run('DELETE FROM conversations WHERE project_id = ? AND id = ?', [row.project_id, row.id]);
  }
  return doomed.length;
}

/** Forgets one conversation (deleted, access lost) and its transcript. */
export async function deleteConversation(tx: SqlExecutor, projectId: number, conversationId: number): Promise<void> {
  await deleteConversations(tx, 'project_id = ? AND id = ?', [projectId, conversationId]);
  // A transcript synced without a list row.
  await tx.run('DELETE FROM messages WHERE project_id = ? AND conversation_id = ?', [projectId, conversationId]);
  await tx.run('DELETE FROM sync_state WHERE scope = ?', [messagesScope(projectId, conversationId)]);
}

/** Everything of one project (the caller lost access to it: 403), unsent messages included. */
export async function dropProject(tx: SqlExecutor, projectId: number): Promise<void> {
  for (const table of ['messages', 'conversations', 'notifications', 'sync_state', 'outbox']) {
    await tx.run(`DELETE FROM ${table} WHERE project_id = ?`, [projectId]);
  }
}

// --- conversations ---------------------------------------------------------

/**
 * Upserts a list row and marks its transcript for a sync. Every delivered row
 * marks it, even one byte-identical to the stored row: the server re-delivers
 * a conversation when one of its message groups changed (a trigger bumps the
 * conversation's sync_at), and a group rewrite (an answer that finished
 * streaming, an edit) changes neither `updated_at` nor
 * `message_groups_count`. A settle-window repeat costs one transcript delta
 * read from its cursor.
 */
export async function upsertConversationRow(
  tx: SqlExecutor,
  projectId: number,
  row: ConversationSummary,
  gen: number,
): Promise<void> {
  const json = JSON.stringify(row);
  await tx.run(
    `INSERT INTO conversations (project_id, id, name, updated_ms, row_json, sync_gen, messages_dirty)
     VALUES (?, ?, ?, ?, ?, ?, 1)
     ON CONFLICT (project_id, id) DO UPDATE SET
       messages_dirty = 1,
       name = excluded.name,
       updated_ms = excluded.updated_ms,
       row_json = excluded.row_json,
       sync_gen = excluded.sync_gen`,
    [projectId, row.id, typeof row.name === 'string' ? row.name : '', parseServerTime(row.updated_at), json, gen],
  );
}

/**
 * Keeps the last full read of a conversation (participants and uuid, without
 * its message groups) so the thread can be shown and addressed offline.
 */
export async function saveConversationDetail(
  db: Db,
  projectId: number,
  detail: ConversationDetail,
  now: number = Date.now(),
): Promise<void> {
  const id = Number(detail.id);
  if (!Number.isSafeInteger(id) || id <= 0) return;
  const { message_groups: _omit, ...rest } = detail;
  await db.run(
    `INSERT INTO conversations (project_id, id, uuid, name, updated_ms, detail_json, opened_ms, sync_gen)
     VALUES (?, ?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT (project_id, id) DO UPDATE SET
       uuid = excluded.uuid,
       name = excluded.name,
       updated_ms = MAX(COALESCE(conversations.updated_ms, 0), COALESCE(excluded.updated_ms, 0)),
       detail_json = excluded.detail_json,
       opened_ms = excluded.opened_ms`,
    [
      projectId,
      id,
      typeof detail.uuid === 'string' && detail.uuid !== '' ? detail.uuid : null,
      typeof detail.name === 'string' ? detail.name : '',
      parseServerTime(detail.updated_at),
      JSON.stringify(rest),
      now,
      DETAIL_ONLY_GEN,
    ],
  );
}

export async function readConversationRows(
  db: Db,
  projectId: number,
  limit: number,
  offset: number,
): Promise<{ rows: ConversationSummary[]; total: number }> {
  const rows = await db.all<{ row_json: string }>(
    `SELECT row_json FROM conversations WHERE project_id = ? AND row_json IS NOT NULL
     ORDER BY COALESCE(updated_ms, 0) DESC, id DESC LIMIT ? OFFSET ?`,
    [projectId, limit, offset],
  );
  const total = await db.get<{ n: number }>(
    'SELECT count(*) AS n FROM conversations WHERE project_id = ? AND row_json IS NOT NULL',
    [projectId],
  );
  return { rows: rows.map((r) => JSON.parse(r.row_json) as ConversationSummary), total: total?.n ?? 0 };
}

export interface ConversationRecord {
  id: number;
  row: ConversationSummary | null;
  /** The last full read, without its message groups. */
  detail: ConversationDetail | null;
}

/** A conversation by numeric id or uuid. */
export async function readConversation(db: Db, projectId: number, ref: string): Promise<ConversationRecord | null> {
  const byId = /^[1-9]\d{0,15}$/.test(ref);
  const found = await db.get<{ id: number; row_json: string | null; detail_json: string | null }>(
    byId
      ? 'SELECT id, row_json, detail_json FROM conversations WHERE project_id = ? AND id = ?'
      : 'SELECT id, row_json, detail_json FROM conversations WHERE project_id = ? AND uuid = ?',
    [projectId, byId ? Number(ref) : ref.toLowerCase()],
  );
  if (found === null) return null;
  return {
    id: found.id,
    row: found.row_json ? (JSON.parse(found.row_json) as ConversationSummary) : null,
    detail: found.detail_json ? (JSON.parse(found.detail_json) as ConversationDetail) : null,
  };
}

/** Conversations whose transcript should be synced next, most recently updated first. */
export async function dirtyConversations(db: Db, projectId: number, limit: number): Promise<number[]> {
  const rows = await db.all<{ id: number }>(
    `SELECT id FROM conversations WHERE project_id = ? AND messages_dirty = 1 AND row_json IS NOT NULL
     ORDER BY COALESCE(updated_ms, 0) DESC LIMIT ?`,
    [projectId, limit],
  );
  return rows.map((r) => r.id);
}

// --- messages --------------------------------------------------------------

/** The numeric message group id of a transcript row (the wire spells it as a string). */
export function messageGroupId(row: { id?: unknown }): number | null {
  const value = typeof row.id === 'number' ? row.id : typeof row.id === 'string' && /^\d{1,16}$/.test(row.id) ? Number(row.id) : NaN;
  return Number.isSafeInteger(value) && value > 0 ? value : null;
}

export async function upsertMessageRow(
  tx: SqlExecutor,
  projectId: number,
  conversationId: number,
  row: ConversationMessage,
  gen: number,
): Promise<void> {
  const id = messageGroupId(row);
  if (id === null) return;
  await tx.run(
    `INSERT INTO messages (project_id, conversation_id, id, uid, created_ms, row_json, sync_gen)
     VALUES (?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT (project_id, conversation_id, id) DO UPDATE SET
       uid = excluded.uid, created_ms = excluded.created_ms, row_json = excluded.row_json, sync_gen = excluded.sync_gen`,
    [projectId, conversationId, id, typeof row.uid === 'string' ? row.uid : null, parseServerTime(row.created_at), JSON.stringify(row), gen],
  );
}

export async function readMessageRows(db: Db, projectId: number, conversationId: number): Promise<ConversationMessage[]> {
  const rows = await db.all<{ row_json: string }>(
    'SELECT row_json FROM messages WHERE project_id = ? AND conversation_id = ? ORDER BY COALESCE(created_ms, 0) ASC, id ASC',
    [projectId, conversationId],
  );
  return rows.map((r) => JSON.parse(r.row_json) as ConversationMessage);
}

// --- notifications ---------------------------------------------------------

export async function upsertNotificationRow(tx: SqlExecutor, projectId: number, row: Notification, gen: number): Promise<void> {
  await tx.run(
    `INSERT INTO notifications (project_id, id, uuid, is_seen, created_ms, row_json, sync_gen)
     VALUES (?, ?, ?, ?, ?, ?, ?)
     ON CONFLICT (project_id, id) DO UPDATE SET
       uuid = excluded.uuid, is_seen = excluded.is_seen, created_ms = excluded.created_ms,
       row_json = excluded.row_json, sync_gen = excluded.sync_gen`,
    [projectId, row.id, row.uuid ?? null, row.is_seen ? 1 : 0, parseServerTime(row.created_at), JSON.stringify(row), gen],
  );
}

export async function readNotificationRows(db: Db, projectId: number, limit: number): Promise<Notification[]> {
  const rows = await db.all<{ row_json: string }>(
    'SELECT row_json FROM notifications WHERE project_id = ? ORDER BY COALESCE(created_ms, 0) DESC, id DESC LIMIT ?',
    [projectId, limit],
  );
  return rows.map((r) => JSON.parse(r.row_json) as Notification);
}

export async function countUnseenNotifications(db: Db, projectId: number): Promise<number> {
  const row = await db.get<{ n: number }>('SELECT count(*) AS n FROM notifications WHERE project_id = ? AND is_seen = 0', [projectId]);
  return row?.n ?? 0;
}
