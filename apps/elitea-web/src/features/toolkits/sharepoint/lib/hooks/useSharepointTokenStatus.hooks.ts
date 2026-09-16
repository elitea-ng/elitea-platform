import { useCallback, useEffect, useState } from 'react';
import { getLogoutMarkerEventKey } from '@/shared/lib/oauthLogoutSync';

import { MCP_TOKEN_CHANGE_EVENT, getAccessToken, getStorageKey } from '../helpers/mcpTokenStorage.helpers';

/**
 * Local, `features/toolkits`-owned duplicate of
 * `apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpTokenChange.hooks.js`
 * — see `../helpers/mcpTokenStorage.helpers.ts`'s own doc comment for the
 * full "thin piece, not a redesign" rationale (`no-sideways-features`
 * forbids importing the real hook from `features/mcps`).
 *
 * Only the `serverUrl` (SharePoint's own composite-key or plain-URL shape,
 * `../helpers/token.helpers.ts`) input path is kept — the baseline's
 * `toolkitType` pre-built-MCP branch is retained in the underlying storage
 * helper for `getStorageKey`'s precedence order, but SharePoint's own
 * callers (`SharepointDelegatedLoginButton.jsx`/`SharepointOAuthStatus.jsx`)
 * never pass one, so this hook's own parameter list drops it rather than
 * exposing dead surface.
 */
export interface UseSharepointTokenStatusResult {
  readonly isLoggedIn: boolean;
  readonly refreshLoginStatus: () => void;
}

export function useSharepointTokenStatus(serverUrl: string | undefined): UseSharepointTokenStatusResult {
  const storageKey = getStorageKey({ serverUrl });

  const [isLoggedIn, setIsLoggedIn] = useState<boolean>(() => (storageKey !== null ? getAccessToken(serverUrl) !== null : false));

  const refreshLoginStatus = useCallback(() => {
    setIsLoggedIn(storageKey !== null && getAccessToken(serverUrl) !== null);
  }, [storageKey, serverUrl]);

  useEffect(() => {
    refreshLoginStatus();
  }, [refreshLoginStatus]);

  useEffect(() => {
    if (storageKey === null) return;

    function handleTokenChange(event: Event): void {
      const detail = (event as CustomEvent<{ readonly serverUrl?: string }>).detail;
      if (detail?.serverUrl === storageKey) refreshLoginStatus();
    }
    function handleCrossTabLogout(event: StorageEvent): void {
      if (event.key === getLogoutMarkerEventKey(storageKey) && event.newValue !== null) refreshLoginStatus();
    }

    window.addEventListener(MCP_TOKEN_CHANGE_EVENT, handleTokenChange);
    window.addEventListener('storage', handleCrossTabLogout);
    return () => {
      window.removeEventListener(MCP_TOKEN_CHANGE_EVENT, handleTokenChange);
      window.removeEventListener('storage', handleCrossTabLogout);
    };
  }, [storageKey, refreshLoginStatus]);

  return { isLoggedIn, refreshLoginStatus };
}
