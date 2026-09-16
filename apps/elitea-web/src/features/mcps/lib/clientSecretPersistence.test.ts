/**
 * Issue #177, half 1: an OAuth `client_secret` must not be persisted in
 * browser storage AT ALL.
 *
 * `storage.ts` used to write it into `el.mcp.credentials` and into every
 * `el.mcp.tokens` record. A client secret does not expire and authenticates
 * the application rather than one session, so a copy on disk is worth more to
 * an attacker with script execution than the access token beside it.
 *
 * These assertions are written against the RAW storage areas, not against
 * `getSavedCredentials()`/`getTokenInfo()`. Both accessors still return the
 * secret while the document lives (`clientSecretVault.ts` holds it), so an
 * accessor-level assertion would pass whether the value reached disk or not.
 * The sweep over every key of BOTH areas is also the guard against the
 * failure this repo has already shipped once: moving a secret out of one
 * storage key and into another one is not a fix.
 */
import { afterEach, describe, expect, it } from 'vitest';

import { getSavedCredentials, getTokenInfo, logout, removeSavedCredentials, setAccessToken, setSavedCredentials } from './storage';

const SERVER = 'https://mcp-secret.example.com';
const SECRET = 'super-secret-oauth-client-secret';

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

/** Every key/value pair currently held by either web-storage area. */
function dumpAllStorage(): string {
  const lines: string[] = [];
  for (const area of [window.localStorage, window.sessionStorage]) {
    for (let index = 0; index < area.length; index++) {
      const key = area.key(index);
      if (key === null) continue;
      lines.push(`${key}=${area.getItem(key) ?? ''}`);
    }
  }
  return lines.join('\n');
}

describe('client_secret never reaches browser storage', () => {
  it('retains a Main reference across refresh without retaining a document secret', () => {
    setAccessToken(SERVER, 'old', 3600, undefined, undefined, 'refresh', { client_id: 'legacy', client_secret: SECRET });
    setAccessToken(SERVER, 'new', 3600, undefined, undefined, 'refresh', { client_id: 'registered', used_dcr: true, client_reference: 'main-reference' });
    setAccessToken(SERVER, 'refreshed', 3600, undefined, undefined, 'rotated-refresh');

    expect(getTokenInfo(SERVER)).toMatchObject({ client_id: 'registered', client_reference: 'main-reference', used_dcr: true });
    expect(getTokenInfo(SERVER)?.client_secret).toBeUndefined();
    expect(dumpAllStorage()).not.toContain(SECRET);
  });

  it.each([
    { client_id: 'different-public-client', used_dcr: true },
    { client_id: 'registered', used_dcr: false },
  ])('does not inherit a reference when grant ownership changes: %j', (metadata) => {
    setAccessToken(SERVER, 'old', 3600, undefined, undefined, 'refresh', { client_id: 'registered', used_dcr: true, client_reference: 'old-reference' });
    setAccessToken(SERVER, 'new', 3600, undefined, undefined, 'new-refresh', metadata);

    expect(getTokenInfo(SERVER)?.client_reference).toBeUndefined();
    expect(getTokenInfo(SERVER)?.client_secret).toBeUndefined();
    expect(dumpAllStorage()).not.toContain('old-reference');
  });

  it('setSavedCredentials writes the client_id and nothing else about the secret', () => {
    setSavedCredentials({ serverUrl: SERVER, clientId: 'public-client-id', clientSecret: SECRET });

    const raw = window.sessionStorage.getItem('el.mcp.credentials') ?? '';
    expect(raw).toContain('public-client-id'); // precondition: the record really was written
    expect(raw).not.toContain(SECRET);
    expect(raw).not.toContain('client_secret');
    expect(dumpAllStorage()).not.toContain(SECRET);
  });

  it('setAccessToken persists the token record without its client_secret', () => {
    setAccessToken(SERVER, 'the-access-token', 3600, undefined, undefined, 'the-refresh-token', {
      token_endpoint: 'https://as.example.com/token',
      client_id: 'dcr-issued-client',
      client_secret: SECRET,
      used_dcr: true,
    });

    const raw = window.sessionStorage.getItem('el.mcp.tokens') ?? '';
    expect(raw).toContain('the-access-token'); // precondition
    expect(raw).toContain('dcr-issued-client'); // the rest of the record survives
    expect(raw).not.toContain(SECRET);
    expect(raw).not.toContain('client_secret');
    expect(dumpAllStorage()).not.toContain(SECRET);
  });

  it('keeps the secret usable for the life of THIS document, so an in-flight refresh still works', () => {
    setSavedCredentials({ serverUrl: SERVER, clientId: 'cid', clientSecret: SECRET });
    setAccessToken(SERVER, 'tok', 3600, undefined, undefined, 'refresh', { client_secret: SECRET, client_id: 'cid' });

    expect(getSavedCredentials(SERVER)?.client_secret).toBe(SECRET);
    expect(getTokenInfo(SERVER)?.client_secret).toBe(SECRET);
  });

  it('carries the held secret forward when a refresh re-writes the token without one', () => {
    setAccessToken(SERVER, 'tok', 3600, undefined, undefined, 'refresh', { client_secret: SECRET });
    setAccessToken(SERVER, 'tok-2', 3600, undefined, undefined, 'refresh');

    expect(getTokenInfo(SERVER)?.access_token).toBe('tok-2');
    expect(getTokenInfo(SERVER)?.client_secret).toBe(SECRET);
  });

  it('forgets the held secret when that server is signed out or its credentials are removed', () => {
    setSavedCredentials({ serverUrl: SERVER, clientId: 'cid', clientSecret: SECRET });
    setAccessToken(SERVER, 'tok', 3600, undefined, undefined, 'refresh', { client_secret: SECRET });

    logout(SERVER);
    removeSavedCredentials(SERVER);

    setAccessToken(SERVER, 'tok-3', 3600, undefined, undefined, 'refresh');
    expect(getTokenInfo(SERVER)?.client_secret).toBeUndefined();
    expect(getSavedCredentials(SERVER)).toBeNull();
  });
});

describe('after a reload the secret is gone, because nothing on disk holds it', () => {
  /**
   * A fresh module registry is what a reload gives the page: the storage areas
   * survive, every module-scope value does not. If the secret came back here,
   * it came back from disk — which is the whole defect.
   */
  it('recovers the token and the client_id, and cannot recover the client_secret', async () => {
    setSavedCredentials({ serverUrl: SERVER, clientId: 'cid', clientSecret: SECRET });
    setAccessToken(SERVER, 'tok', 3600, undefined, undefined, 'refresh', { client_id: 'cid', client_secret: SECRET });

    const { vi } = await import('vitest');
    vi.resetModules();
    const reloaded = await import('./storage');

    expect(reloaded.getTokenInfo(SERVER)?.access_token).toBe('tok');
    expect(reloaded.getSavedCredentials(SERVER)?.client_id).toBe('cid');
    expect(reloaded.getTokenInfo(SERVER)?.client_secret).toBeUndefined();
    expect(reloaded.getSavedCredentials(SERVER)?.client_secret).toBeUndefined();
  });
});
