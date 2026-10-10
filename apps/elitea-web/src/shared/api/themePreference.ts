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
 * Calls `eliteaFetch` directly rather than the generated
 * `getCurrentAuthorTheme`/`updateCurrentAuthorTheme`, because those cannot
 * pass the `background` transport flag. Loaded only through a dynamic
 * `import()`, so none of this is in the initial chunk.
 */
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

/**
 * Saves are chained, so two quick clicks reach the server in the order they
 * were made and the last one is what is stored.
 */
let pendingSave: Promise<unknown> = Promise.resolve();

/** Stores `mode`; resolves `true` once the server has it, `false` otherwise. */
export function saveThemePreference(mode: ThemeMode): Promise<boolean> {
  const save = pendingSave.then(async () => {
    try {
      await eliteaFetch(
        getGetCurrentAuthorThemeUrl(),
        { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ theme_mode: mode }) },
        { background: true },
      );
      return true;
    } catch {
      return false;
    }
  });
  pendingSave = save;
  return save;
}
