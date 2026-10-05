import { sessionApiClient, type ApiClient } from '@/api/client';
import {
  conversationsScope,
  deleteConversation,
  deleteConversations,
  dirtyConversations,
  dropProject,
  messageGroupId,
  messagesScope,
  notificationsScope,
  upsertConversationRow,
  upsertMessageRow,
  upsertNotificationRow,
  type ConversationMessage,
  type ConversationSummary,
  type Notification,
} from '@/db/repo';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';
import { fetchConversationChanges, fetchMessageChanges, fetchNotificationChanges } from '@/sync/deltaApi';
import { syncScope, tombstoneIds, type ScopeSpec, type SyncOutcome } from '@/sync/engine';

/**
 * What is synced, and when it stops:
 *
 * 1. the project's conversation list (`mine=true`), then
 * 2. the transcripts of conversations whose row was delivered since their last
 *    transcript sync (most recently updated first, `TRANSCRIPTS_PER_RUN` per
 *    run; the rest follow on the next run), then
 * 3. the caller's notifications.
 *
 * A 403 on the conversation or notification list means the caller lost the
 * project: everything stored for it is dropped and the run stops. A 403 or
 * 404 on one transcript drops that conversation only. Offline stops the run
 * quietly; every cursor stays where it was.
 *
 * One run per project at a time; a request while one runs is folded into it.
 */

export const TRANSCRIPTS_PER_RUN = 20;

export interface ProjectSyncResult {
  conversations: SyncOutcome['status'];
  transcripts: number;
  notifications: SyncOutcome['status'] | 'skipped';
  /** The project was dropped from the store (403). */
  dropped: boolean;
}

export interface SyncManagerDeps {
  getDb: () => Db | null;
  client?: () => ApiClient;
  now?: () => number;
  /** Called after a run changed stored data (screens reading the store refresh). */
  onChanged?: (projectId: number) => void;
}

function conversationsSpec(db: Db, client: ApiClient, projectId: number): ScopeSpec<ConversationSummary> {
  return {
    scope: conversationsScope(projectId),
    projectId,
    fetch: (cursor) => fetchConversationChanges(client, projectId, cursor),
    apply: async (tx, page, gen) => {
      for (const id of tombstoneIds(page.tombstones)) await deleteConversation(tx, projectId, id);
      for (const row of page.rows) await upsertConversationRow(tx, projectId, row, gen);
    },
    // Conversations known only from a thread read (sync_gen -1) are not part of the list.
    sweep: (tx, gen) => deleteConversations(tx, 'project_id = ? AND sync_gen >= 0 AND sync_gen < ?', [projectId, gen]).then(() => {}),
  };
}

function messagesSpec(client: ApiClient, projectId: number, conversationId: number): ScopeSpec<ConversationMessage> {
  return {
    scope: messagesScope(projectId, conversationId),
    projectId,
    fetch: (cursor) => fetchMessageChanges(client, projectId, conversationId, cursor),
    apply: async (tx, page, gen) => {
      for (const id of tombstoneIds(page.tombstones)) {
        await tx.run('DELETE FROM messages WHERE project_id = ? AND conversation_id = ? AND id = ?', [projectId, conversationId, id]);
      }
      for (const row of page.rows) {
        if (messageGroupId(row) !== null) await upsertMessageRow(tx, projectId, conversationId, row, gen);
      }
    },
    sweep: async (tx, gen) => {
      await tx.run('DELETE FROM messages WHERE project_id = ? AND conversation_id = ? AND sync_gen < ?', [projectId, conversationId, gen]);
    },
  };
}

function notificationsSpec(client: ApiClient, projectId: number): ScopeSpec<Notification> {
  return {
    scope: notificationsScope(projectId),
    projectId,
    fetch: (cursor) => fetchNotificationChanges(client, projectId, cursor),
    apply: async (tx, page, gen) => {
      for (const id of tombstoneIds(page.tombstones)) {
        await tx.run('DELETE FROM notifications WHERE project_id = ? AND id = ?', [projectId, id]);
      }
      for (const row of page.rows) await upsertNotificationRow(tx, projectId, row, gen);
    },
    sweep: async (tx, gen) => {
      await tx.run('DELETE FROM notifications WHERE project_id = ? AND sync_gen < ?', [projectId, gen]);
    },
  };
}

export class SyncManager {
  private readonly running = new Map<number, Promise<ProjectSyncResult | null>>();
  private readonly again = new Set<number>();

  constructor(private readonly deps: SyncManagerDeps) {}

  private client(): ApiClient {
    return (this.deps.client ?? sessionApiClient)();
  }

  /** Syncs a project; concurrent calls share the run (and one more run follows if asked meanwhile). */
  syncProject(projectId: number): Promise<ProjectSyncResult | null> {
    const current = this.running.get(projectId);
    if (current) {
      this.again.add(projectId);
      return current;
    }
    const run = (async () => {
      let result: ProjectSyncResult | null = null;
      do {
        this.again.delete(projectId);
        result = await this.runProject(projectId);
      } while (this.again.has(projectId) && result !== null && !result.dropped && result.conversations === 'ok');
      return result;
    })().finally(() => {
      this.running.delete(projectId);
      this.again.delete(projectId);
    });
    this.running.set(projectId, run);
    return run;
  }

  /** Fire and forget. */
  requestSync(projectId: number | null): void {
    if (projectId === null || this.deps.getDb() === null) return;
    this.syncProject(projectId).catch((error: unknown) => log.warn('sync failed', { error }));
  }

  private async runProject(projectId: number): Promise<ProjectSyncResult | null> {
    const db = this.deps.getDb();
    if (db === null) return null;
    const client = this.client();
    const result: ProjectSyncResult = { conversations: 'ok', transcripts: 0, notifications: 'skipped', dropped: false };
    let changed = false;

    const conversations = await syncScope(db, conversationsSpec(db, client, projectId), { now: this.deps.now });
    result.conversations = conversations.status;
    if (conversations.status === 'forbidden') {
      await this.drop(db, projectId);
      result.dropped = true;
      this.deps.onChanged?.(projectId);
      return result;
    }
    if (conversations.status === 'ok') {
      changed ||= conversations.rows + conversations.tombstones > 0 || conversations.resynced;
      for (const conversationId of await dirtyConversations(db, projectId, TRANSCRIPTS_PER_RUN)) {
        const outcome = await this.syncTranscriptIn(db, client, projectId, conversationId);
        if (outcome === 'offline') break;
        result.transcripts++;
        changed = true;
      }
    }
    if (conversations.status !== 'offline') {
      const notifications = await syncScope(db, notificationsSpec(client, projectId), { now: this.deps.now });
      result.notifications = notifications.status;
      if (notifications.status === 'forbidden') {
        await this.drop(db, projectId);
        result.dropped = true;
      }
      if (notifications.status === 'ok') changed ||= notifications.rows + notifications.tombstones > 0;
    }
    if (changed || result.dropped) this.deps.onChanged?.(projectId);
    return result;
  }

  /** One conversation's transcript (thread open). */
  async syncTranscript(projectId: number, conversationId: number): Promise<SyncOutcome['status'] | null> {
    const db = this.deps.getDb();
    if (db === null) return null;
    const status = await this.syncTranscriptIn(db, this.client(), projectId, conversationId);
    if (status !== 'offline') this.deps.onChanged?.(projectId);
    return status;
  }

  private async syncTranscriptIn(db: Db, client: ApiClient, projectId: number, conversationId: number) {
    const outcome = await syncScope(db, messagesSpec(client, projectId, conversationId), { now: this.deps.now });
    if (outcome.status === 'ok' && outcome.complete) {
      // Only a finished transcript is clean; one cut short by the page cap
      // (or a cursor that did not move) continues on the next run.
      await db.run('UPDATE conversations SET messages_dirty = 0 WHERE project_id = ? AND id = ?', [projectId, conversationId]);
    } else if (outcome.status === 'forbidden' || outcome.status === 'not_found') {
      await db.transaction((tx) => deleteConversation(tx, projectId, conversationId));
    }
    return outcome.status;
  }

  private async drop(db: Db, projectId: number): Promise<void> {
    log.info('project access lost; dropping its offline data', { projectId });
    await db.transaction((tx) => dropProject(tx, projectId));
  }
}
