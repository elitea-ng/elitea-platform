/**
 * The shared theme preference transport: a read answers the mode, `null` for
 * "never chosen" and `undefined` for "could not tell"; a save never throws,
 * and two saves land in the order they were made.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { fetchThemePreference, isThemeMode, saveThemePreference } from './themePreference';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/theme`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
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
});
