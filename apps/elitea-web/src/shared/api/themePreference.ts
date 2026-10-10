/**
 * The caller's colour-theme preference, stored server-side so the web app and
 * the desktop app — two origins, two separate `localStorage`s — show the same
 * theme for the same user (`GET`/`PUT /social/author/theme`,
 * services/elitea-main/internal/api/v2/social/theme.go).
 *
 * `localStorage['el-mode']` stays the first-paint cache: the static
 * `scheme-init` script and MUI read it before any request can answer, and the
 * server value is applied on top once it arrives (`app/ThemePreferenceSync`).
 *
 * Every function here swallows its failure. A theme that did not sync is not
 * worth a toast, and both calls are `background` requests: a 401 on them must
 * not open the re-auth window — the session probe owns that decision.
 *
 * A choice the server has not confirmed is UNSYNCED: it is kept as a marker
 * `{user_id, mode, id}` (`el.theme.unsynced`, so it survives a reload and
 * goes with the logout sweep; in memory when storage refuses writes) from
 * the moment it is made until its PUT is answered. While it is set for the
 * signed-in user, the server's value is stale by definition, so a read must
 * not apply it (`app/ThemePreferenceSync` retries the PUT instead). A save
 * clears only its own marker (by `id`: another tab may have written a newer
 * one), on success or on a 4xx, which is permanent — the server's value then
 * applies again. Network errors, 408, 429 and 5xx are retried with backoff,
 * at most {@link MAX_SAVE_ATTEMPTS} times; the marker then stays for the next
 * page load to try again.
 *
 * Calls `eliteaFetch` directly rather than the generated
 * `getCurrentAuthorTheme`/`updateCurrentAuthorTheme`, because those cannot
 * pass the `background` transport flag. Loaded only through a dynamic
 * `import()`, so none of this is in the initial chunk.
 */
import { createStorage } from '@/shared/lib/storage';

import { EliteaApiError, eliteaFetch } from './generated/mutator';
import { getGetCurrentAuthorThemeUrl } from './generated/social/social';

export type ThemeMode = 'system' | 'light' | 'dark';

export function isThemeMode(value: unknown): value is ThemeMode {
  return value === 'system' || value === 'light' || value === 'dark';
}

interface ThemePreferenceBody {
  theme_mode?: unknown;
}

/**
 * The stored mode, `null` when the user has never chosen one, or `undefined`
 * when the read failed (the caller then leaves the local choice alone).
 */
export async function fetchThemePreference(): Promise<ThemeMode | null | undefined> {
  try {
    const { data } = await eliteaFetch<{ data: ThemePreferenceBody }>(
      getGetCurrentAuthorThemeUrl(),
      { method: 'GET' },
      { background: true },
    );
    if (data.theme_mode === null) return null;
    return isThemeMode(data.theme_mode) ? data.theme_mode : undefined;
  } catch {
    return undefined;
  }
}

const UNSYNCED_KEY = 'theme.unsynced';

/** One save's tries (the first and the retries) on a transient failure. */
export const MAX_SAVE_ATTEMPTS = 5;

let retryBaseMs = 1000;

/** Test hook: the first retry's delay (it doubles on each), and a clean slate for the page state below. */
export function configureThemeSync(options: { retryBaseMs: number }): void {
  retryBaseMs = options.retryBaseMs;
  boundUser = undefined;
  memoryMarker = undefined;
  storageFailed = false;
  exhausted = false;
}

interface UnsyncedMarker {
  user_id: string;
  mode: ThemeMode;
  /** The write's own id: a save clears the marker only while it is still its own. */
  id: string;
}

function isMarker(raw: unknown): UnsyncedMarker | undefined {
  if (typeof raw !== 'object' || raw === null) return undefined;
  const value = raw as Record<string, unknown>;
  return typeof value.user_id === 'string' && isThemeMode(value.mode) && typeof value.id === 'string'
    ? { user_id: value.user_id, mode: value.mode, id: value.id }
    : undefined;
}

/** The signed-in user the marker is written for and read as (`app/ThemePreferenceSync` binds it). */
let boundUser: string | undefined;

export function bindThemeUser(userId: string | undefined): void {
  boundUser = userId;
}

/**
 * Storage can refuse (a private window, blocked site data, a full quota).
 * After a failed write the in-memory marker is the truth for this page.
 */
let memoryMarker: UnsyncedMarker | undefined;
let storageFailed = false;

function readMarker(): UnsyncedMarker | undefined {
  if (storageFailed) return memoryMarker;
  try {
    return createStorage('local').getJSON(UNSYNCED_KEY, isMarker) ?? undefined;
  } catch {
    storageFailed = true;
    return memoryMarker;
  }
}

function writeMarker(marker: UnsyncedMarker | undefined): void {
  memoryMarker = marker;
  if (storageFailed) return;
  try {
    const storage = createStorage('local');
    if (marker === undefined) storage.remove(UNSYNCED_KEY);
    else storage.setJSON(UNSYNCED_KEY, marker);
  } catch {
    storageFailed = true;
  }
}

function clearMarkerIfMine(id: string): void {
  if (readMarker()?.id === id) writeMarker(undefined);
  else if (memoryMarker?.id === id) memoryMarker = undefined;
}

function randomId(): string {
  return typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `${String(Date.now())}-${String(Math.random()).slice(2)}`;
}

/** The signed-in user's local choice no PUT has confirmed yet, if any (another user's marker is not theirs). */
export function unsyncedThemeChoice(): ThemeMode | undefined {
  const marker = readMarker();
  return marker !== undefined && boundUser !== undefined && marker.user_id === boundUser ? marker.mode : undefined;
}

/** Bumped by every save, so a read can tell a choice was made while it was out. */
let generation = 0;
/** Saves not finished yet (their PUTs and retries). */
let inFlight = 0;
/** The last save gave up on transient failures: no more retries on this page. */
let exhausted = false;

export interface ThemeSyncState {
  /** Changes whenever a save starts. */
  generation: number;
  /** A save is out. */
  saving: boolean;
  /** A local choice is not confirmed by the server: a read's answer is stale. */
  busy: boolean;
}

export function themeSyncState(): ThemeSyncState {
  return { generation, saving: inFlight > 0, busy: inFlight > 0 || unsyncedThemeChoice() !== undefined };
}

type PutOutcome = 'saved' | 'refused' | 'failed';

/** A 4xx other than 408 / 429 will not change on a retry. */
function isPermanent(error: unknown): boolean {
  if (!(error instanceof EliteaApiError)) return false;
  const { failure } = error;
  return (
    (failure.kind === 'http' || failure.kind === 'auth') &&
    failure.status >= 400 &&
    failure.status < 500 &&
    failure.status !== 408 &&
    failure.status !== 429
  );
}

/**
 * PUTs are chained, so two quick clicks reach the server in the order they
 * were made and the last one is what is stored.
 */
let pendingSave: Promise<unknown> = Promise.resolve();

function enqueuePut(mode: ThemeMode): Promise<PutOutcome> {
  const put = pendingSave.then(async (): Promise<PutOutcome> => {
    try {
      await eliteaFetch(
        getGetCurrentAuthorThemeUrl(),
        { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ theme_mode: mode }) },
        { background: true },
      );
      return 'saved';
    } catch (error) {
      return isPermanent(error) ? 'refused' : 'failed';
    }
  });
  pendingSave = put;
  return put;
}

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Stores `mode`; resolves `true` once the server has it, `false` otherwise.
 * Marks it unsynced at once (for the bound user). Its own success, or a
 * permanent refusal, clears its own mark; a transient failure is retried
 * with backoff unless a later choice took over.
 */
export async function saveThemePreference(mode: ThemeMode): Promise<boolean> {
  generation += 1;
  const mine = generation;
  const id = randomId();
  exhausted = false;
  if (boundUser !== undefined) writeMarker({ user_id: boundUser, mode, id });
  inFlight += 1;
  try {
    for (let attempt = 1; ; attempt += 1) {
      const outcome = await enqueuePut(mode);
      if (outcome !== 'failed') {
        clearMarkerIfMine(id);
        return outcome === 'saved';
      }
      if (generation !== mine) return false;
      if (attempt >= MAX_SAVE_ATTEMPTS) {
        exhausted = true;
        return false;
      }
      await sleep(retryBaseMs * 2 ** (attempt - 1));
      // Checked right before the next PUT is queued: a newer choice's PUT
      // must stay the last one sent.
      if (generation !== mine) return false;
    }
  } finally {
    inFlight -= 1;
  }
}

/**
 * Push the signed-in user's unconfirmed choice again: `skipped` when there
 * is none, a save is already out, or this page gave up on it; `refused`
 * when the server will not take it (the marker is gone; its value applies).
 */
export async function retryUnsyncedTheme(): Promise<'none' | 'skipped' | 'saved' | 'refused' | 'failed'> {
  const mode = unsyncedThemeChoice();
  if (mode === undefined) return 'none';
  if (inFlight > 0 || exhausted) return 'skipped';
  if (await saveThemePreference(mode)) return 'saved';
  return unsyncedThemeChoice() === undefined ? 'refused' : 'failed';
}
