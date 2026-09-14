/**
 * Port of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpTokenChange.hooks.js
 * (unit A5). Monitors the `elitea_mcp_token_change` `window` event
 * `storage.ts` dispatches, so the whole UI re-renders login/logout state
 * immediately, even when the change originated in a different component
 * tree (e.g. a modal's `onAuthorize` vs. a status badge elsewhere on the
 * same page).
 */
import { useCallback, useEffect, useState } from 'react';

import { MCP_TOKEN_CHANGE_EVENT } from '../lib/constants';
import { getLogoutMarkerEventKey } from '../lib/logoutSync';
import { getAccessToken, getStorageKey, getTokenInfo } from '../lib/storage';

export interface McpTokenChangeOptions {
  serverUrl?: string | undefined;
  toolkitType?: string | undefined;
}

export interface McpTokenChangeResult {
  isLoggedIn: boolean;
  /** Allows logout after access-token expiry, including a saved refresh grant. */
  hasStoredAuthorization: boolean;
  refreshLoginStatus: () => void;
}

interface TokenChangeEventDetail {
  serverUrl: string;
  type: 'login' | 'logout';
}

/** Accepts either a bare server-URL string (legacy call shape) or an options object. */
export function useMcpTokenChange(serverUrlOrOptions: string | McpTokenChangeOptions | undefined): McpTokenChangeResult {
  const options = typeof serverUrlOrOptions === 'string' ? { serverUrl: serverUrlOrOptions } : (serverUrlOrOptions ?? {});
  const { serverUrl, toolkitType } = options;
  const storageKey = getStorageKey({ serverUrl, toolkitType });
  const logoutMarkerKey = getLogoutMarkerEventKey(storageKey);

  const [isLoggedIn, setIsLoggedIn] = useState<boolean>(() => (storageKey ? getAccessToken(serverUrl, toolkitType) !== null : false));
  const [hasStoredAuthorization, setHasStoredAuthorization] = useState(() => getTokenInfo(serverUrl, toolkitType) !== null);

  const refreshLoginStatus = useCallback(() => {
    setIsLoggedIn(Boolean(storageKey) && getAccessToken(serverUrl, toolkitType) !== null);
    setHasStoredAuthorization(getTokenInfo(serverUrl, toolkitType) !== null);
  }, [storageKey, serverUrl, toolkitType]);

  useEffect(() => {
    refreshLoginStatus();
  }, [refreshLoginStatus]);

  useEffect(() => {
    if (!storageKey) return;

    function handleTokenChange(event: Event): void {
      const detail = (event as CustomEvent<TokenChangeEventDetail>).detail;
      if (detail?.serverUrl === storageKey) refreshLoginStatus();
    }

    function handleCrossTabLogout(event: StorageEvent): void {
      if (event.key === logoutMarkerKey && event.newValue !== null) refreshLoginStatus();
    }

    window.addEventListener(MCP_TOKEN_CHANGE_EVENT, handleTokenChange);
    window.addEventListener('storage', handleCrossTabLogout);
    return () => {
      window.removeEventListener(MCP_TOKEN_CHANGE_EVENT, handleTokenChange);
      window.removeEventListener('storage', handleCrossTabLogout);
    };
  }, [storageKey, logoutMarkerKey, refreshLoginStatus]);

  return { isLoggedIn, hasStoredAuthorization, refreshLoginStatus };
}
