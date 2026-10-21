import type { Db } from '@/db/sql';
import { pauseFromGroup } from '@/features/chat/pauses/continueBodies';
import type { PendingConversation } from '@/features/surfaces/surfaceSnapshot';

/**
 * Conversations of a project whose NEWEST answer waits for the user (an
 * approval, a clarifying question or a toolkit sign-in), from the offline
 * store after a sync. Only the newest message of a conversation can be
 * pending (the same rule the thread uses, `pauseFromGroup`).
 *
 * The store holds the conversations sync brought down (newest first, the
 * retention window); a conversation it never synced is not counted. With no
 * offline store (`offline_retention_days` 0) only the live turns of this run
 * of the app are known.
 */

/** Conversations examined, newest first: enough for a count, bounded for a read on every sync. */
export const PENDING_SCAN_LIMIT = 200;

export async function readPendingApprovals(db: Db, projectId: number): Promise<PendingConversation[]> {
  const rows = await db.all<{ id: number; uuid: string | null; name: string; row_json: string }>(
    `SELECT c.id AS id, c.uuid AS uuid, c.name AS name, m.row_json AS row_json
       FROM conversations c
       JOIN messages m ON m.project_id = c.project_id AND m.conversation_id = c.id
      WHERE c.project_id = ?
        AND m.id = (SELECT MAX(m2.id) FROM messages m2 WHERE m2.project_id = c.project_id AND m2.conversation_id = c.id)
      ORDER BY COALESCE(c.updated_ms, 0) DESC, c.id DESC
      LIMIT ?`,
    [projectId, PENDING_SCAN_LIMIT],
  );
  const pending: PendingConversation[] = [];
  for (const row of rows) {
    let group: Record<string, unknown> | null = null;
    try {
      const parsed: unknown = JSON.parse(row.row_json);
      group = parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : null;
    } catch {
      group = null;
    }
    // A question the user asked last is never a pause.
    if (group === null || group.role === 'user') continue;
    const pause = pauseFromGroup(group);
    if (pause === null || pause.kind === 'output_limit') continue;
    pending.push({ ref: row.uuid ?? String(row.id), name: row.name });
  }
  return pending;
}
