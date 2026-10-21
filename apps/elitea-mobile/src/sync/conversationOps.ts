import type { ActionOutcome, ConversationAction } from '@/api/queries/conversationActions';
import { deleteConversation } from '@/db/repo';
import type { Db, SqlExecutor } from '@/db/sql';
import { log } from '@/lib/log';

/**
 * The Chats list's action queue (issue #6): rename, pin/unpin and delete made on this
 * device, kept in the encrypted store (`conversation_ops`, migration 3) until the
 * workspace took them, so they survive going offline, a restart and the lock.
 *
 * - One pending action per conversation and kind: a second rename replaces the first, an
 *   unpin replaces a pending pin (the last word wins). A delete drops the conversation's
 *   pending rename and pin (they would only 404).
 * - The list shows the queued state at once (`overlayOf`), over whatever the network or
 *   the saved copy says, until the action is sent.
 * - Drain (on reconnect, foreground, the store opening, and right after an action): each
 *   due action is sent once. Taken (`done`, or `gone` for a delete of a conversation that
 *   no longer exists): removed, and the saved copy is brought in line at once (the delta
 *   follows). `gone` for a rename or pin (deleted elsewhere, or access lost): the server
 *   took nothing, so the conversation itself is forgotten, never shown with that value.
 *   Not answered or a 5xx/429: kept, retried with backoff (`Retry-After` wins). Refused
 *   (403, 400, ...): dropped and reported, so the list goes back to what the server says.
 * - Offline (`isOnline` false), nothing is attempted: the reconnect drains. Likewise while
 *   the update screen holds the workspace (`isBlocked`): no send, no attempt counted.
 *
 * Everything lives in the store, so it is wiped with it (sign-out, revoke, server switch,
 * offline storage turned off) and dropped with its project (403).
 */

export type OpKind = ConversationAction['kind'];

export interface ConversationOp {
  projectId: number;
  conversationId: number;
  action: ConversationAction;
  attempts: number;
  nextAttemptMs: number;
  createdMs: number;
}

interface OpRow {
  project_id: number;
  conversation_id: number;
  kind: string;
  value: string | null;
  attempts: number;
  next_attempt_ms: number;
  created_ms: number;
}

function toAction(kind: string, value: string | null): ConversationAction | null {
  if (kind === 'rename' && typeof value === 'string' && value !== '') return { kind: 'rename', name: value };
  if (kind === 'pin') return { kind: 'pin', pinned: value === '1' };
  if (kind === 'delete') return { kind: 'delete' };
  return null;
}

function valueOf(action: ConversationAction): string | null {
  if (action.kind === 'rename') return action.name;
  if (action.kind === 'pin') return action.pinned ? '1' : '0';
  return null;
}

function toOp(row: OpRow): ConversationOp | null {
  const action = toAction(row.kind, row.value);
  if (action === null) return null;
  return {
    projectId: row.project_id,
    conversationId: row.conversation_id,
    action,
    attempts: row.attempts,
    nextAttemptMs: row.next_attempt_ms,
    createdMs: row.created_ms,
  };
}

/** Queues an action (replacing a pending one of the same kind on the same conversation). */
export async function enqueueOp(db: Db, projectId: number, conversationId: number, action: ConversationAction, now: number = Date.now()): Promise<void> {
  await db.transaction(async (tx) => {
    if (action.kind === 'delete') {
      await tx.run('DELETE FROM conversation_ops WHERE project_id = ? AND conversation_id = ?', [projectId, conversationId]);
    } else {
      // A conversation already queued for deletion is not renamed or pinned.
      const doomed = await tx.get<{ n: number }>(
        "SELECT count(*) AS n FROM conversation_ops WHERE project_id = ? AND conversation_id = ? AND kind = 'delete'",
        [projectId, conversationId],
      );
      if ((doomed?.n ?? 0) > 0) return;
    }
    await tx.run(
      `INSERT INTO conversation_ops (project_id, conversation_id, kind, value, attempts, next_attempt_ms, created_ms)
       VALUES (?, ?, ?, ?, 0, 0, ?)
       ON CONFLICT (project_id, conversation_id, kind) DO UPDATE SET
         value = excluded.value, attempts = 0, next_attempt_ms = 0, created_ms = excluded.created_ms`,
      [projectId, conversationId, action.kind, valueOf(action), now],
    );
  });
}

/** Pending actions, oldest first (all projects when `projectId` is null). */
export async function listOps(db: Db, projectId: number | null = null): Promise<ConversationOp[]> {
  const rows = await db.all<OpRow>(
    projectId === null
      ? 'SELECT * FROM conversation_ops ORDER BY created_ms ASC, rowid ASC'
      : 'SELECT * FROM conversation_ops WHERE project_id = ? ORDER BY created_ms ASC, rowid ASC',
    projectId === null ? [] : [projectId],
  );
  return rows.map(toOp).filter((op): op is ConversationOp => op !== null);
}

export async function removeOp(db: Db, op: Pick<ConversationOp, 'projectId' | 'conversationId' | 'action'>): Promise<void> {
  await db.run('DELETE FROM conversation_ops WHERE project_id = ? AND conversation_id = ? AND kind = ?', [op.projectId, op.conversationId, op.action.kind]);
}

/** What the list shows for a conversation while actions on it are queued. */
export interface ConversationOverlay {
  name?: string;
  pinned?: boolean;
  deleted?: boolean;
}

/** The queued state per conversation id. */
export function overlayOf(ops: readonly Pick<ConversationOp, 'conversationId' | 'action'>[]): Map<number, ConversationOverlay> {
  const map = new Map<number, ConversationOverlay>();
  for (const op of ops) {
    const entry = map.get(op.conversationId) ?? {};
    if (op.action.kind === 'rename') entry.name = op.action.name;
    else if (op.action.kind === 'pin') entry.pinned = op.action.pinned;
    else entry.deleted = true;
    map.set(op.conversationId, entry);
  }
  return map;
}

/** Applies queued state to list rows: deleted rows leave, renamed and (un)pinned rows change. */
export function applyOverlay<T extends { id: number; name?: string | null; is_pinned?: boolean }>(
  rows: readonly T[],
  overlay: ReadonlyMap<number, ConversationOverlay>,
): T[] {
  if (overlay.size === 0) return [...rows];
  const out: T[] = [];
  for (const row of rows) {
    const entry = overlay.get(row.id);
    if (!entry) {
      out.push(row);
      continue;
    }
    if (entry.deleted) continue;
    out.push({
      ...row,
      ...(entry.name !== undefined ? { name: entry.name } : {}),
      ...(entry.pinned !== undefined ? { is_pinned: entry.pinned } : {}),
    });
  }
  return out;
}

/**
 * Brings the saved copy in line with an action the workspace took, so the offline list
 * shows it before the delta re-delivers the row (and a delete is gone at once).
 */
export async function applyTakenOp(tx: SqlExecutor, op: Pick<ConversationOp, 'projectId' | 'conversationId' | 'action'>): Promise<void> {
  const where = 'project_id = ? AND id = ?';
  const key = [op.projectId, op.conversationId];
  switch (op.action.kind) {
    case 'delete': {
      // Its unsent messages go too: sent to a conversation that is gone, they would only
      // fail and stay behind with no thread left to show or remove them. The outbox keys a
      // send by the thread's route ref (the id or the uuid) and by the uuid.
      const saved = await tx.get<{ uuid: string | null }>(`SELECT uuid FROM conversations WHERE ${where}`, key);
      const uuid = saved?.uuid ?? null;
      await tx.run(
        `DELETE FROM outbox WHERE project_id = ? AND (conversation_ref = ?${uuid === null ? '' : ' OR conversation_ref = ? OR conversation_uuid = ?'})`,
        uuid === null ? [op.projectId, String(op.conversationId)] : [op.projectId, String(op.conversationId), uuid, uuid],
      );
      await deleteConversation(tx, op.projectId, op.conversationId);
      return;
    }
    case 'rename':
      await tx.run(
        `UPDATE conversations SET name = ?,
           row_json = CASE WHEN row_json IS NOT NULL AND json_valid(row_json) THEN json_set(row_json, '$.name', ?) ELSE row_json END,
           detail_json = CASE WHEN detail_json IS NOT NULL AND json_valid(detail_json) THEN json_set(detail_json, '$.name', ?) ELSE detail_json END
         WHERE ${where}`,
        [op.action.name, op.action.name, op.action.name, ...key],
      );
      return;
    case 'pin':
      await tx.run(
        `UPDATE conversations SET
           row_json = CASE WHEN row_json IS NOT NULL AND json_valid(row_json) THEN json_set(row_json, '$.is_pinned', json(?)) ELSE row_json END
         WHERE ${where}`,
        [op.action.pinned ? 'true' : 'false', ...key],
      );
  }
}

/** Backoff after `attempts` failed sends: 2 s doubling to 10 min; `Retry-After` wins (1 s to 15 min). */
export function opBackoffMs(attempts: number, retryAfterMs?: number): number {
  if (retryAfterMs !== undefined) return Math.min(Math.max(retryAfterMs, 1000), 15 * 60_000);
  return Math.min(2000 * 2 ** Math.max(0, attempts - 1), 10 * 60_000);
}

export interface OpsDrainerDeps {
  getDb: () => Db | null;
  send: (op: ConversationOp) => Promise<ActionOutcome>;
  isOnline?: () => boolean;
  /**
   * True while the workspace demands an app update (426, the update screen): nothing is
   * sent, and a send that got the 426 is held (no attempt, no backoff), so the queue goes
   * out as soon as the block clears instead of waiting out a backoff it never earned.
   */
  isBlocked?: () => boolean;
  now?: () => number;
  /** Arms a timer for the next due retry; returns its cancel. */
  schedule?: (delayMs: number, wake: () => void) => () => void;
  /** After an action left the queue: taken (`done`/`gone`) or refused (`rejected`). */
  onSettled?: (op: ConversationOp, outcome: 'done' | 'gone' | 'rejected') => void;
}

export class ConversationOpsDrainer {
  private running: Promise<void> | null = null;
  private again = false;
  private cancelWake: (() => void) | null = null;
  private readonly listeners = new Set<() => void | Promise<void>>();

  constructor(private readonly deps: OpsDrainerDeps) {}

  /** Told whenever the queue changed (queued, sent, refused); a listener may return its reload. */
  subscribe(listener: () => void | Promise<void>): () => void {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  /** Tells every listener; resolves once each one's reload (if it returned one) finished. */
  notify(): Promise<void> {
    const reloads: Promise<void>[] = [];
    for (const listener of [...this.listeners]) {
      try {
        const reload = listener();
        if (reload instanceof Promise) reloads.push(reload.catch((error: unknown) => log.warn('conversation ops listener failed', { error })));
      } catch (error) {
        log.warn('conversation ops listener failed', { error });
      }
    }
    return Promise.all(reloads).then(() => undefined);
  }

  /** Sends every due action once; concurrent calls share the pass (and one more follows). */
  drain(): Promise<void> {
    if (this.running) {
      this.again = true;
      return this.running;
    }
    this.running = (async () => {
      do {
        this.again = false;
        await this.pass();
      } while (this.again);
    })().finally(() => {
      this.running = null;
    });
    return this.running;
  }

  private async pass(): Promise<void> {
    const db = this.deps.getDb();
    if (db === null) return;
    if (this.deps.isOnline && !this.deps.isOnline()) return;
    const blocked = () => this.deps.isBlocked?.() === true;
    if (blocked()) return;
    const now = this.deps.now ?? Date.now;
    let changed = false;
    let nextWake = Infinity;
    for (const op of await listOps(db)) {
      if (this.deps.getDb() !== db) return;
      if (op.nextAttemptMs > now()) {
        nextWake = Math.min(nextWake, op.nextAttemptMs);
        continue;
      }
      const result = await this.deps.send(op);
      // The queue may have moved on meanwhile (a newer action of this kind, the store closed).
      if (this.deps.getDb() !== db) return;
      const current = (await listOps(db, op.projectId)).find((o) => o.conversationId === op.conversationId && o.action.kind === op.action.kind);
      const superseded = current === undefined || JSON.stringify(current.action) !== JSON.stringify(op.action) || current.createdMs !== op.createdMs;
      if (result.outcome === 'retry') {
        // Held by the update screen: kept as it is; the block clearing drains again.
        if (blocked()) break;
        if (superseded) continue;
        const attempts = op.attempts + 1;
        const at = now() + opBackoffMs(attempts, result.retryAfterMs);
        await db.run(
          'UPDATE conversation_ops SET attempts = ?, next_attempt_ms = ? WHERE project_id = ? AND conversation_id = ? AND kind = ?',
          [attempts, at, op.projectId, op.conversationId, op.action.kind],
        );
        nextWake = Math.min(nextWake, at);
        continue;
      }
      if (result.outcome === 'rejected') {
        log.warn('conversation action refused', { kind: op.action.kind, status: result.status });
        if (!superseded) await removeOp(db, op);
      } else if (result.outcome === 'gone' && op.action.kind !== 'delete') {
        // A rename or pin answered 404: the conversation was deleted elsewhere or the caller
        // lost access to it. The server took nothing, so the value is not saved; the
        // conversation (and whatever else is queued for it) is forgotten instead.
        await db.transaction(async (tx) => {
          await tx.run('DELETE FROM conversation_ops WHERE project_id = ? AND conversation_id = ?', [op.projectId, op.conversationId]);
          await applyTakenOp(tx, { projectId: op.projectId, conversationId: op.conversationId, action: { kind: 'delete' } });
        });
      } else {
        await db.transaction(async (tx) => {
          if (!superseded) {
            await tx.run('DELETE FROM conversation_ops WHERE project_id = ? AND conversation_id = ? AND kind = ?', [op.projectId, op.conversationId, op.action.kind]);
          }
          // Taken by the server: the saved copy follows, unless a newer action replaced it.
          if (!superseded || op.action.kind === 'delete') await applyTakenOp(tx, op);
        });
      }
      changed = true;
      this.deps.onSettled?.(op, result.outcome);
    }
    if (changed) void this.notify();
    this.cancelWake?.();
    this.cancelWake = null;
    if (nextWake !== Infinity && this.deps.schedule) {
      this.cancelWake = this.deps.schedule(Math.max(0, nextWake - now()), () => {
        this.cancelWake = null;
        this.drain().catch((error: unknown) => log.warn('conversation ops drain failed', { error }));
      });
    }
  }
}
