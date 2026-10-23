import { deleteConversations } from '@/db/repo';
import type { Db } from '@/db/sql';
import type { ClientPolicy } from '@/security/policy';

/**
 * Retention by policy (ADR-0025 decision 5): `offline_retention_days` and
 * `offline_max_mb`. Run when the store opens, on resume and after a sync.
 *
 * - Age: a conversation whose last server activity is older than the
 *   retention window is dropped with its whole transcript and its transcript
 *   cursor (opening it here does not extend its life on the device). Last
 *   activity is the later of the row's `updated_at` and its newest stored
 *   message: a chat turn does not stamp the conversation's `updated_at`
 *   (only renames and settings do), so `updated_at` alone would drop a
 *   long-lived thread messaged yesterday. A transcript is kept or dropped as
 *   a unit, so an offline thread is never silently missing its middle.
 *   Notifications go by `created_at`.
 * - Size: while the live pages exceed `offline_max_mb`, the least recently
 *   active conversations (server update or last opened here) go first, then the oldest notifications. Freed pages
 *   are returned to the file system (`incremental_vacuum`).
 *
 * Sync cursors are kept: a dropped item comes back only if it changes again
 * on the server, which is the point of retention.
 */

const DAY_MS = 86_400_000;
const MB = 1024 * 1024;
/** Notifications and orphan messages are evicted in batches; conversations one at a time. */
const EVICT_BATCH = 10;

export interface RetentionResult {
  conversations: number;
  notifications: number;
}

/** Newest stored message of the conversation row in scope (server time). */
const NEWEST_MESSAGE =
  '(SELECT MAX(m.created_ms) FROM messages m WHERE m.project_id = conversations.project_id AND m.conversation_id = conversations.id)';
/** Last server activity: the row's update or its newest message. */
const SERVER_ACTIVITY = `MAX(COALESCE(updated_ms, 0), COALESCE(${NEWEST_MESSAGE}, 0))`;
/** Size-cap order: server activity or the last time it was opened here. */
const ACTIVITY = `MAX(${SERVER_ACTIVITY}, COALESCE(opened_ms, 0))`;

/** Bytes held by live pages (freed pages are not counted). */
export async function liveBytes(db: Db): Promise<number> {
  const pages = await db.get<{ page_count: number }>('PRAGMA page_count;');
  const free = await db.get<{ freelist_count: number }>('PRAGMA freelist_count;');
  const size = await db.get<{ page_size: number }>('PRAGMA page_size;');
  return ((pages?.page_count ?? 0) - (free?.freelist_count ?? 0)) * (size?.page_size ?? 4096);
}

export async function enforceRetention(
  db: Db,
  policy: Pick<ClientPolicy, 'offline_retention_days' | 'offline_max_mb'>,
  now: number = Date.now(),
): Promise<RetentionResult> {
  const result: RetentionResult = { conversations: 0, notifications: 0 };
  if (policy.offline_retention_days > 0) {
    const cutoff = now - policy.offline_retention_days * DAY_MS;
    await db.transaction(async (tx) => {
      result.conversations += await deleteConversations(tx, `${SERVER_ACTIVITY} < ?`, [cutoff]);
      const gone = await tx.run('DELETE FROM notifications WHERE COALESCE(created_ms, 0) < ?', [cutoff]);
      result.notifications += gone.changes;
    });
  }

  if (policy.offline_max_mb > 0) {
    const cap = policy.offline_max_mb * MB;
    for (let round = 0; round < 10_000 && (await liveBytes(db)) > cap; round++) {
      const evicted = await db.transaction(async (tx) => {
        const conversations = await deleteConversations(
          tx,
          `rowid IN (SELECT rowid FROM conversations ORDER BY ${ACTIVITY} ASC LIMIT 1)`,
          [],
        );
        if (conversations > 0) {
          result.conversations += conversations;
          return conversations;
        }
        const notifications = await tx.run(
          `DELETE FROM notifications WHERE rowid IN (SELECT rowid FROM notifications ORDER BY COALESCE(created_ms, 0) ASC LIMIT ${EVICT_BATCH})`,
        );
        // Orphan transcripts (synced for a conversation outside the list).
        const orphans = await tx.run(
          `DELETE FROM messages WHERE rowid IN (SELECT rowid FROM messages ORDER BY COALESCE(created_ms, 0) ASC LIMIT ${EVICT_BATCH * 10})`,
        );
        result.notifications += notifications.changes;
        return notifications.changes + orphans.changes;
      });
      if (evicted === 0) break;
    }
  }

  if (result.conversations > 0 || result.notifications > 0) {
    await db.exec('PRAGMA incremental_vacuum;');
  }
  return result;
}
