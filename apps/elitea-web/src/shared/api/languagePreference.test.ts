/**
 * The shared language preference transport and the boot rule: a read answers
 * the tag, `null` for "never chosen" and `undefined` for "could not tell"; a
 * save never throws and marks itself unsynced until the server confirms; and
 * `decideLanguageSync` lets the server win over the local cache.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { decideLanguageSync, fetchLanguagePreference, saveLanguagePreference, unsyncedLanguage } from './languagePreference';

const BASE = '/api/v2';
const URL = `${BASE}/social/author/language`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  localStorage.clear();
});

describe('fetchLanguagePreference', () => {
  it('answers the stored tag, null for never chosen, undefined for a failed read', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ language: 'pt-BR' })));
    await expect(fetchLanguagePreference()).resolves.toBe('pt-BR');
    server.use(http.get(URL, () => HttpResponse.json({ language: null })));
    await expect(fetchLanguagePreference()).resolves.toBeNull();
    server.use(http.get(URL, () => HttpResponse.json({ error: 'x' }, { status: 500 })));
    await expect(fetchLanguagePreference()).resolves.toBeUndefined();
  });
});

describe('saveLanguagePreference', () => {
  it('PUTs the tag and clears its unsynced mark once the server answers', async () => {
    const bodies: unknown[] = [];
    server.use(
      http.put(URL, async ({ request }) => {
        bodies.push(await request.json());
        expect(unsyncedLanguage()).toBe('de');
        return HttpResponse.json({ language: 'de' });
      }),
    );
    await expect(saveLanguagePreference('de')).resolves.toBe(true);
    expect(bodies).toEqual([{ language: 'de' }]);
    expect(unsyncedLanguage()).toBeUndefined();
  });

  it('keeps the mark on a transient failure and drops it on a permanent refusal', async () => {
    server.use(http.put(URL, () => HttpResponse.json({ error: 'x' }, { status: 503 })));
    await expect(saveLanguagePreference('de')).resolves.toBe(false);
    expect(unsyncedLanguage()).toBe('de');

    server.use(http.put(URL, () => HttpResponse.json({ error: 'x' }, { status: 400 })));
    await expect(saveLanguagePreference('de')).resolves.toBe(false);
    expect(unsyncedLanguage()).toBeUndefined();
  });
});

describe('decideLanguageSync', () => {
  it('lets the server value win over the local cache', () => {
    expect(decideLanguageSync({ server: 'de', current: 'en', stored: 'en', unsynced: undefined })).toEqual({
      kind: 'apply',
      locale: 'de',
    });
    // Also when the screen is only the system's detected language (nothing stored).
    expect(decideLanguageSync({ server: 'fr-CA', current: 'fr', stored: undefined, unsynced: undefined })).toEqual({
      kind: 'apply',
      locale: 'fr-CA',
    });
  });

  it('does nothing when the server agrees, has no value for this build, or the read failed', () => {
    expect(decideLanguageSync({ server: 'de', current: 'de', stored: 'de', unsynced: undefined })).toEqual({ kind: 'none' });
    expect(decideLanguageSync({ server: 'fil', current: 'en', stored: 'en', unsynced: undefined })).toEqual({ kind: 'none' });
    expect(decideLanguageSync({ server: undefined, current: 'en', stored: 'es', unsynced: undefined })).toEqual({ kind: 'none' });
  });

  it('seeds a never-chosen server value from a local choice, but not from the detected language', () => {
    expect(decideLanguageSync({ server: null, current: 'es', stored: 'es', unsynced: undefined })).toEqual({
      kind: 'push',
      locale: 'es',
    });
    expect(decideLanguageSync({ server: null, current: 'es', stored: undefined, unsynced: undefined })).toEqual({ kind: 'none' });
  });

  it('pushes an unconfirmed local choice instead of applying the stale server value', () => {
    expect(decideLanguageSync({ server: 'de', current: 'es', stored: 'es', unsynced: 'es' })).toEqual({
      kind: 'push',
      locale: 'es',
    });
  });
});
