import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { STORAGE_NAMESPACE, clearNamespace } from '@/shared/lib/storage';

import {
  MCP_TOKEN_CHANGE_EVENT,
  canonicalizeServerUrl,
  getAccessToken,
  getStorageKey,
  isPrebuildMcpType,
  logout,
  setConnectionVerified,
} from './mcpTokenStorage.helpers';

describe('isPrebuildMcpType', () => {
  it('returns true for an mcp_-prefixed type other than the bare "mcp"', () => {
    expect(isPrebuildMcpType('mcp_github')).toBe(true);
  });

  it('returns false for the bare "mcp" (remote MCP, not pre-built)', () => {
    expect(isPrebuildMcpType('mcp')).toBe(false);
  });

  it('returns false for "sharepoint" (SharePoint is never a pre-built MCP type)', () => {
    expect(isPrebuildMcpType('sharepoint')).toBe(false);
  });

  it('returns false for undefined', () => {
    expect(isPrebuildMcpType(undefined)).toBe(false);
  });
});

describe('canonicalizeServerUrl', () => {
  it('lower-cases scheme/host and drops a trailing slash on a bare origin', () => {
    expect(canonicalizeServerUrl('HTTPS://Login.Microsoftonline.com/')).toBe('https://login.microsoftonline.com');
  });

  it('preserves a non-trivial path', () => {
    expect(canonicalizeServerUrl('https://login.microsoftonline.com/tenant/oauth2/token')).toBe(
      'https://login.microsoftonline.com/tenant/oauth2/token',
    );
  });

  it('passes a credential-scoped composite key through unchanged', () => {
    expect(canonicalizeServerUrl('uuid-1:https://login.microsoftonline.com/tenant')).toBe(
      'uuid-1:https://login.microsoftonline.com/tenant',
    );
  });

  it('falls back to the raw input on an unparseable URL', () => {
    expect(canonicalizeServerUrl('not a url')).toBe('not a url');
  });
});

describe('getStorageKey', () => {
  it('prefers the pre-built toolkitType key', () => {
    expect(getStorageKey({ serverUrl: 'https://example.com', toolkitType: 'mcp_github' })).toBe('mcp_github');
  });

  it('returns a credential-scoped composite serverUrl as-is', () => {
    expect(getStorageKey({ serverUrl: 'uuid-1:https://login.microsoftonline.com/tenant' })).toBe(
      'uuid-1:https://login.microsoftonline.com/tenant',
    );
  });

  it('canonicalizes a plain serverUrl', () => {
    expect(getStorageKey({ serverUrl: 'HTTPS://Login.Microsoftonline.com/' })).toBe('https://login.microsoftonline.com');
  });

  it('returns null when neither serverUrl nor toolkitType is given', () => {
    expect(getStorageKey({})).toBeNull();
  });
});

describe('getAccessToken / logout / setConnectionVerified (sessionStorage round-trip)', () => {
  const serverUrl = 'uuid-1:https://login.microsoftonline.com/tenant';

  beforeEach(() => {
    window.sessionStorage.clear();
  });

  afterEach(() => {
    window.sessionStorage.clear();
    vi.restoreAllMocks();
  });

  it('getAccessToken returns null when nothing is stored', () => {
    expect(getAccessToken(serverUrl)).toBeNull();
  });

  /**
   * Issue #22. These tokens used to be written to a RAW, un-namespaced
   * `sessionStorage` key (`mcp_oauth_tokens`), bypassing
   * `shared/lib/storage.ts`. `performLogout()` sweeps only the `el.*`
   * namespace, so a SharePoint OAuth access token survived logout intact and
   * was inherited by whoever used the tab next.
   *
   * §5.4's own completeness test (`shared/api/auth/logout.test.ts`) could not
   * catch this: it proves "write-set minus cleared-set is empty" from the
   * storage wrapper's write TRACKER, which by construction never sees a write
   * that bypassed the wrapper.
   */
  it('stores under the el.* namespace so clearNamespace() (logout) actually removes the token', () => {
    setConnectionVerified(serverUrl);
    expect(getAccessToken(serverUrl)).not.toBeNull();

    const keys = Object.keys(window.sessionStorage);
    expect(keys.every((key) => key.startsWith(STORAGE_NAMESPACE))).toBe(true);

    clearNamespace();
    expect(getAccessToken(serverUrl)).toBeNull();
  });

  it('leaves no un-namespaced sessionStorage key behind after a write', () => {
    setConnectionVerified(serverUrl);
    expect(window.sessionStorage.getItem('mcp_oauth_tokens')).toBeNull();
  });

  it('setConnectionVerified stores a verified marker getAccessToken then reads back', () => {
    setConnectionVerified(serverUrl);
    expect(getAccessToken(serverUrl)).toBe('__connection_verified__');
  });

  it('setConnectionVerified does not overwrite an existing real token', () => {
    window.sessionStorage.setItem(
      `${STORAGE_NAMESPACE}mcp.tokens`,
      JSON.stringify({ [serverUrl]: { access_token: 'real-token', issued_at: Date.now(), expires_at: Date.now() + 60_000 } }),
    );
    setConnectionVerified(serverUrl);
    expect(getAccessToken(serverUrl)).toBe('real-token');
  });

  it('logout removes the stored token and getAccessToken reports null afterwards', () => {
    setConnectionVerified(serverUrl);
    expect(getAccessToken(serverUrl)).not.toBeNull();
    logout(serverUrl);
    expect(getAccessToken(serverUrl)).toBeNull();
  });

  it('getAccessToken returns null for an expired token', () => {
    window.sessionStorage.setItem(
      `${STORAGE_NAMESPACE}mcp.tokens`,
      JSON.stringify({ [serverUrl]: { access_token: 'stale', issued_at: 0, expires_at: 1 } }),
    );
    expect(getAccessToken(serverUrl)).toBeNull();
  });

  it('setConnectionVerified dispatches MCP_TOKEN_CHANGE_EVENT with the resolved key and "login" type', () => {
    const listener = vi.fn();
    window.addEventListener(MCP_TOKEN_CHANGE_EVENT, listener);
    setConnectionVerified(serverUrl);
    expect(listener).toHaveBeenCalledTimes(1);
    const event = listener.mock.calls[0]?.[0] as CustomEvent<{ serverUrl: string; type: string }>;
    expect(event.detail).toEqual({ serverUrl, type: 'login' });
    window.removeEventListener(MCP_TOKEN_CHANGE_EVENT, listener);
  });

  it('logout publishes invalidation even without a local token because other tabs can hold it', () => {
    const listener = vi.fn();
    window.addEventListener(MCP_TOKEN_CHANGE_EVENT, listener);
    logout(serverUrl);
    expect(listener).toHaveBeenCalledTimes(1);
    setConnectionVerified(serverUrl);
    logout(serverUrl);
    expect(listener).toHaveBeenCalledTimes(3);
    const secondCall = listener.mock.calls[2];
    if (secondCall === undefined) throw new Error('expected a second call');
    const secondEvent = secondCall[0] as CustomEvent<{ type: string }>;
    expect(secondEvent.detail.type).toBe('logout');
    window.removeEventListener(MCP_TOKEN_CHANGE_EVENT, listener);
  });
});
