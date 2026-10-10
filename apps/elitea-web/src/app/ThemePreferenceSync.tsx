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
 *
 * The desktop's native window theme needs nothing here: `DesktopFrame`'s
 * `useNativeWindowTheme` already follows the MUI mode this sets.
 *
 * Lazy-loaded by `App`, together with the API module, to keep both out of the
 * initial chunk.
 */
import { useEffect, useRef } from 'react';

import { useColorScheme } from '@mui/material/styles';

import { fetchThemePreference, isThemeMode, saveThemePreference } from '@/shared/api/themePreference';
import { DEFAULT_COLOR_SCHEME } from '@/shared/brand/constants';

/** One focus-triggered read per half minute at most: a window switch fires both focus and visibilitychange. */
export const REFRESH_INTERVAL_MS = 30_000;

export default function ThemePreferenceSync(): null {
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
    let disposed = false;
    let lastRead = Date.now();

    const read = async (): Promise<void> => {
      const changes = localChanges.current;
      const stored = await fetchThemePreference();
      // A failed read, an unmounted sync, or a toggle that moved while the
      // read was out: the local mode stands.
      if (disposed || stored === undefined || localChanges.current !== changes) return;
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
