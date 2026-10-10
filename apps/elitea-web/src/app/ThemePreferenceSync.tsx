/**
 * Applies the signed-in user's server-stored theme (`shared/api/themePreference`)
 * so the web app and the desktop app agree.
 *
 * - On mount — `App` mounts this once a session user is known, keyed on that
 *   user — the stored mode is read and, when it differs from the local one,
 *   applied with `setMode`. The server wins; `el-mode` was only the
 *   first-paint cache. MUI writes the applied mode back into `el-mode`, so
 *   the next load paints the right theme straight away.
 * - A user who has never chosen on the server, but has a local mode other
 *   than the app default (`DEFAULT_COLOR_SCHEME`, dark), has that choice
 *   seeded up, so the OTHER client picks it up. The default is not seeded: it
 *   is what the other client shows anyway, and storing it would turn "never
 *   chosen" into a choice.
 * - On window focus (at most once per {@link REFRESH_INTERVAL_MS}) it reads
 *   again, which is how a change made on the other client shows up here.
 * - A local change made while a read is in flight wins over that read.
 * - A local choice the server has not confirmed (`unsyncedThemeChoice`: its
 *   PUT failed or is still out) is never overwritten by a read: the read is
 *   skipped and the PUT retried instead, and a read that started while a
 *   choice was unconfirmed discards its answer. A PUT the server refuses for
 *   good (4xx) drops the choice's mark, and the server's value applies.
 *   The mark is the signed-in user's (`userId`), bound here.
 *
 * The desktop's native window theme needs nothing here: `DesktopFrame`'s
 * `useNativeWindowTheme` already follows the MUI mode this sets.
 *
 * Lazy-loaded by `App`, together with the API module, to keep both out of the
 * initial chunk.
 */
import { useEffect, useRef } from 'react';

import { useColorScheme } from '@mui/material/styles';

import {
  bindThemeUser,
  fetchThemePreference,
  isThemeMode,
  retryUnsyncedTheme,
  saveThemePreference,
  themeSyncState,
  type ThemeSyncState,
} from '@/shared/api/themePreference';
import { DEFAULT_COLOR_SCHEME } from '@/shared/brand/constants';

/** One focus-triggered read per half minute at most: a window switch fires both focus and visibilitychange. */
export const REFRESH_INTERVAL_MS = 30_000;

/** Whether a save was out when a read started, or one is out or was made since: the read's answer may be stale. */
function savedMeanwhile(before: ThemeSyncState): boolean {
  const after = themeSyncState();
  return before.busy || after.busy || after.generation !== before.generation;
}

export interface ThemePreferenceSyncProps {
  /** The signed-in user: an unconfirmed choice is applied and pushed only for them. */
  readonly userId: string;
}

export default function ThemePreferenceSync({ userId }: ThemePreferenceSyncProps): null {
  const { mode, setMode } = useColorScheme();
  const modeRef = useRef(mode);
  const setModeRef = useRef(setMode);
  /*
   * Counts mode changes made while mounted. MUI reports `mode: undefined`
   * until its own mount effect has run, so "the mode moved during the read"
   * cannot be a plain before/after comparison: undefined -> stored mode is
   * the first paint resolving, not a choice.
   */
  const localChanges = useRef(0);
  useEffect(() => {
    if (modeRef.current !== undefined && mode !== modeRef.current) localChanges.current += 1;
    modeRef.current = mode;
    setModeRef.current = setMode;
  }, [mode, setMode]);

  useEffect(() => {
    bindThemeUser(userId);
    return () => bindThemeUser(undefined);
  }, [userId]);

  useEffect(() => {
    let disposed = false;
    let lastRead = Date.now();

    const read = async (): Promise<void> => {
      // An unconfirmed local choice: the server's value is stale. Push the
      // choice again instead of reading — unless the server refused it for
      // good, and then its value applies.
      const retried = await retryUnsyncedTheme();
      if (retried !== 'none' && retried !== 'refused') return;
      const before = themeSyncState();
      const changes = localChanges.current;
      const stored = await fetchThemePreference();
      // A failed read, an unmounted sync, a toggle that moved while the read
      // was out, or a save that was out or made meanwhile: the local mode stands.
      if (disposed || stored === undefined || localChanges.current !== changes || savedMeanwhile(before)) return;
      const local = modeRef.current;
      if (stored === null) {
        if (isThemeMode(local) && local !== DEFAULT_COLOR_SCHEME) void saveThemePreference(local);
        return;
      }
      if (stored !== local) setModeRef.current(stored);
    };

    const onFocus = (): void => {
      if (document.visibilityState !== 'visible') return;
      const now = Date.now();
      if (now - lastRead < REFRESH_INTERVAL_MS) return;
      lastRead = now;
      void read();
    };

    void read();
    window.addEventListener('focus', onFocus);
    document.addEventListener('visibilitychange', onFocus);
    return () => {
      disposed = true;
      window.removeEventListener('focus', onFocus);
      document.removeEventListener('visibilitychange', onFocus);
    };
  }, []);

  return null;
}
