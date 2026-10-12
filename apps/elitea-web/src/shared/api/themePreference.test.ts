/**
 * The shared theme preference transport: a read answers the mode, `null` for
 * "never chosen" and `undefined` for "could not tell"; a save never throws,
 * and two saves land in the order they were made.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  bindThemeUser,
  configureThemeSync,
  fetchThemePreference,
  isThemeMode,
  MAX_SAVE_ATTEMPTS,
  retryUnsyncedTheme,
  saveThemePreference,
  themeSyncState,
  unsyncedThemeChoice,
} from './themePreference';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/theme`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  configureThemeSync({ retryBaseMs: 1 });
  bindThemeUser('u1');
});

afterEach(() => {
  resetGeneratedClient();
  localStorage.clear();
});

describe('isThemeMode', () => {
  it('accepts exactly the three modes', () => {
    expect(['system', 'light', 'dark'].every(isThemeMode)).toBe(true);
    expect([undefined, null, '', 'Dark', 'auto', 1].some(isThemeMode)).toBe(false);
  });
});

describe('fetchThemePreference', () => {
  it('answers the stored mode', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ theme_mode: 'dark' })));
    await expect(fetchThemePreference()).resolves.toBe('dark');
  });

  it('answers null when the user never chose one', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ theme_mode: null })));
    await expect(fetchThemePreference()).resolves.toBeNull();
  });

  it('answers undefined for a failed read or an unknown value, never throws', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ error: 'no' }, { status: 500 })));
    await expect(fetchThemePreference()).resolves.toBeUndefined();
    server.use(http.get(URL, () => HttpResponse.json({ theme_mode: 'sepia' })));
    await expect(fetchThemePreference()).resolves.toBeUndefined();
    server.use(http.get(URL, () => HttpResponse.json({ error: 'unauthorized' }, { status: 401 })));
    await expect(fetchThemePreference()).resolves.toBeUndefined();
  });
});

describe('saveThemePreference', () => {
  it('PUTs the mode and resolves true', async () => {
    const bodies: unknown[] = [];
    server.use(http.put(URL, async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json({ theme_mode: 'light' });
    }));
    await expect(saveThemePreference('light')).resolves.toBe(true);
    expect(bodies).toEqual([{ theme_mode: 'light' }]);
  });

  it('resolves false on a refused save instead of throwing', async () => {
    server.use(http.put(URL, () => HttpResponse.json({ error: 'no' }, { status: 500 })));
    await expect(saveThemePreference('dark')).resolves.toBe(false);
  });

  it('sends quick successive saves in order', async () => {
    const order: string[] = [];
    let first = true;
    server.use(http.put(URL, async ({ request }) => {
      const { theme_mode: mode } = (await request.json()) as { theme_mode: string };
      // The first request is the slow one: unchained, the second would land first.
      if (first) {
        first = false;
        await new Promise((resolve) => setTimeout(resolve, 30));
      }
      order.push(mode);
      return HttpResponse.json({ theme_mode: mode });
    }));
    await Promise.all([saveThemePreference('dark'), saveThemePreference('light')]);
    expect(order).toEqual(['dark', 'light']);
  });

  /** Serves PUTs from `statuses` in turn (then 200), recording each mode. */
  function servePuts(statuses: number[]) {
    const modes: string[] = [];
    server.use(http.put(URL, async ({ request }) => {
      const { theme_mode: mode } = (await request.json()) as { theme_mode: string };
      modes.push(mode);
      const status = statuses.shift() ?? 200;
      return status === 200 ? HttpResponse.json({ theme_mode: mode }) : HttpResponse.json({ error: 'no' }, { status });
    }));
    return modes;
  }

  it('keeps a choice unsynced until its PUT is answered, retrying 5xx', async () => {
    const modes = servePuts([500, 503]);
    const saving = saveThemePreference('light');
    expect(unsyncedThemeChoice()).toBe('light');
    expect(themeSyncState()).toMatchObject({ saving: true, busy: true });
    await expect(saving).resolves.toBe(true);
    expect(modes).toEqual(['light', 'light', 'light']);
    expect(unsyncedThemeChoice()).toBeUndefined();
    expect(themeSyncState()).toMatchObject({ saving: false, busy: false });
  });

  it('gives up after a few transient failures and keeps the mark for the next load', async () => {
    const modes = servePuts(Array.from({ length: 20 }, () => 500));
    await expect(saveThemePreference('light')).resolves.toBe(false);
    expect(modes).toHaveLength(MAX_SAVE_ATTEMPTS);
    expect(unsyncedThemeChoice()).toBe('light');
    // This page does not start over on every read.
    await expect(retryUnsyncedTheme()).resolves.toBe('skipped');
    expect(modes).toHaveLength(MAX_SAVE_ATTEMPTS);
  });

  it('drops the mark at once on a 4xx, which no retry would change', async () => {
    const modes = servePuts([403]);
    await expect(saveThemePreference('light')).resolves.toBe(false);
    expect(modes).toEqual(['light']);
    expect(unsyncedThemeChoice()).toBeUndefined();
    // A 429 is not permanent: retried.
    const again = servePuts([429]);
    await expect(saveThemePreference('dark')).resolves.toBe(true);
    expect(again).toEqual(['dark', 'dark']);
  });

  it('does not let an older success clear a newer unsynced choice', async () => {
    const releases: Record<string, () => void> = {};
    server.use(http.put(URL, async ({ request }) => {
      const { theme_mode: mode } = (await request.json()) as { theme_mode: string };
      await new Promise<void>((resolve) => (releases[mode] = resolve));
      return HttpResponse.json({ theme_mode: mode });
    }));
    const older = saveThemePreference('dark');
    const newer = saveThemePreference('light');
    await vi.waitFor(() => expect(releases.dark).toBeDefined());
    releases.dark?.();
    await expect(older).resolves.toBe(true);
    expect(unsyncedThemeChoice()).toBe('light');
    await vi.waitFor(() => expect(releases.light).toBeDefined());
    releases.light?.();
    await expect(newer).resolves.toBe(true);
    expect(unsyncedThemeChoice()).toBeUndefined();
  });

  it('clears only its own mark: another tab\'s newer write survives this one\'s success', async () => {
    let release: (() => void) | undefined;
    server.use(http.put(URL, async ({ request }) => {
      await new Promise<void>((resolve) => (release = resolve));
      return HttpResponse.json(await request.json());
    }));
    const saving = saveThemePreference('light');
    await new Promise((resolve) => setTimeout(resolve, 10));
    // Another tab of the same user chose dark meanwhile.
    localStorage.setItem('el.theme.unsynced', JSON.stringify({ user_id: 'u1', mode: 'dark', id: 'other-tab' }));
    release?.();
    await expect(saving).resolves.toBe(true);
    expect(unsyncedThemeChoice()).toBe('dark');
  });

  it('reads another user\'s mark as no mark', () => {
    localStorage.setItem('el.theme.unsynced', JSON.stringify({ user_id: 'u2', mode: 'dark', id: 'x' }));
    expect(unsyncedThemeChoice()).toBeUndefined();
    bindThemeUser('u2');
    expect(unsyncedThemeChoice()).toBe('dark');
    bindThemeUser(undefined);
    expect(unsyncedThemeChoice()).toBeUndefined();
  });

  it('keeps the mark in memory when storage refuses the write', async () => {
    const setItem = vi.spyOn(localStorage, 'setItem').mockImplementation(() => {
      throw new DOMException('quota', 'QuotaExceededError');
    });
    servePuts(Array.from({ length: 20 }, () => 500));
    await saveThemePreference('light');
    setItem.mockRestore();
    expect(localStorage.getItem('el.theme.unsynced')).toBeNull();
    expect(unsyncedThemeChoice()).toBe('light');
  });
});
