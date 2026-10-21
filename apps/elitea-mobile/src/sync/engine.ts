import { isApiError } from '@/api/errors';
import type { Db, SqlExecutor } from '@/db/sql';
import { log } from '@/lib/log';
import { DeltaShapeError, type DeltaPage, type Tombstone } from '@/sync/deltaApi';

/**
 * One list's incremental sync (ADR-0025 WP6) against its stored cursor.
 *
 * - Pages: `changes_since=<cursor>` (`0` the first time), applied in ONE
 *   transaction with the cursor that follows them, so a crash can never
 *   store a cursor past rows that were not written (or the other way round:
 *   a rolled-back page is simply fetched again). While `has_more`, the next
 *   page follows at once.
 * - Upsert by id: rows inside the server's 5 s settle window come back on the
 *   next call; writing them again is harmless.
 * - Tombstones (`deleted`, `access_lost`, or any reason we do not know) are
 *   removals. They are applied before the page's rows: a row in the page is
 *   the item as it is now.
 * - Full resync (first sync, 410 `sync_cursor_expired`, 400
 *   `invalid_sync_cursor`): starts from `0` under a new generation; rows the
 *   full pass did not return are swept when its last page lands. The old rows
 *   stay readable until then, so an interrupted resync loses nothing.
 * - 403: the caller lost the whole project (not a tombstone) → `forbidden`,
 *   and the caller drops the project's data. 404: the list is gone.
 * - No answer (offline, timeout, aborted): `offline`; the cursor is kept.
 */

export interface ScopeSpec<Row> {
  scope: string;
  projectId: number;
  fetch(cursor: string): Promise<DeltaPage<Row>>;
  /** Writes one page (tombstones, then rows) at generation `gen`. */
  apply(tx: SqlExecutor, page: DeltaPage<Row>, gen: number): Promise<void>;
  /** Deletes this list's rows written before generation `gen` (end of a full resync). */
  sweep(tx: SqlExecutor, gen: number): Promise<void>;
}

export type SyncOutcome =
  | {
      status: 'ok';
      pages: number;
      rows: number;
      tombstones: number;
      resynced: boolean;
      /** The last page said `has_more: false`: the list is caught up (not stopped by the page cap or a stalled cursor). */
      complete: boolean;
    }
  | { status: 'forbidden' }
  | { status: 'not_found' }
  | { status: 'offline' }
  | { status: 'error'; error: unknown };

/** Pages per run; a list that needs more continues on the next run from its cursor. */
export const MAX_PAGES_PER_RUN = 50;

interface StateRow {
  cursor: string | null;
  gen: number;
  full_gen: number | null;
}

export const FULL_SYNC_CURSOR = '0';

async function readState(db: Db, scope: string, projectId: number): Promise<StateRow> {
  const row = await db.get<StateRow>('SELECT cursor, gen, full_gen FROM sync_state WHERE scope = ?', [scope]);
  if (row !== null) return row;
  await db.run('INSERT INTO sync_state (scope, project_id, cursor, gen, full_gen) VALUES (?, ?, NULL, 0, NULL)', [scope, projectId]);
  return { cursor: null, gen: 0, full_gen: null };
}

/** Restarts a list from `0` under a new generation (old rows swept when the pass completes). */
async function startFullResync(db: Db, scope: string, state: StateRow): Promise<StateRow> {
  const next = { cursor: FULL_SYNC_CURSOR, gen: state.gen, full_gen: Math.max(state.gen, state.full_gen ?? 0) + 1 };
  await db.run('UPDATE sync_state SET cursor = ?, full_gen = ? WHERE scope = ?', [next.cursor, next.full_gen, scope]);
  return next;
}

function httpStatus(error: unknown): number | undefined {
  return isApiError(error) && error.kind === 'http' ? error.status : undefined;
}

function isNoAnswer(error: unknown): boolean {
  return isApiError(error) && (error.kind === 'network' || error.kind === 'timeout' || error.kind === 'aborted');
}

export async function syncScope<Row>(
  db: Db,
  spec: ScopeSpec<Row>,
  options: { now?: () => number; maxPages?: number } = {},
): Promise<SyncOutcome> {
  const now = options.now ?? Date.now;
  const maxPages = options.maxPages ?? MAX_PAGES_PER_RUN;
  let state = await readState(db, spec.scope, spec.projectId);
  if (state.cursor === null) state = await startFullResync(db, spec.scope, state);
  let pages = 0;
  let rows = 0;
  let tombstones = 0;
  let resets = 0;
  let resynced = false;
  let complete = false;

  while (pages < maxPages) {
    const cursor = state.cursor ?? FULL_SYNC_CURSOR;
    let page: DeltaPage<Row>;
    try {
      page = await spec.fetch(cursor);
    } catch (error) {
      const status = httpStatus(error);
      const code = isApiError(error) ? error.code : undefined;
      if (status === 410 || (status === 400 && code === 'invalid_sync_cursor')) {
        // The cursor is unusable (expired past tombstone retention, or not
        // this list's): discard it and resync this list from the start.
        if (resets++ >= 1 || cursor === FULL_SYNC_CURSOR) return { status: 'error', error };
        log.info('sync cursor refused; full resync', { scope: spec.scope, status });
        state = await startFullResync(db, spec.scope, state);
        resynced = true;
        continue;
      }
      if (status === 403) return { status: 'forbidden' };
      if (status === 404) return { status: 'not_found' };
      if (isNoAnswer(error)) return { status: 'offline' };
      if (!(error instanceof DeltaShapeError)) log.warn('sync page failed', { scope: spec.scope, status });
      return { status: 'error', error };
    }

    const finishing = !page.hasMore && state.full_gen !== null;
    const writeGen = state.full_gen ?? state.gen;
    const next: StateRow = finishing
      ? { cursor: page.nextCursor, gen: state.full_gen as number, full_gen: null }
      : { ...state, cursor: page.nextCursor };
    await db.transaction(async (tx) => {
      await spec.apply(tx, page, writeGen);
      if (finishing) await spec.sweep(tx, state.full_gen as number);
      await tx.run('UPDATE sync_state SET cursor = ?, gen = ?, full_gen = ?, synced_ms = ? WHERE scope = ?', [
        next.cursor,
        next.gen,
        next.full_gen,
        now(),
        spec.scope,
      ]);
    });
    pages++;
    rows += page.rows.length;
    tombstones += page.tombstones.length;
    const stalled = page.nextCursor === cursor;
    complete = !page.hasMore;
    state = next;
    if (!page.hasMore || stalled) break;
  }
  return { status: 'ok', pages, rows, tombstones, resynced, complete };
}

/** Tombstone ids, for `apply` implementations. */
export function tombstoneIds(tombstones: readonly Tombstone[]): number[] {
  return tombstones.map((t) => t.id);
}
