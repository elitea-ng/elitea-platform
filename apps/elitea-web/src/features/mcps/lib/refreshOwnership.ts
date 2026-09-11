import type { McpOAuthTokenResponse } from '../api/mcpOAuthClient';

import { getStorageKey, getTokenInfo } from './storage';
import type { StoredMcpToken } from './types';

const refreshes = new Map<string, Promise<McpOAuthTokenResponse>>();

/** Share one refresh grant per credential key in this tab. */
export function withRefreshOwnership(key: string, refresh: () => Promise<McpOAuthTokenResponse>): Promise<McpOAuthTokenResponse> {
  const canonicalKey = getStorageKey({ serverUrl: key }) ?? key;
  const pending = refreshes.get(canonicalKey);
  if (pending) return pending;
  const request = refresh().finally(() => { refreshes.delete(canonicalKey); });
  refreshes.set(canonicalKey, request);
  return request;
}

/** Logout and later authorization take precedence over an old refresh response. */
export function stillOwnsToken(key: string, expected: StoredMcpToken | null): boolean {
  const current = getTokenInfo(key);
  return Boolean(expected && current && current.issued_at === expected.issued_at
    && current.access_token === expected.access_token && current.refresh_token === expected.refresh_token);
}
