/**
 * Token refresh/validity lifecycle — port of the refresh half of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpAuthFlow.helpers.js
 * (unit A5, spec §9.3): `triggerProactiveRefresh`, `refreshAccessToken`,
 * `getValidAccessToken`. The authorization-CODE half (`startMcpAuthFlow`)
 * lives in `oauthFlow.ts` — split for the §3.5 400-line budget and because
 * the two halves have distinct call graphs (background refresh vs.
 * user-initiated popup flow).
 *
 * Wires itself into `tokenRefresh.ts`'s refresh queue on import
 * (`configureRefreshTrigger(triggerProactiveRefresh)`) — a plain function
 * reference swap, not a store/subscription, so it is safe at module scope
 * per R-S2's actual concern (no zustand `create()` at module scope in a
 * file `app/` also imports).
 */
import { refreshMcpOAuthToken } from '../api/mcpOAuthClient';
import type { McpOAuthTokenResponse } from '../api/mcpOAuthClient';
import { getToolkitOAuthSettings } from '../api/toolkitCredentials';

import {
  canonicalizeServerUrl,
  getAccessToken,
  getRefreshToken,
  getSavedCredentials,
  getTokenInfo,
  logout,
  needsRefresh,
  setAccessToken,
} from './storage';
import { clearRefreshPending, configureRefreshTrigger, markRefreshPending } from './tokenRefresh';
import { generateSessionId } from './crypto';
import type { StoredMcpToken } from './types';
import { stillOwnsToken, withRefreshOwnership } from './refreshOwnership';

interface ResolvedCredentials {
  clientId: string | null;
  clientSecret: string | null;
  tokenEndpoint: string | null;
}

function resolveCredentials(serverUrl: string, tokenInfo: StoredMcpToken | null): ResolvedCredentials {
  const saved = getSavedCredentials(serverUrl);
  return {
    clientId: saved?.client_id ?? null,
    clientSecret: saved?.client_secret ?? null,
    tokenEndpoint: tokenInfo?.token_endpoint ?? null,
  };
}

/**
 * Credential-resolution steps 2 and 3, each split into its own function
 * (§3.5 complexity budget: `triggerProactiveRefresh`'s inlined form
 * measured 24 — a single `resolveRefreshCredentials` extraction still
 * landed exactly at the 12 boundary, so each fallback step gets its own
 * tiny function to leave headroom).
 */
async function applyToolkitCredentialFallback(tokenInfo: StoredMcpToken, credentials: ResolvedCredentials): Promise<ResolvedCredentials> {
  // Baseline: `mcpAuthFlow.helpers.js`'s proactive-refresh credential
  // resolution — `if (!tokenInfo?.used_dcr) { /* try toolkit API */ }`.
  // Once Dynamic Client Registration issued this token's client_id/secret,
  // the toolkit's DB-configured OAuth client must never silently replace
  // it (a different, unrelated client for the refresh_token grant) —
  // `applyStoredCredentialFallback` (the next step) recovers the
  // DCR-issued client_id/secret from `tokenInfo.client_id`/`client_secret`
  // instead, which `oauthFlow.ts`'s `buildTokenPersistenceMetadata`
  // already persists regardless of `used_dcr`.
  if (credentials.clientId || tokenInfo.used_dcr || !tokenInfo.toolkit_id || !tokenInfo.project_id) return credentials;
  const toolkitSettings = await getToolkitOAuthSettings(tokenInfo.project_id, tokenInfo.toolkit_id);
  if (!toolkitSettings) return credentials;
  return {
    clientId: toolkitSettings.client_id ?? credentials.clientId,
    clientSecret: toolkitSettings.client_secret ?? credentials.clientSecret,
    tokenEndpoint: toolkitSettings.token_endpoint ?? credentials.tokenEndpoint,
  };
}

function applyStoredCredentialFallback(tokenInfo: StoredMcpToken, credentials: ResolvedCredentials): ResolvedCredentials {
  if (credentials.clientId || !tokenInfo.client_id) return credentials;
  return {
    ...credentials,
    clientId: tokenInfo.client_id,
    clientSecret: credentials.clientSecret ?? tokenInfo.client_secret ?? null,
  };
}

/** Saved credentials -> toolkit API fallback -> stored token metadata, in that order (baseline: `mcpAuth.helpers.js`'s proactive-refresh credential resolution). */
async function resolveRefreshCredentials(serverUrl: string, tokenInfo: StoredMcpToken): Promise<ResolvedCredentials> {
  if (tokenInfo.used_dcr) return {
    clientId: tokenInfo.client_id ?? null,
    clientSecret: tokenInfo.client_secret ?? null,
    tokenEndpoint: tokenInfo.token_endpoint ?? null,
  };
  const base = resolveCredentials(serverUrl, tokenInfo);
  const withToolkitFallback = await applyToolkitCredentialFallback(tokenInfo, base);
  return applyStoredCredentialFallback(tokenInfo, withToolkitFallback);
}

/** Applies a successful refresh response to storage — a no-op when the response carried no `access_token` (baseline treats that as a silent failure, not an error). */
function applyRefreshedTokenResult(serverUrl: string, tokenInfo: StoredMcpToken, credentials: ResolvedCredentials, tokenJson: McpOAuthTokenResponse): void {
  if (!tokenJson.access_token || !stillOwnsToken(serverUrl, tokenInfo)) return;

  const canonicalServer = canonicalizeServerUrl(serverUrl);
  const sessionId = tokenJson.session_id ?? tokenInfo.session_id ?? generateSessionId();
  setAccessToken(
    canonicalServer,
    tokenJson.access_token,
    tokenJson.expires_in,
    sessionId,
    tokenJson.id_token,
    tokenJson.refresh_token ?? tokenInfo.refresh_token,
    { token_endpoint: credentials.tokenEndpoint ?? undefined, client_id: credentials.clientId ?? undefined, client_secret: credentials.clientSecret ?? undefined },
  );
}

/** Refresh only at the configured endpoint. Keep one active grant per credential key. */
async function performProactiveRefresh(serverUrl: string, tokenInfo: StoredMcpToken, credentials: ResolvedCredentials): Promise<void> {
  if (!credentials.tokenEndpoint) return;

  await withRefreshOwnership(serverUrl, async () => {
    requireCurrentAuthorization(serverUrl, tokenInfo);
    const tokenJson = await refreshMcpOAuthToken({
      projectId: tokenInfo.project_id ?? 1,
      refresh_token: tokenInfo.refresh_token as string,
      token_endpoint: credentials.tokenEndpoint ?? undefined,
      client_id: credentials.clientId ?? undefined,
      client_secret: credentials.clientSecret ?? undefined,
      toolkit_id: tokenInfo.toolkit_id,
      // DCR credentials belong to this grant, not to the stored toolkit client.
      used_dcr: tokenInfo.used_dcr || undefined,
    });

    applyRefreshedTokenResult(serverUrl, tokenInfo, credentials, tokenJson);
    return tokenJson;
  });
}

/**
 * Refresh stored credentials without discarding a still-valid token on transient failure.
 * Execution callers await completion. The background queue can ignore the returned promise.
 */
export async function triggerProactiveRefresh(serverUrl: string): Promise<void> {
  markRefreshPending(serverUrl);

  const tokenInfo = getTokenInfo(serverUrl);
  if (!tokenInfo?.refresh_token) {
    clearRefreshPending(serverUrl);
    return;
  }

  try {
    const credentials = await resolveRefreshCredentials(serverUrl, tokenInfo);
    await performProactiveRefresh(serverUrl, tokenInfo, credentials);
  } catch {
    // Keep a still-valid token on transient failure. Never log provider errors or credential keys.
  } finally {
    clearRefreshPending(serverUrl);
  }
}

configureRefreshTrigger((key) => { void triggerProactiveRefresh(key); });

export interface RefreshAccessTokenOptions {
  serverUrl: string;
  tokenEndpoint?: string;
  clientId?: string;
  clientSecret?: string;
  projectId?: string | number;
  toolkitId?: string;
}

export interface McpOAuthTokenResult {
  access_token: string;
  expires_in?: number;
  session_id?: string;
  id_token?: string;
  refresh_token?: string;
}

/** `true || undefined` normalization for a wire field, split out purely to keep `refreshAccessToken`'s own cyclomatic-complexity count under the §3.5 budget (12) — same tiny helper as `oauthFlow.ts`'s `toWireFlag`, duplicated locally rather than shared (too small to be worth a cross-file import for a single extra branch). */
function toWireFlag(used: boolean | undefined): boolean | undefined {
  return used || undefined;
}

function clearFailedRefresh(serverUrl: string, token: StoredMcpToken | null): void {
  if (stillOwnsToken(serverUrl, token)) logout(serverUrl);
}

function requireCurrentAuthorization(serverUrl: string, token: StoredMcpToken | null): void {
  if (!stillOwnsToken(serverUrl, token)) throw new Error('Authorization changed during token refresh');
}

/** User-visible (awaited) refresh — clears the stored token on failure (it may have been revoked), unlike the silent proactive path. */
export function refreshAccessToken(options: RefreshAccessTokenOptions): Promise<McpOAuthTokenResult> {
  return withRefreshOwnership(options.serverUrl, () => refreshOwnedAccessToken(options));
}

async function refreshOwnedAccessToken(options: RefreshAccessTokenOptions): Promise<McpOAuthTokenResult> {
  const { serverUrl, tokenEndpoint, clientId, clientSecret, projectId, toolkitId } = options;

  const refreshToken = getRefreshToken(serverUrl);
  if (!refreshToken) {
    throw new Error('No refresh token available');
  }

  const canonicalServer = canonicalizeServerUrl(serverUrl);
  // Fetched up front (not just after the refresh call) so `used_dcr` is
  // available to send on the request itself — see `performProactiveRefresh`'s
  // identical requirement, same evidence chain (`oauthFlow.ts`'s doc comment).
  const existingTokenInfo = getTokenInfo(serverUrl);

  let tokenJson: McpOAuthTokenResult;
  try {
    tokenJson = await refreshMcpOAuthToken({
      projectId: projectId ?? 1,
      token_endpoint: tokenEndpoint,
      refresh_token: refreshToken,
      client_id: clientId,
      client_secret: clientSecret,
      toolkit_id: toolkitId,
      used_dcr: toWireFlag(existingTokenInfo?.used_dcr),
    });
  } catch (error) {
    clearFailedRefresh(serverUrl, existingTokenInfo);
    throw error instanceof Error ? error : new Error('Token refresh failed');
  }

  if (!tokenJson.access_token) {
    throw new Error('No access token received from token refresh');
  }

  requireCurrentAuthorization(serverUrl, existingTokenInfo);

  const sessionId = tokenJson.session_id ?? existingTokenInfo?.session_id ?? generateSessionId();

  setAccessToken(
    canonicalServer,
    tokenJson.access_token,
    tokenJson.expires_in,
    sessionId,
    tokenJson.id_token,
    tokenJson.refresh_token ?? refreshToken,
    {
      token_endpoint: tokenEndpoint,
      client_id: clientId,
      client_secret: clientSecret,
      project_id: projectId === undefined ? undefined : String(projectId),
      toolkit_id: toolkitId,
    },
  );

  return tokenJson;
}

/** Returns a live access token, refreshing first if the stored one is due — falls back to the (possibly stale) existing token if a refresh attempt fails. */
export async function getValidAccessToken(options: RefreshAccessTokenOptions): Promise<string | null> {
  const { serverUrl, tokenEndpoint } = options;

  const accessToken = getAccessToken(serverUrl);
  if (accessToken && !needsRefresh(serverUrl)) {
    return accessToken;
  }

  if (needsRefresh(serverUrl) && tokenEndpoint) {
    try {
      const result = await refreshAccessToken(options);
      return getAccessToken(serverUrl) === result.access_token ? result.access_token : getAccessToken(serverUrl);
    } catch {
      return getAccessToken(serverUrl);
    }
  }

  return accessToken;
}
