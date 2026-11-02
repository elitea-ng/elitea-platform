/**
 * ROUTE-050 `/mcp-auth-callback` page — port of
 * apps/elitea-ui/src/[fsd]/pages/mcp/index.jsx (unit A5). Query params
 * PARAM-048..051 (`code`/`state`/`error`/`error_description`).
 *
 * This page receives the OAuth redirect FROM the MCP server's authorize
 * endpoint. It does NOT perform the token exchange itself — it relays the
 * code/error back to the OPENER window (the page that opened this as a
 * popup, via `features/mcps`' `McpAuthModal` -> `startMcpAuthFlow` ->
 * `openAuthPopup`), which then performs the exchange using the app's own
 * authenticated session (spec: "Token exchange uses the app's access_token
 * (user is already logged in)"). `features/mcps/lib/window.ts`'s
 * `createAuthorizationMonitor` is the receiving end of the three channels
 * `sendAuthResult` below uses (postMessage / BroadcastChannel /
 * localStorage-as-signal).
 *
 * pages/ never fetches (spec §3.2) — there is genuinely no data fetch here
 * either in the baseline or this port; the only "backend" this page talks
 * to is the opener window via browser messaging primitives, which is a
 * page-layer concern (composition/wiring), not a features/ data hook.
 *
 * DEVIATION FROM BASELINE: query params come from the route's own
 * `validateSearch` (`src/routes/_shell/mcp-auth-callback.tsx`, R1;
 * `pickParams('code', 'error', 'error_description', 'state')`) via
 * `Route.useSearch()`, not a raw `new URLSearchParams(window.location.search)`
 * read — the whole point of TanStack Router's per-route search schema
 * (spec §8.2). Every field defaults to `''` (not `null`); `''` is treated
 * as "absent" throughout, matching the baseline's `params.get(...)` ->
 * `null` semantics.
 *
 * `localStorage` writes go through `shared/lib/storage.ts`'s
 * `createStorage('local')` — the sanctioned single place to touch it — so
 * this transient key is swept by `clearNamespace()` on logout if a race
 * ever leaves one behind, same rationale as `features/mcps/lib/window.ts`'s
 * reading side.
 */
import type { ReactNode } from 'react';
import { useEffect, useMemo, useRef, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { getRouteApi } from '@tanstack/react-router';

import { t } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';

const routeApi = getRouteApi('/_shell/mcp-auth-callback');

type CallbackStatus = 'processing' | 'success' | 'error';

interface AuthResultMessage {
  type: 'mcp-auth-result';
  state?: string | undefined;
  success?: boolean | undefined;
  code?: string | undefined;
  error?: string | undefined;
  error_description?: string | undefined;
}

function crossTabKey(state: string): string {
  return `mcp-auth-result-${state}`;
}

/**
 * Sends the auth result to the opener via all three channels so delivery
 * survives `window.opener` being severed (the popup navigated cross-origin
 * to the MCP server's own pages before landing back here).
 */
function sendAuthResult(message: AuthResultMessage): void {
  // `Window.opener` is typed `any` by the DOM lib; narrow it once here so
  // every read below is type-checked instead of `no-unsafe-member-access`.
  const opener = window.opener as Window | null | undefined;
  if (opener && !opener.closed) {
    try {
      opener.postMessage(message, window.location.origin);
    } catch {
      // Opener may be cross-origin/blocked — the other two channels still cover it.
    }
  }

  if (message.state) {
    try {
      const channel = new BroadcastChannel(`mcp-auth-${message.state}`);
      channel.postMessage(message);
      channel.close();
    } catch {
      // BroadcastChannel unsupported.
    }
  }

  const { state } = message;
  if (state) {
    try {
      const local = createStorage('local');
      local.setJSON(crossTabKey(state), message);
      setTimeout(() => local.remove(crossTabKey(state)), 5000);
    } catch {
      // localStorage may be disabled.
    }
  }
}

function containerSx(theme: Theme) {
  return {
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'center',
    justifyContent: 'center',
    padding: theme.spacing(4),
    // The shell's scroll box, not the viewport (which the title row shares).
    minHeight: '100%',
    boxSizing: 'border-box' as const,
    gap: theme.spacing(2),
  };
}

export function McpAuthCallbackPage(): ReactNode {
  const search = routeApi.useSearch();
  const [status, setStatus] = useState<CallbackStatus>('processing');
  const [errorMessage, setErrorMessage] = useState('');

  // Prevents double-processing under React Strict Mode's double-invoked effects.
  const processedRef = useRef(false);

  const authParams = useMemo(
    () => ({
      code: search.code || undefined,
      state: search.state || undefined,
      error: search.error || undefined,
      error_description: search.error_description || undefined,
    }),
    [search.code, search.state, search.error, search.error_description],
  );

  useEffect(() => {
    if (processedRef.current) return;
    processedRef.current = true;

    if (authParams.error) {
      sendAuthResult({ type: 'mcp-auth-result', state: authParams.state, error: authParams.error, error_description: authParams.error_description });
      setStatus('error');
      setErrorMessage(authParams.error_description ?? authParams.error);
      // Read at point-of-use (not a module-top-level constant): a component
      // that reads `import.meta.env.DEV` inside a function body, rather
      // than caching it at import time, stays controllable by
      // `vi.stubEnv('DEV', …)` per-test — same convention as
      // `app/providers/basename.ts`'s `getAppBasename()`.
      if (!import.meta.env.DEV) setTimeout(() => window.close(), 2000);
      return;
    }

    if (authParams.code) {
      sendAuthResult({ type: 'mcp-auth-result', state: authParams.state, success: true, code: authParams.code });
      setStatus('success');
      setTimeout(() => window.close(), 1000);
      return;
    }

    sendAuthResult({ type: 'mcp-auth-result', state: authParams.state, error: 'invalid_request', error_description: 'Missing authorization code' });
    setStatus('error');
    setErrorMessage(t('mcpAuthCallback.invalidResponse', 'Invalid authorization response'));
  }, [authParams]);

  return (
    <Box sx={containerSx}>
      {status === 'processing' && (
        <>
          <CircularProgress size={24} />
          <Typography variant="bodyMedium">{t('mcpAuthCallback.processing', 'Processing authorization...')}</Typography>
        </>
      )}
      {status === 'success' && (
        <Typography
          variant="bodyMedium"
          sx={{ color: 'status.published' }}
        >
          {t('mcpAuthCallback.success', 'Authorization successful! Closing window...')}
        </Typography>
      )}
      {status === 'error' && (
        <>
          <Typography
            variant="bodyMedium"
            sx={{ color: 'status.rejected' }}
          >
            {t('mcpAuthCallback.failed', 'Authorization failed')}
          </Typography>
          {errorMessage && (
            <Typography
              variant="bodySmall"
              sx={{ color: 'text.secondary' }}
            >
              {errorMessage}
            </Typography>
          )}
        </>
      )}
    </Box>
  );
}
