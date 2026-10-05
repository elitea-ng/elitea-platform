import { isApiError } from '@/api/errors';
import {
  TurnStartError,
  restorePreparedTurn,
  startTurn,
  type PreparedTurn,
  type StartedTurn,
} from '@/api/queries/sendMessage';
import type { Db } from '@/db/sql';
import { log } from '@/lib/log';

/**
 * The offline outbox (plan-app B4.6, ADR-0025 WP6).
 *
 * A message sent while the device is offline is stored with its client
 * `question_id` (lowercase canonical UUID) and the EXACT body bytes, and shown
 * in the thread as queued. The drain (on reconnect, foreground, store open,
 * or "Retry") POSTs those bytes unchanged. The server treats `question_id` as
 * the admission idempotency key: the same id with the same body replays the
 * original admission (200, `created: false`), so a send whose answer was lost
 * can be retried any number of times without starting a second turn.
 *
 * Outcomes per item:
 * - admitted (200, new or replayed): removed; the thread refetches and the
 *   answer arrives through the transcript;
 * - no answer, 5xx, 429, 503, a retryable 409: stays queued with backoff
 *   (`Retry-After` wins), and everything queued after it waits too (strict
 *   order, so messages are admitted in the order they were written);
 * - 409 "conflicts with an existing turn": the server holds this question_id
 *   with a different body. Never resent; shown as failed (the user can send
 *   the text again as a NEW message, with a new question_id, or delete it);
 * - another refusal (400, 403, 422 unsupported agent, a bad answer): failed;
 * - signed out / ending / upgrade required: left queued for later, with no
 *   timed wake (the store opening, reconnect, foreground or the update screen
 *   clearing drains again).
 *
 * The outbox lives in the encrypted store, so it is wiped with it (logout,
 * revocation, server switch, offline storage turned off by policy).
 */

export type OutboxState = 'queued' | 'sending' | 'failed';
export type OutboxFailure = 'idempotency_conflict' | 'conflict' | 'unsupported_agent' | 'rejected' | 'bad_response' | 'invalid';

export interface OutboxItem {
  questionId: string;
  projectId: number;
  conversationRef: string;
  conversationUuid: string;
  bodyJson: string;
  userText: string;
  state: OutboxState;
  attempts: number;
  nextAttemptMs: number;
  lastError: OutboxFailure | null;
  createdMs: number;
}

interface OutboxRow {
  question_id: string;
  project_id: number;
  conversation_ref: string;
  conversation_uuid: string;
  body_json: string;
  user_text: string;
  state: string;
  attempts: number;
  next_attempt_ms: number;
  last_error: string | null;
  created_ms: number;
}

const toItem = (row: OutboxRow): OutboxItem => ({
  questionId: row.question_id,
  projectId: row.project_id,
  conversationRef: row.conversation_ref,
  conversationUuid: row.conversation_uuid,
  bodyJson: row.body_json,
  userText: row.user_text,
  state: (['queued', 'sending', 'failed'].includes(row.state) ? row.state : 'queued') as OutboxState,
  attempts: row.attempts,
  nextAttemptMs: row.next_attempt_ms,
  lastError: (row.last_error as OutboxFailure | null) ?? null,
  createdMs: row.created_ms,
});

const FAILURE_TEXT: Record<OutboxFailure, string> = {
  idempotency_conflict: 'Not sent: the workspace already has a different message with this id.',
  conflict: 'Not sent: the conversation changed meanwhile.',
  unsupported_agent: 'Not sent: this agent needs the web app.',
  rejected: 'Not sent: the workspace refused this message.',
  bad_response: 'Not sent: the workspace answered in a way this app cannot follow.',
  invalid: 'Not sent: this message could not be read back.',
};

/** What the thread shows under an unsent message. */
export function outboxStatus(item: Pick<OutboxItem, 'state' | 'lastError' | 'attempts'>): { text: string; error: boolean } {
  switch (item.state) {
    case 'sending':
      return { text: 'Sending…', error: false };
    case 'failed':
      return { text: FAILURE_TEXT[item.lastError ?? 'rejected'] ?? FAILURE_TEXT.rejected, error: true };
    default:
      return {
        text: item.attempts > 0 ? 'Queued. Retrying when the workspace can be reached.' : "Queued. Sends when you're back online.",
        error: false,
      };
  }
}

/**
 * Stores a prepared send. The same question_id twice is the same item.
 * `failedAttempts` counts tries already made elsewhere (a live send the
 * workspace did not answer): the item starts in that many attempts' backoff,
 * or waits `retryAfterMs` when the server's `Retry-After` said so.
 */
export async function enqueue(
  db: Db,
  prepared: PreparedTurn,
  conversationRef: string,
  now: number = Date.now(),
  options: { failedAttempts?: number; retryAfterMs?: number } = {},
): Promise<void> {
  const { request, bodyJson } = prepared;
  const attempts = Math.max(0, options.failedAttempts ?? 0);
  const nextAttemptMs = attempts > 0 ? now + outboxBackoffMs(attempts, options.retryAfterMs) : 0;
  await db.run(
    `INSERT INTO outbox (question_id, project_id, conversation_ref, conversation_uuid, body_json, user_text, state, attempts, next_attempt_ms, created_ms)
     VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)
     ON CONFLICT (question_id) DO NOTHING`,
    [request.questionId, request.projectId, conversationRef, request.conversationUuid, bodyJson, request.userInput, attempts, nextAttemptMs, now],
  );
}

export async function listOutbox(db: Db, projectId: number, conversationUuid: string): Promise<OutboxItem[]> {
  const rows = await db.all<OutboxRow>(
    'SELECT * FROM outbox WHERE project_id = ? AND conversation_uuid = ? ORDER BY created_ms ASC',
    [projectId, conversationUuid],
  );
  return rows.map(toItem);
}

/** True when the conversation has a message the outbox has not sent yet (queued or sending). */
export async function hasUnsent(db: Db, projectId: number, conversationUuid: string): Promise<boolean> {
  const row = await db.get<{ n: number }>(
    "SELECT 1 AS n FROM outbox WHERE project_id = ? AND conversation_uuid = ? AND state IN ('queued', 'sending') LIMIT 1",
    [projectId, conversationUuid],
  );
  return row !== null;
}

export async function removeFromOutbox(db: Db, questionId: string): Promise<void> {
  await db.run('DELETE FROM outbox WHERE question_id = ?', [questionId]);
}

/** Backoff after `attempts` failed tries: 2 s, 4 s, … capped at 5 min; `Retry-After` wins. */
export function outboxBackoffMs(attempts: number, retryAfterMs?: number): number {
  if (retryAfterMs !== undefined) return Math.min(Math.max(retryAfterMs, 1000), 15 * 60_000);
  return Math.min(2000 * 2 ** Math.max(0, attempts - 1), 5 * 60_000);
}

type Classified =
  | { kind: 'sent'; started: StartedTurn }
  | { kind: 'retry'; retryAfterMs?: number }
  | { kind: 'failed'; reason: OutboxFailure }
  /** Not this item's fault (signed out, upgrade required): leave it, stop draining. */
  | { kind: 'hold' };

function classify(error: unknown): Exclude<Classified, { kind: 'sent' }> {
  if (error instanceof TurnStartError) {
    const failure = error.failure;
    switch (failure.kind) {
      case 'transient':
        return { kind: 'retry', retryAfterMs: failure.retryAfterMs };
      case 'conflict':
        return failure.retryable ? { kind: 'retry', retryAfterMs: failure.retryAfterMs } : { kind: 'failed', reason: 'conflict' };
      case 'idempotency_conflict':
        return { kind: 'failed', reason: 'idempotency_conflict' };
      case 'unsupported_agent':
        return { kind: 'failed', reason: 'unsupported_agent' };
      case 'bad_response':
        return { kind: 'failed', reason: 'bad_response' };
      case 'rejected':
        return { kind: 'failed', reason: 'rejected' };
    }
  }
  if (isApiError(error)) {
    if (error.kind === 'network' || error.kind === 'timeout' || error.kind === 'aborted') return { kind: 'retry' };
    return { kind: 'hold' };
  }
  return { kind: 'retry' };
}

export interface OutboxDrainerDeps {
  getDb: () => Db | null;
  start?: (prepared: PreparedTurn) => Promise<StartedTurn>;
  now?: () => number;
  /** An item was admitted (the thread refetches, the transcript syncs). */
  onSent?: (item: OutboxItem, started: StartedTurn) => void;
  /** Stored items changed (screens re-read). */
  onChanged?: () => void;
  /** Wakes the drain again when the earliest backoff ends. */
  schedule?: (delayMs: number, wake: () => void) => () => void;
}

export interface DrainResult {
  sent: number;
  failed: number;
  /** Items left waiting (backoff, offline, held). */
  waiting: number;
}

export class OutboxDrainer {
  private running: Promise<DrainResult> | null = null;
  private again = false;
  private cancelWake: (() => void) | null = null;
  private readonly listeners = new Set<() => void>();

  constructor(private readonly deps: OutboxDrainerDeps) {}

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  /** Re-read by screens after any change. */
  notify(): void {
    this.deps.onChanged?.();
    for (const listener of [...this.listeners]) listener();
  }

  /** Sends every due item in order; concurrent calls share one run (one more follows). */
  drain(): Promise<DrainResult> {
    if (this.running) {
      this.again = true;
      return this.running;
    }
    this.running = (async () => {
      let result: DrainResult;
      do {
        this.again = false;
        result = await this.drainOnce();
      } while (this.again);
      return result;
    })().finally(() => {
      this.running = null;
    });
    return this.running;
  }

  /** "Retry" on a queued item: due now. */
  async retryNow(questionId: string): Promise<void> {
    const db = this.deps.getDb();
    if (!db) return;
    await db.run("UPDATE outbox SET next_attempt_ms = 0 WHERE question_id = ? AND state = 'queued'", [questionId]);
    this.notify();
    void this.drain();
  }

  async remove(questionId: string): Promise<void> {
    const db = this.deps.getDb();
    if (!db) return;
    await removeFromOutbox(db, questionId);
    this.notify();
  }

  private async drainOnce(): Promise<DrainResult> {
    const result: DrainResult = { sent: 0, failed: 0, waiting: 0 };
    const db = this.deps.getDb();
    if (!db) return result;
    const now = this.deps.now ?? Date.now;
    const start = this.deps.start ?? ((prepared: PreparedTurn) => startTurn(prepared));
    // A crash mid-send leaves 'sending': resending the same bytes is safe.
    const rows = await db.all<OutboxRow>("SELECT * FROM outbox WHERE state IN ('queued', 'sending') ORDER BY created_ms ASC");
    let stop = false;
    // A hold is not the item's fault and has no backoff of its own: no wake is
    // scheduled after it (that wake would be due at once and resend in a tight
    // loop while, e.g., a 426 holds). Store open, reconnect, foreground and the
    // update screen clearing drain again.
    let held = false;
    for (const item of rows.map(toItem)) {
      // Strict order: an item in backoff holds back everything queued after
      // it, so messages are admitted in the order they were written.
      if (stop || item.nextAttemptMs > now()) {
        stop = true;
        result.waiting++;
        continue;
      }
      let prepared: PreparedTurn;
      try {
        prepared = restorePreparedTurn(item.bodyJson);
      } catch {
        await this.fail(db, item, 'invalid');
        result.failed++;
        continue;
      }
      // The rows are a snapshot from the start of this pass: claim the item
      // only if it is still there and unsent (the user may have deleted it
      // while an earlier item was in flight); otherwise skip it.
      const claim = await db.run(
        "UPDATE outbox SET state = 'sending' WHERE question_id = ? AND state IN ('queued', 'sending')",
        [item.questionId],
      );
      if (claim.changes === 0) continue;
      this.notify();
      let outcome: Classified;
      try {
        outcome = { kind: 'sent', started: await start(prepared) };
      } catch (error) {
        outcome = classify(error);
      }
      switch (outcome.kind) {
        case 'sent':
          await removeFromOutbox(db, item.questionId);
          result.sent++;
          this.deps.onSent?.(item, outcome.started);
          break;
        case 'failed':
          await this.fail(db, item, outcome.reason);
          result.failed++;
          break;
        case 'retry': {
          const attempts = item.attempts + 1;
          await db.run("UPDATE outbox SET state = 'queued', attempts = ?, next_attempt_ms = ? WHERE question_id = ?", [
            attempts,
            now() + outboxBackoffMs(attempts, outcome.retryAfterMs),
            item.questionId,
          ]);
          result.waiting++;
          // The workspace is unreachable or busy: later items wait too (and keep their order).
          stop = true;
          break;
        }
        case 'hold':
          await db.run("UPDATE outbox SET state = 'queued' WHERE question_id = ?", [item.questionId]);
          result.waiting++;
          stop = true;
          held = true;
          break;
      }
      this.notify();
    }
    if (held) this.cancelScheduledWake();
    else await this.scheduleWake(db);
    if (result.sent + result.failed > 0) log.info('outbox drained', { ...result });
    return result;
  }

  private async fail(db: Db, item: OutboxItem, reason: OutboxFailure): Promise<void> {
    await db.run("UPDATE outbox SET state = 'failed', last_error = ? WHERE question_id = ?", [reason, item.questionId]);
    this.notify();
  }

  /** Wakes the drain when the head of the queue is due again. */
  private cancelScheduledWake(): void {
    this.cancelWake?.();
    this.cancelWake = null;
  }

  private async scheduleWake(db: Db): Promise<void> {
    this.cancelScheduledWake();
    if (!this.deps.schedule) return;
    const next = await db.get<{ at: number }>(
      "SELECT next_attempt_ms AS at FROM outbox WHERE state = 'queued' ORDER BY created_ms ASC LIMIT 1",
    );
    if (next === null) return;
    const delay = Math.max(0, next.at - (this.deps.now ?? Date.now)());
    this.cancelWake = this.deps.schedule(delay, () => void this.drain());
  }
}

