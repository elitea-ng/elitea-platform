import { afterEach, describe, expect, it, vi } from 'vitest';

import { createStorage } from '@/shared/lib/storage';

import { getLogoutMarkerStorageKey } from './logoutSync';

import {
  addIgnoredServer,
  canonicalizeServerUrl,
  clearIgnoredServers,
  getAccessToken,
  getFilteredIgnoredServers,
  getIgnoredServers,
  getRefreshToken,
  getSavedCredentials,
  getServersWithoutTokens,
  getSessionId,
  getStorageKey,
  getTokenInfo,
  isExpired,
  isPrebuildMcpType,
  isServerIgnored,
  logout,
  needsProactiveRefresh,
  needsRefresh,
  removeIgnoredServer,
  removeSavedCredentials,
  setAccessToken,
  setConnectionVerified,
  setSavedCredentials,
  setSessionId,
} from './storage';

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('isPrebuildMcpType', () => {
  it('true for mcp_-prefixed types', () => {
    expect(isPrebuildMcpType('mcp_github')).toBe(true);
    expect(isPrebuildMcpType('mcp_context7')).toBe(true);
  });

  it('false for the bare "mcp" type (remote MCP, not pre-built)', () => {
    expect(isPrebuildMcpType('mcp')).toBe(false);
  });

  it('false for non-string / absent input', () => {
    expect(isPrebuildMcpType(undefined)).toBe(false);
    expect(isPrebuildMcpType(null)).toBe(false);
    expect(isPrebuildMcpType('github')).toBe(false);
  });
});

describe('canonicalizeServerUrl', () => {
  it('lower-cases scheme and host, keeps explicit port and a non-root path verbatim (baseline only strips trailing "/" when the path itself is "/" or "")', () => {
    expect(canonicalizeServerUrl('HTTPS://Example.COM:8443/mcp/')).toBe('https://example.com:8443/mcp/');
  });

  it('strips a bare trailing slash on the root path', () => {
    expect(canonicalizeServerUrl('https://example.com/')).toBe('https://example.com');
  });

  it('keeps a meaningful trailing slash on a non-root path as normalized (still stripped per baseline behaviour)', () => {
    // Baseline behaviour (mcAuth.helpers.js:145): ANY trailing slash is stripped
    // when path is '/' or '' AFTER normalization — a genuinely non-root path
    // like '/mcp/' is NOT stripped, since its path is neither '/' nor ''.
    expect(canonicalizeServerUrl('https://example.com/mcp/')).toBe('https://example.com/mcp/');
  });

  it('passes credential-scoped composite keys through unparsed', () => {
    const composite = 'abc-123-uuid:https://login.example.com/tenant/oauth2';
    expect(canonicalizeServerUrl(composite)).toBe(composite);
  });

  it('falls back to the raw input on an unparseable URL', () => {
    expect(canonicalizeServerUrl('not a url')).toBe('not a url');
  });
});

describe('getStorageKey', () => {
  it('prefers the pre-built toolkitType over any serverUrl', () => {
    expect(getStorageKey({ serverUrl: 'https://x.example.com', toolkitType: 'mcp_github' })).toBe('mcp_github');
  });

  it('passes a credential-scoped serverUrl through as-is', () => {
    const composite = 'uuid-1:https://login.microsoftonline.com/tenant';
    expect(getStorageKey({ serverUrl: composite })).toBe(composite);
  });

  it('canonicalises a plain remote-MCP serverUrl', () => {
    expect(getStorageKey({ serverUrl: 'https://Example.com/mcp/' })).toBe('https://example.com/mcp/');
  });

  it('returns null when neither serverUrl nor a pre-built toolkitType is given', () => {
    expect(getStorageKey({})).toBeNull();
    expect(getStorageKey()).toBeNull();
    expect(getStorageKey({ toolkitType: 'github' })).toBeNull(); // not mcp_-prefixed
  });
});

describe('token CRUD round-trip', () => {
  it('setAccessToken -> getTokenInfo/getAccessToken/getRefreshToken/getSessionId round-trips every field', () => {
    setAccessToken(
      'https://mcp.example.com',
      'access-123',
      3600,
      'sess-1',
      'idtok-1',
      'refresh-1',
      { token_endpoint: 'https://mcp.example.com/token', client_id: 'cid' },
    );

    expect(getAccessToken('https://mcp.example.com')).toBe('access-123');
    expect(getRefreshToken('https://mcp.example.com')).toBe('refresh-1');
    expect(getSessionId('https://mcp.example.com')).toBe('sess-1');

    const info = getTokenInfo('https://mcp.example.com');
    expect(info?.token_endpoint).toBe('https://mcp.example.com/token');
    expect(info?.client_id).toBe('cid');
  });

  it('stores under toolkitType for pre-built MCPs, independent of serverUrl', () => {
    setAccessToken(undefined, 'tok', 60, undefined, undefined, undefined, {}, 'mcp_github');
    expect(getAccessToken(undefined, 'mcp_github')).toBe('tok');
    expect(getAccessToken('https://unrelated.example.com')).toBeNull();
  });

  it('a null expiresInSec produces a null expires_at (never expires from this path)', () => {
    setAccessToken('https://no-expiry.example.com', 'tok', null, undefined, undefined, undefined);
    const info = getTokenInfo('https://no-expiry.example.com');
    expect(info?.expires_at).toBeNull();
    expect(isExpired(info)).toBe(false);
  });

  it('setAccessToken with no resolvable storage key is a safe no-op', () => {
    expect(() => setAccessToken(undefined, 'tok', 60, undefined, undefined, undefined)).not.toThrow();
  });

  it('logout removes the token and getAccessToken returns null afterwards', () => {
    setAccessToken('https://logout-me.example.com', 'tok', 3600, undefined, undefined, undefined);
    expect(getAccessToken('https://logout-me.example.com')).toBe('tok');
    logout('https://logout-me.example.com');
    expect(getAccessToken('https://logout-me.example.com')).toBeNull();
  });

  it('logout on an already-absent key is a safe no-op', () => {
    expect(() => logout('https://never-logged-in.example.com')).not.toThrow();
  });

  it('rejects a session token issued before another tab logged out', () => {
    const server = 'https://cross-tab.example.com';
    const key = getStorageKey({ serverUrl: server })!;
    createStorage('local').set(getLogoutMarkerStorageKey(key)!, '200');
    createStorage('session').setJSON('mcp.tokens', {
      [key]: { access_token: 'stale-token', issued_at: 100, expires_at: null },
    });

    expect(getAccessToken(server)).toBeNull();
    expect(createStorage('session').getJSON('mcp.tokens')).toEqual({});
  });

  it('places same-millisecond reauthorization after the logout marker', () => {
    const server = 'https://reauthorize.example.com';
    vi.spyOn(Date, 'now').mockReturnValue(200);
    logout(server);
    setAccessToken(server, 'new-token', null, undefined, undefined, undefined);

    expect(getAccessToken(server)).toBe('new-token');
    expect(getTokenInfo(server)?.issued_at).toBe(201);
  });

  it('setSessionId updates only an EXISTING token record; no-op when absent', () => {
    setSessionId('https://has-token.example.com', 'new-session');
    expect(getSessionId('https://has-token.example.com')).toBeNull(); // no token existed yet

    setAccessToken('https://has-token.example.com', 'tok', 3600, 'orig-session', undefined, undefined);
    setSessionId('https://has-token.example.com', 'updated-session');
    expect(getSessionId('https://has-token.example.com')).toBe('updated-session');
  });
});

describe('expiry / refresh decisions', () => {
  it('isExpired is false with no expires_at', () => {
    expect(isExpired(null)).toBe(false);
    expect(isExpired({ access_token: 'a', issued_at: 0, expires_at: null })).toBe(false);
  });

  it('isExpired is true once Date.now() passes expires_at', () => {
    const past = { access_token: 'a', issued_at: Date.now() - 10_000, expires_at: Date.now() - 1 };
    expect(isExpired(past)).toBe(true);
  });

  it('needsProactiveRefresh is false before the 75% threshold, true after', () => {
    const now = Date.now();
    const issued = now - 8000; // token is 8s old
    const expires = now + 2000; // expires in 2s => total lifetime 10s, threshold at 7.5s
    expect(needsProactiveRefresh({ access_token: 'a', issued_at: issued, expires_at: expires })).toBe(true);

    const freshIssued = now - 1000; // 1s old of a 10s token => well under threshold
    const freshExpires = now + 9000;
    expect(needsProactiveRefresh({ access_token: 'a', issued_at: freshIssued, expires_at: freshExpires })).toBe(
      false,
    );
  });

  it('needsRefresh requires BOTH an expiry/threshold breach AND a refresh_token', () => {
    setAccessToken('https://needs-refresh.example.com', 'tok', -1, undefined, undefined, 'a-refresh-token');
    // expiresInSec=-1 => already expired
    expect(needsRefresh('https://needs-refresh.example.com')).toBe(true);
  });

  it('needsRefresh is false without a refresh_token even if expired', () => {
    setAccessToken('https://expired-no-refresh.example.com', 'tok', -1, undefined, undefined, undefined);
    expect(needsRefresh('https://expired-no-refresh.example.com')).toBe(false);
  });

  it('needsRefresh is false for an absent token', () => {
    expect(needsRefresh('https://never-seen.example.com')).toBe(false);
  });
});

describe('setConnectionVerified', () => {
  it('marks a header-based server as connected without overwriting a real token', () => {
    setConnectionVerified('https://header-auth.example.com');
    expect(getAccessToken('https://header-auth.example.com')).toBe('__connection_verified__');
  });

  it('does not clobber an existing real access token', () => {
    setAccessToken('https://real-token.example.com', 'real-access-token', 3600, undefined, undefined, undefined);
    setConnectionVerified('https://real-token.example.com');
    expect(getAccessToken('https://real-token.example.com')).toBe('real-access-token');
  });

  it('is a no-op with no resolvable storage key', () => {
    expect(() => setConnectionVerified(undefined)).not.toThrow();
  });
});

describe('credentials CRUD', () => {
  it('setSavedCredentials -> getSavedCredentials round-trips client id/secret', () => {
    setSavedCredentials({ serverUrl: 'https://creds.example.com', clientId: 'cid', clientSecret: 'csecret' });
    const creds = getSavedCredentials('https://creds.example.com');
    expect(creds).toEqual({ client_id: 'cid', client_secret: 'csecret' });
  });

  it('removeSavedCredentials clears a stored entry', () => {
    setSavedCredentials({ serverUrl: 'https://creds2.example.com', clientId: 'cid' });
    removeSavedCredentials('https://creds2.example.com');
    expect(getSavedCredentials('https://creds2.example.com')).toBeNull();
  });

  it('getSavedCredentials returns null for an unknown key', () => {
    expect(getSavedCredentials('https://unknown.example.com')).toBeNull();
  });
});

describe('ignored-servers list', () => {
  it('addIgnoredServer -> isServerIgnored -> getIgnoredServers round-trips', () => {
    addIgnoredServer('https://Ignored.example.com/');
    expect(isServerIgnored('https://ignored.example.com')).toBe(true);
    expect(getIgnoredServers()).toContain('https://ignored.example.com');
  });

  it('removeIgnoredServer clears the entry', () => {
    addIgnoredServer('https://temp-ignore.example.com');
    removeIgnoredServer('https://temp-ignore.example.com');
    expect(isServerIgnored('https://temp-ignore.example.com')).toBe(false);
  });

  it('clearIgnoredServers empties the whole list', () => {
    addIgnoredServer('https://a.example.com');
    addIgnoredServer('https://b.example.com');
    clearIgnoredServers();
    expect(getIgnoredServers()).toEqual([]);
  });

  it('setAccessToken removes a server from the ignored list once it gains a token', () => {
    addIgnoredServer('https://now-authed.example.com');
    expect(isServerIgnored('https://now-authed.example.com')).toBe(true);
    setAccessToken('https://now-authed.example.com', 'tok', 3600, undefined, undefined, undefined);
    expect(isServerIgnored('https://now-authed.example.com')).toBe(false);
  });

  it('getFilteredIgnoredServers merges the ignored list with un-tokened candidates, deduplicated', () => {
    addIgnoredServer('https://explicitly-ignored.example.com');
    setAccessToken('https://has-token.example.com', 'tok', 3600, undefined, undefined, undefined);

    const filtered = getFilteredIgnoredServers([
      'https://has-token.example.com', // has a valid token -> excluded
      'https://candidate-without-token.example.com', // no token -> included
      'https://explicitly-ignored.example.com', // already in the list -> not duplicated
    ]);

    expect(filtered).toContain('https://candidate-without-token.example.com');
    expect(filtered).toContain('https://explicitly-ignored.example.com');
    expect(filtered).not.toContain('https://has-token.example.com');
    expect(filtered.filter((u) => canonicalizeServerUrlSafe(u) === 'https://explicitly-ignored.example.com')).toHaveLength(1);
  });

  it('getFilteredIgnoredServers returns [] when nothing is ignored', () => {
    expect(getFilteredIgnoredServers(['https://no-tokens-no-ignores.example.com'])).toEqual([]);
  });

  it('getServersWithoutTokens filters to only the un-tokened candidates, ignoring the ignored-list entirely', () => {
    setAccessToken('https://tokened.example.com', 'tok', 3600, undefined, undefined, undefined);
    const result = getServersWithoutTokens(['https://tokened.example.com', 'https://bare.example.com']);
    expect(result).toEqual(['https://bare.example.com']);
  });
});

function canonicalizeServerUrlSafe(u: string): string {
  return canonicalizeServerUrl(u);
}
