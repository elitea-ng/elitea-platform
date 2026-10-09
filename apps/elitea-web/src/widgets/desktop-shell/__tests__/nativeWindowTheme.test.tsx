/**
 * The native window (and its macOS sidebar material) follows the app's
 * palette mode: the bug was a light material behind a dark app.
 */
import { act, render, waitFor } from '@testing-library/react';
import { useColorScheme } from '@mui/material/styles';
import { afterEach, describe, expect, it } from 'vitest';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { createFakeAppIpc, MACOS_PLATFORM, type FakeAppIpc } from '@/shared/desktop/appEvents.fake';
import { clearNamespace } from '@/shared/lib/storage';

import { nativeWindowTheme, useNativeWindowTheme } from '../model/useNativeWindowTheme';

describe('nativeWindowTheme', () => {
  it('passes light and dark through and hands "system" (or no mode yet) back to the OS', () => {
    expect(nativeWindowTheme('light')).toBe('light');
    expect(nativeWindowTheme('dark')).toBe('dark');
    expect(nativeWindowTheme('system')).toBeNull();
    expect(nativeWindowTheme(undefined)).toBeNull();
  });
});

describe('useNativeWindowTheme', () => {
  afterEach(() => {
    clearNamespace();
    localStorage.clear();
  });

  let setMode: ((mode: 'light' | 'dark' | 'system') => void) | undefined;
  function Probe({ ipc }: { ipc: FakeAppIpc }): null {
    useNativeWindowTheme(ipc);
    setMode = useColorScheme().setMode;
    return null;
  }

  it('sets the window theme on mount and on every mode change, system included', async () => {
    const ipc = createFakeAppIpc(MACOS_PLATFORM);
    render(
      <BrandThemeProvider>
        <Probe ipc={ipc} />
      </BrandThemeProvider>,
    );
    await waitFor(() => expect(ipc.calls.themes.length).toBeGreaterThan(0));
    act(() => setMode?.('dark'));
    await waitFor(() => expect(ipc.calls.themes.at(-1)).toBe('dark'));
    act(() => setMode?.('light'));
    await waitFor(() => expect(ipc.calls.themes.at(-1)).toBe('light'));
    act(() => setMode?.('system'));
    await waitFor(() => expect(ipc.calls.themes.at(-1)).toBeNull());
    // One call per change, not per render.
    expect(ipc.calls.themes.slice(-3)).toEqual(['dark', 'light', null]);
  });

  it('does nothing without a host', () => {
    function Bare(): null {
      useNativeWindowTheme(undefined);
      return null;
    }
    expect(() =>
      render(
        <BrandThemeProvider>
          <Bare />
        </BrandThemeProvider>,
      ),
    ).not.toThrow();
  });
});
