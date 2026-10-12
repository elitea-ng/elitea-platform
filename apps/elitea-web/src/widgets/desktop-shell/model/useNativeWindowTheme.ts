/**
 * The native window follows the app's palette mode, not the OS's.
 *
 * On macOS the sidebar shows the window's system material (vibrancy), and
 * that material takes the WINDOW's appearance: with the app in dark mode
 * and the OS in light, the sidebar was a light grey panel under light text.
 * So the window's theme is set from the MUI mode on every change — `light`
 * or `dark` as chosen, and "system" as `null`, which hands the window back
 * to the OS appearance (the app's `system` mode follows the same OS setting).
 */
import { useEffect } from 'react';

import { useColorScheme } from '@mui/material/styles';

import type { AppIpc, NativeWindowTheme } from '@/shared/desktop/appEvents';

/** MUI's mode (`undefined` before it is read) to the window's theme. */
export function nativeWindowTheme(mode: string | undefined): NativeWindowTheme {
  return mode === 'light' || mode === 'dark' ? mode : null;
}

export function useNativeWindowTheme(ipc: AppIpc | undefined): void {
  const { mode } = useColorScheme();
  const theme = nativeWindowTheme(mode);
  useEffect(() => {
    if (ipc === undefined) return;
    // A refused call leaves the OS appearance; nothing else depends on it.
    ipc.setWindowTheme(theme).catch(() => undefined);
  }, [ipc, theme]);
}
