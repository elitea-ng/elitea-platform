/**
 * The server-stored theme reaches MUI's mode: the server wins over the
 * `el-mode` cache, a never-stored server value is seeded from a local choice,
 * a toggle made during the read is not undone, and focus reads again.
 */
import { act, render, waitFor } from '@testing-library/react';
import { useColorScheme } from '@mui/material/styles';
import { delay, http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { BrandThemeProvider } from '@/app/providers/BrandThemeProvider';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { clearNamespace } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import { saveThemePreference, unsyncedThemeChoice } from '@/shared/api/themePreference';

import ThemePreferenceSync, { REFRESH_INTERVAL_MS } from './ThemePreferenceSync';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/theme`;

let current: { mode: string | undefined; setMode: (mode: 'system' | 'light' | 'dark') => void } | undefined;
function Probe(): null {
  const { mode, setMode } = useColorScheme();
  current = { mode, setMode };
  return null;
}

function renderSync() {
  return render(
    <BrandThemeProvider>
      <ThemePreferenceSync />
      <Probe />
    </BrandThemeProvider>,
  );
}

/** Serves `answer()` to every read and records each read and each saved mode. */
function serveTheme(answer: () => Response | Promise<Response>) {
  const calls = { reads: 0, saves: [] as unknown[] };
  server.use(
    http.get(URL, () => {
      calls.reads += 1;
      return answer();
    }),
    http.put(URL, async ({ request }) => {
      const body = (await request.json()) as { theme_mode: unknown };
      calls.saves.push(body.theme_mode);
      return HttpResponse.json(body);
    }),
  );
  return calls;
}

/** Lets pending requests and their React updates settle. */
async function settle(): Promise<void> {
  await act(() => new Promise((resolve) => setTimeout(resolve, 20)));
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  clearNamespace();
  localStorage.clear();
  current = undefined;
});

describe('ThemePreferenceSync', () => {
  it('applies the server mode over the local cache, and caches it', async () => {
    localStorage.setItem('el-mode', 'light');
    const calls = serveTheme(() => HttpResponse.json({ theme_mode: 'system' }));
    renderSync();
    await waitFor(() => expect(current?.mode).toBe('system'));
    await waitFor(() => expect(localStorage.getItem('el-mode')).toBe('system'));
    await settle();
    expect(calls.saves).toEqual([]);
  });

  it('keeps the local mode when the read fails', async () => {
    localStorage.setItem('el-mode', 'light');
    const calls = serveTheme(() => HttpResponse.json({ error: 'no' }, { status: 500 }));
    renderSync();
    await waitFor(() => expect(calls.reads).toBe(1));
    await settle();
    expect(current?.mode).toBe('light');
    expect(calls.saves).toEqual([]);
  });

  it.each(['light', 'system'])('seeds a never-stored server value from a local %s', async (local) => {
    localStorage.setItem('el-mode', local);
    const calls = serveTheme(() => HttpResponse.json({ theme_mode: null }));
    renderSync();
    await waitFor(() => expect(calls.saves).toEqual([local]));
    expect(current?.mode).toBe(local);
  });

  it('does not seed the app default (dark)', async () => {
    const calls = serveTheme(() => HttpResponse.json({ theme_mode: null }));
    renderSync();
    await waitFor(() => expect(calls.reads).toBe(1));
    await settle();
    expect(calls.saves).toEqual([]);
  });

  it('does not undo a change the user made while the read was out', async () => {
    localStorage.setItem('el-mode', 'light');
    const calls = serveTheme(async () => {
      await delay(100);
      return HttpResponse.json({ theme_mode: 'dark' });
    });
    renderSync();
    await waitFor(() => expect(current?.mode).toBe('light'));
    await waitFor(() => expect(calls.reads).toBe(1));
    act(() => current?.setMode('system'));
    await act(() => new Promise((resolve) => setTimeout(resolve, 150)));
    expect(current?.mode).toBe('system');
  });

  it('reads again on focus, at most once per interval', async () => {
    let clock = 1_000_000;
    const now = vi.spyOn(Date, 'now').mockImplementation(() => clock);
    let stored = 'light';
    const calls = serveTheme(() => HttpResponse.json({ theme_mode: stored }));
    renderSync();
    await waitFor(() => expect(current?.mode).toBe('light'));
    expect(calls.reads).toBe(1);

    // Too soon: ignored.
    act(() => { window.dispatchEvent(new Event('focus')); });
    await settle();
    expect(calls.reads).toBe(1);

    // The other client changed it.
    clock += REFRESH_INTERVAL_MS;
    stored = 'dark';
    act(() => { window.dispatchEvent(new Event('focus')); });
    await waitFor(() => expect(current?.mode).toBe('dark'));
    expect(calls.reads).toBe(2);
    now.mockRestore();
  });

  /** What `ThemeModeToggle` does on a click. */
  function toggle(mode: 'system' | 'light' | 'dark'): void {
    act(() => {
      current?.setMode(mode);
      void saveThemePreference(mode);
    });
  }

  it('never lets a read overwrite a choice whose save failed; it retries the save', async () => {
    let clock = 1_000_000;
    const now = vi.spyOn(Date, 'now').mockImplementation(() => clock);
    let putFails = true;
    const calls = { reads: 0, saves: [] as unknown[] };
    server.use(
      http.get(URL, () => {
        calls.reads += 1;
        return HttpResponse.json({ theme_mode: 'dark' });
      }),
      http.put(URL, async ({ request }) => {
        const body = (await request.json()) as { theme_mode: unknown };
        calls.saves.push(body.theme_mode);
        return putFails ? HttpResponse.json({ error: 'no' }, { status: 500 }) : HttpResponse.json(body);
      }),
    );
    renderSync();
    await waitFor(() => expect(current?.mode).toBe('dark'));
    toggle('light');
    await waitFor(() => expect(calls.saves).toEqual(['light']));
    await settle();
    expect(unsyncedThemeChoice()).toBe('light');

    // The server still says dark: the read does not apply it, it pushes light again.
    clock += REFRESH_INTERVAL_MS;
    putFails = false;
    act(() => { window.dispatchEvent(new Event('focus')); });
    await waitFor(() => expect(calls.saves).toEqual(['light', 'light']));
    await settle();
    expect(calls.reads).toBe(1);
    expect(current?.mode).toBe('light');
    expect(unsyncedThemeChoice()).toBeUndefined();
    now.mockRestore();
  });

  it('discards a read that starts while a save is still out', async () => {
    let clock = 1_000_000;
    const now = vi.spyOn(Date, 'now').mockImplementation(() => clock);
    const calls = { reads: 0, saves: [] as unknown[] };
    server.use(
      // The read answers at once, before the server has the save.
      http.get(URL, () => {
        calls.reads += 1;
        return HttpResponse.json({ theme_mode: 'dark' });
      }),
      http.put(URL, async ({ request }) => {
        const body = (await request.json()) as { theme_mode: unknown };
        calls.saves.push(body.theme_mode);
        await delay(150);
        return HttpResponse.json(body);
      }),
    );
    renderSync();
    await waitFor(() => expect(current?.mode).toBe('dark'));
    toggle('light');
    clock += REFRESH_INTERVAL_MS;
    act(() => { window.dispatchEvent(new Event('focus')); });
    await act(() => new Promise((resolve) => setTimeout(resolve, 250)));
    expect(current?.mode).toBe('light');
    expect(calls.saves).toEqual(['light']);
    expect(unsyncedThemeChoice()).toBeUndefined();
    now.mockRestore();
  });
});
