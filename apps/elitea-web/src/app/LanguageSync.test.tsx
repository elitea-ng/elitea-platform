/**
 * The server-stored language reaches the page at boot: the server wins over
 * the `el.lang` cache (save and reload), and a local choice the server never
 * had is seeded up.
 */
import { render, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { createStorage } from '@/shared/lib/storage';
import { server } from '@/test/setup';

import LanguageSync from './LanguageSync';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/language`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  localStorage.clear();
});

describe('LanguageSync', () => {
  it('applies the server language over the local cache and reloads', async () => {
    createStorage('local').set('lang', 'en');
    server.use(http.get(URL, () => HttpResponse.json({ language: 'de' })));
    const reload = vi.fn<() => void>();
    render(<LanguageSync reload={reload} />);
    await waitFor(() => expect(reload).toHaveBeenCalledOnce());
    expect(createStorage('local').get('lang')).toBe('de');
  });

  it('does not reload when the cache refuses the value (no reload loop)', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ language: 'de' })));
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new DOMException('blocked', 'SecurityError');
    });
    const reload = vi.fn<() => void>();
    try {
      render(<LanguageSync reload={reload} />);
      await waitFor(() => expect(setItem).toHaveBeenCalled());
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(reload).not.toHaveBeenCalled();
    } finally {
      setItem.mockRestore();
    }
  });

  it('leaves the page alone when the server already agrees', async () => {
    let reads = 0;
    server.use(
      http.get(URL, () => {
        reads += 1;
        return HttpResponse.json({ language: 'en' });
      }),
    );
    const reload = vi.fn<() => void>();
    render(<LanguageSync reload={reload} />);
    await waitFor(() => expect(reads).toBe(1));
    expect(reload).not.toHaveBeenCalled();
  });

  it('seeds a never-stored server value from the local choice', async () => {
    createStorage('local').set('lang', 'es');
    const saved: unknown[] = [];
    server.use(
      http.get(URL, () => HttpResponse.json({ language: null })),
      http.put(URL, async ({ request }) => {
        const body = (await request.json()) as { language: unknown };
        saved.push(body.language);
        return HttpResponse.json(body);
      }),
    );
    const reload = vi.fn<() => void>();
    render(<LanguageSync reload={reload} />);
    await waitFor(() => expect(saved).toEqual(['es']));
    expect(reload).not.toHaveBeenCalled();
  });

  it('keeps the local language when the read fails', async () => {
    createStorage('local').set('lang', 'es');
    server.use(http.get(URL, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    const reload = vi.fn<() => void>();
    render(<LanguageSync reload={reload} />);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(reload).not.toHaveBeenCalled();
    expect(createStorage('local').get('lang')).toBe('es');
  });
});
