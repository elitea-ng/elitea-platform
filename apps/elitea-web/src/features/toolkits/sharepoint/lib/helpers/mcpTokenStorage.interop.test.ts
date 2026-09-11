/**
 * Cross-slice interop: SharePoint READS the token record that `features/mcps`
 * WRITES.
 *
 * `no-sideways-features` forbids `features/toolkits` importing
 * `features/mcps`, so this module hand-copies three constants (storage key,
 * change-event name, verified sentinel) from `features/mcps/lib/constants.ts`.
 * A hand-copied constant is exactly the kind of thing that drifts silently —
 * and it HAD drifted: this module read the baseline's raw
 * `sessionStorage['mcp_oauth_tokens']` while the real writer had long since
 * moved to the namespaced `el.mcp.tokens`. Nothing failed. A SharePoint OAuth
 * login simply never showed as connected.
 *
 * This test pins the two sides together through their REAL implementations —
 * `features/mcps`' own writer, this module's reader — so any future change to
 * either name fails here instead of silently disconnecting the feature. The
 * import below is the one place in the repo where the two slices meet, and it
 * is a TEST file (dependency-cruiser excludes `*.test.*` from the layer gate,
 * see `.dependency-cruiser.cjs`'s `options.exclude`).
 */
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getTokenInfo, logout as mcpLogout, setAccessToken } from '@/features/mcps/lib/storage';
import { getLogoutMarkerEventKey, loadLogoutMarker, publishLogout } from '@/shared/lib/oauthLogoutSync';
import { createStorage } from '@/shared/lib/storage';

import { MCP_TOKEN_CHANGE_EVENT, getAccessToken, logout } from './mcpTokenStorage.helpers';
import { useSharepointTokenStatus } from '../hooks/useSharepointTokenStatus.hooks';

/** The composite `"{configUuid}:{oauthEndpoint}"` key SharePoint credentials use (`token.helpers.ts`). */
const SHAREPOINT_TOKEN_KEY = 'cfg-uuid-1:https://login.microsoftonline.com/tenant';

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
});

describe('SharePoint token reader ↔ features/mcps token writer', () => {
  it('publishes the common logout marker and invalidates copied refresh grants', () => {
    setAccessToken(SHAREPOINT_TOKEN_KEY, 'sp-token', 3600, undefined, undefined, 'refresh');
    const copy = createStorage('session').get('mcp.tokens')!;
    logout(SHAREPOINT_TOKEN_KEY);
    expect(loadLogoutMarker(SHAREPOINT_TOKEN_KEY)).toBeGreaterThan(0);
    // Another document still has its sessionStorage copy until the next read.
    createStorage('session').set('mcp.tokens', copy);
    expect(getAccessToken(SHAREPOINT_TOKEN_KEY)).toBeNull();
    expect(getTokenInfo(SHAREPOINT_TOKEN_KEY)).toBeNull();
  });

  it('updates the older status widget after another tab logs out', () => {
    setAccessToken(SHAREPOINT_TOKEN_KEY, 'sp-token', 3600, undefined, undefined, 'refresh');
    const { result } = renderHook(() => useSharepointTokenStatus(SHAREPOINT_TOKEN_KEY));
    expect(result.current.isLoggedIn).toBe(true);
    publishLogout(SHAREPOINT_TOKEN_KEY);
    act(() => { window.dispatchEvent(new StorageEvent('storage', {
      key: getLogoutMarkerEventKey(SHAREPOINT_TOKEN_KEY), newValue: String(loadLogoutMarker(SHAREPOINT_TOKEN_KEY)),
    })); });
    expect(result.current.isLoggedIn).toBe(false);
  });
  it('reads back a token written by the real features/mcps writer (the OAuth modal path)', () => {
    setAccessToken(SHAREPOINT_TOKEN_KEY, 'sp-oauth-token', 3600, null, null, null);

    expect(getAccessToken(SHAREPOINT_TOKEN_KEY)).toBe('sp-oauth-token');
  });

  it('reports null again once features/mcps removes the token', () => {
    setAccessToken(SHAREPOINT_TOKEN_KEY, 'sp-oauth-token', 3600, null, null, null);
    mcpLogout(SHAREPOINT_TOKEN_KEY);

    expect(getAccessToken(SHAREPOINT_TOKEN_KEY)).toBeNull();
  });

  it('agrees with features/mcps on the token-change event name, so useSharepointTokenStatus actually wakes up', () => {
    const seen: string[] = [];
    const listener = (event: Event): void => {
      seen.push((event as CustomEvent<{ readonly serverUrl?: string }>).detail?.serverUrl ?? '');
    };
    window.addEventListener(MCP_TOKEN_CHANGE_EVENT, listener);

    setAccessToken(SHAREPOINT_TOKEN_KEY, 'sp-oauth-token', 3600, null, null, null);

    window.removeEventListener(MCP_TOKEN_CHANGE_EVENT, listener);
    expect(seen).toContain(SHAREPOINT_TOKEN_KEY);
  });
});
