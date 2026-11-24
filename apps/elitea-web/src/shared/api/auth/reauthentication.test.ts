import { afterEach, describe, expect, it, vi } from 'vitest';

import { createHttpClient } from '../http';
import { EliteaApiError } from '../generated/mutator';
import {
  reauthenticationUrl,
  recentSignInRequest,
  saveReauthenticationDraft,
  takeReauthenticationDraft,
} from './reauthentication';

const STALE_BODY = { error: { type: 'authentication_error', code: 'reauthentication_required', message: 'sign in again' } };

function stale(headers: Record<string, string>): Response {
  return new Response(JSON.stringify(STALE_BODY), {
    status: 401,
    headers: { 'content-type': 'application/json', ...headers },
  });
}

afterEach(() => {
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
});

describe('the 401 reauthentication_required answer', () => {
  it('is surfaced with its headers and never opens the re-auth popup', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>().mockResolvedValue(
      stale({ 'X-Elitea-Reauthentication-Max-Age': '600', 'X-Elitea-Reauthentication-Method': 'saml' }),
    ));
    const reauthenticate = vi.fn().mockResolvedValue(undefined);
    const result = await createHttpClient({ baseUrl: '/api/v2', reauthenticate }).post('/auth/token/', { body: {} });

    expect(reauthenticate).not.toHaveBeenCalled();
    expect(result.ok).toBe(false);
    if (result.ok) return;
    expect(recentSignInRequest(new EliteaApiError(result.error))).toEqual({ maxAge: 600, method: 'saml' });
  });

  it('carries no method when the session has no browser sign-in', async () => {
    vi.stubGlobal('fetch', vi.fn<typeof fetch>().mockResolvedValue(stale({ 'X-Elitea-Reauthentication-Max-Age': 'x' })));
    const result = await createHttpClient({ baseUrl: '/api/v2' }).post('/auth/token/', { body: {} });

    if (result.ok) throw new Error('expected a failure');
    expect(recentSignInRequest(new EliteaApiError(result.error))).toEqual({ maxAge: null, method: null });
  });

  it('is not another failure', () => {
    expect(recentSignInRequest(new Error('boom'))).toBeNull();
    expect(recentSignInRequest(new EliteaApiError({ kind: 'http', status: 403, url: '/x', body: STALE_BODY }))).toBeNull();
    expect(recentSignInRequest(new EliteaApiError({ kind: 'http', status: 401, url: '/x', body: STALE_BODY }))).toBeNull();
  });
});

describe('the sign-in start', () => {
  it('is the login of the method, with the limit and the page to return to', () => {
    expect(reauthenticationUrl({ maxAge: 900, method: 'oidc' }, '/settings/x?a=1')).toBe(
      '/auth/oidc/login?max_age=900&target_to=%2Fsettings%2Fx%3Fa%3D1',
    );
    expect(reauthenticationUrl({ maxAge: null, method: 'saml' }, 'settings')).toBe(
      '/auth/saml/login?max_age=0&target_to=%2Fsettings',
    );
    expect(reauthenticationUrl({ maxAge: 900, method: 'form' }, '/p')).toBe('/auth/form/reauthenticate?target_to=%2Fp');
    expect(reauthenticationUrl({ maxAge: 900, method: null }, '/p')).toBeNull();
  });
});

describe('the draft', () => {
  it('is read once', () => {
    saveReauthenticationDraft('k', { name: 'a' });
    expect(takeReauthenticationDraft('k')).toEqual({ name: 'a' });
    expect(takeReauthenticationDraft('k')).toBeUndefined();
  });

  it('is discarded when the sign-in was abandoned long ago', () => {
    const savedAt = Date.UTC(2026, 10, 24, 8, 0);
    saveReauthenticationDraft('k', { name: 'a' }, savedAt);
    expect(takeReauthenticationDraft('k', savedAt + 11 * 60 * 1000)).toBeUndefined();
    // …and is gone, not kept for a later read.
    expect(takeReauthenticationDraft('k', savedAt)).toBeUndefined();
    saveReauthenticationDraft('k', { name: 'b' }, savedAt);
    expect(takeReauthenticationDraft('k', savedAt + 9 * 60 * 1000)).toEqual({ name: 'b' });
  });
});
