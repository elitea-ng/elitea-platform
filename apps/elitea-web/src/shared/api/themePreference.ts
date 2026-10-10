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
 * (`el.theme.unsynced`, so it survives a reload and goes with the logout
 * sweep) from the moment it is made until a PUT of it succeeds. While it is
 * set, the server's value is stale by definition, so a read must not apply
 * it (`app/ThemePreferenceSync` retries the PUT instead).
 *
 * Calls `eliteaFetch` directly rather than the generated
 * `getCurrentAuthorTheme`/`updateCurrentAuthorTheme`, because those cannot
 * pass the `background` transport flag. Loaded only through a dynamic
 * `import()`, so none of this is in the initial chunk.
 */
import { createStorage } from '@/shared/lib/storage';

import { eliteaFetch } from './generated/mutator';
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

/** Storage can be unavailable (a private window, blocked site data): then the marker lives as long as the page. */
let unsyncedInMemory: ThemeMode | undefined;

function writeUnsynced(mode: ThemeMode | undefined): void {
  unsyncedInMemory = mode;
  try {
    const storage = createStorage('local');
    if (mode === undefined) storage.remove(UNSYNCED_KEY);
    else storage.set(UNSYNCED_KEY, mode);
  } catch {
    // The in-memory marker stands.
  }
}

/** The local choice no PUT has confirmed yet, if any. */
export function unsyncedThemeChoice(): ThemeMode | undefined {
  try {
    const stored = createStorage('local').get(UNSYNCED_KEY);
    return isThemeMode(stored) ? stored : undefined;
  } catch {
    return unsyncedInMemory;
  }
}

/** Bumped by every save, so a read can tell a choice was made while it was out. */
let generation = 0;
/** PUTs sent and not answered yet. */
let inFlight = 0;

export interface ThemeSyncState {
  /** Changes whenever a save starts. */
  generation: number;
  /** A PUT is out. */
  saving: boolean;
  /** A local choice is not confirmed by the server: a read's answer is stale. */
  busy: boolean;
}

export function themeSyncState(): ThemeSyncState {
  return { generation, saving: inFlight > 0, busy: inFlight > 0 || unsyncedThemeChoice() !== undefined };
}

/**
 * Saves are chained, so two quick clicks reach the server in the order they
 * were made and the last one is what is stored.
 */
let pendingSave: Promise<unknown> = Promise.resolve();

/**
 * Stores `mode`; resolves `true` once the server has it, `false` otherwise.
 * Marks it unsynced at once; only its own success clears the mark, and only
 * when no later choice was made meanwhile.
 */
export function saveThemePreference(mode: ThemeMode): Promise<boolean> {
  generation += 1;
  const mine = generation;
  inFlight += 1;
  writeUnsynced(mode);
  const save = pendingSave.then(async () => {
    try {
      await eliteaFetch(
        getGetCurrentAuthorThemeUrl(),
        { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ theme_mode: mode }) },
        { background: true },
      );
      if (generation === mine) writeUnsynced(undefined);
      return true;
    } catch {
      return false;
    } finally {
      inFlight -= 1;
    }
  });
  pendingSave = save;
  return save;
}
