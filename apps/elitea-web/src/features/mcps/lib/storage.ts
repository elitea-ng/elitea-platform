/**
 * MCP token/credential/ignored-server storage — port of the storage half of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpAuth.helpers.js
 * (unit A5, spec §9.3). The refresh-QUEUE half (pending-refresh tracking,
 * the background scheduler, `getAllTokens()`'s rate-limited proactive-
 * refresh trigger) lives in `tokenRefresh.ts` — the original file was 594
 * lines, breaching the §3.5 400-line budget; this is the split point the
 * old file's own section comment (`// === Refresh Queue Management ===`,
 * line 301) already marks.
 *
 * Storage backend: old app used raw `window.sessionStorage` directly
 * (`mcAuth.helpers.js:22-31`). This port goes through
 * `shared/lib/storage.ts`'s `createStorage('session')` instead — the ONLY
 * sanctioned place to touch `sessionStorage` (R-A1's storage analogue, spec
 * §5.4) — which also means these keys are swept by `clearNamespace()` on
 * logout, fixing a real old-app leak (MCP OAuth tokens were NOT among the 2
 * keys the old logout cleared; see `shared/api/auth/logout.ts`'s header).
 *
 * ONE field never reaches that storage: `client_secret` (issue #177). It
 * authenticates the application rather than one session and it does not
 * expire, so it is held for the life of the document by
 * `clientSecretVault.ts` and stripped from every record on its way to disk.
 * Read that file before changing anything below that touches the field.
 */
import { createStorage } from '@/shared/lib/storage';

import { forgetClientSecret, rememberClientSecret, rememberGrantClient, stripClientSecrets, withRecalledSecret } from './clientSecretVault';
import { MC_TOKENS_STORAGE_KEY, MCP_CONNECTION_VERIFIED, MCP_CREDENTIALS_STORAGE_KEY, MCP_IGNORED_SERVERS_STORAGE_KEY, MCP_PREBUILD_PREFIX, MCP_TOKEN_CHANGE_EVENT } from './constants';
import { loadLogoutMarker, publishLogout } from './logoutSync';
import type { IgnoredServerMap, SetAccessTokenOAuthMeta, StoredMcpCredential, StoredMcpCredentialMap, StoredMcpToken, StoredMcpTokenMap } from './types';

export type { SetAccessTokenOAuthMeta };

const PROACTIVE_REFRESH_THRESHOLD = 0.75; // Refresh at 75% of token lifetime.

function sessionStore() {
  return createStorage('session');
}

/** Pre-built MCP toolkit types (`mcp_github`, `mcp_context7`, ...) — `'mcp'` alone is a remote MCP, not pre-built. */
export function isPrebuildMcpType(toolkitType: string | undefined | null): boolean {
  return typeof toolkitType === 'string' && toolkitType.startsWith(MCP_PREBUILD_PREFIX) && toolkitType !== 'mcp';
}

/**
 * A credential-scoped composite key has the form `"<uuid>:<url>"` — used to
 * isolate OAuth tokens per credential (e.g. two SharePoint credentials
 * sharing an `oauth_discovery_endpoint`/tenant). Detected by: the key
 * contains `"://"`, AND the segment before the FIRST `"://"` itself
 * contains a `:` (the uuid/url separator) — a plain URL's scheme
 * (`"https"`) never contains `:`.
 */
function isCredentialScopedKey(key: string | undefined | null): key is string {
  if (!key || !key.includes('://')) return false;
  const prefix = key.split('://')[0] ?? '';
  return prefix.includes(':');
}

export function canonicalizeServerUrl(url: string): string {
  if (isCredentialScopedKey(url)) return url;
  try {
    const parsed = new URL(url);
    const scheme = parsed.protocol.replace(':', '').toLowerCase();
    const host = parsed.hostname.toLowerCase();
    const port = parsed.port ? `:${parsed.port}` : '';
    const path = parsed.pathname || '';
    const normalized = `${scheme}://${host}${port}${path}`;
    return normalized.endsWith('/') && (path === '/' || path === '') ? normalized.slice(0, -1) : normalized;
  } catch {
    return url;
  }
}

export interface StorageKeyParams {
  serverUrl?: string | undefined;
  toolkitType?: string | undefined;
}

/**
 * Resolves the record key tokens/credentials are stored under:
 *  1. pre-built MCP -> `toolkitType` verbatim (e.g. `"mcp_github"`).
 *  2. credential-scoped composite key (`"<uuid>:<url>"`) -> as-is, no canonicalisation.
 *  3. remote MCP / config OAuth -> canonicalised `serverUrl`.
 *  4. neither provided -> `null`.
 */
export function getStorageKey({ serverUrl, toolkitType }: StorageKeyParams = {}): string | null {
  if (isPrebuildMcpType(toolkitType)) return toolkitType as string;
  if (serverUrl && isCredentialScopedKey(serverUrl)) return serverUrl;
  if (serverUrl) return canonicalizeServerUrl(serverUrl);
  return null;
}

function resolveChangeKey(keyOrServerUrl: string): string {
  if (isPrebuildMcpType(keyOrServerUrl) || isCredentialScopedKey(keyOrServerUrl)) return keyOrServerUrl;
  return canonicalizeServerUrl(keyOrServerUrl);
}

function dispatchTokenChangeEvent(keyOrServerUrl: string, type: 'login' | 'logout'): void {
  if (typeof window === 'undefined') return;
  const key = resolveChangeKey(keyOrServerUrl);
  window.dispatchEvent(new CustomEvent(MCP_TOKEN_CHANGE_EVENT, { detail: { serverUrl: key, type } }));
}

/* ── raw record accessors ─────────────────────────────────────────────── */

function loadTokens(): StoredMcpTokenMap {
  const stored = sessionStore().getJSON<StoredMcpTokenMap>(MC_TOKENS_STORAGE_KEY) ?? {};
  const tokens = Object.fromEntries(
    Object.entries(stored).filter(([key, token]) => {
      const marker = loadLogoutMarker(key);
      return !marker || Number(token.issued_at) > marker;
    }),
  );
  if (Object.keys(tokens).length !== Object.keys(stored).length) saveTokens(tokens);
  return tokens;
}
function saveTokens(tokens: StoredMcpTokenMap): void {
  sessionStore().setJSON(MC_TOKENS_STORAGE_KEY, stripClientSecrets(tokens));
}
function loadCredentials(): StoredMcpCredentialMap {
  return sessionStore().getJSON<StoredMcpCredentialMap>(MCP_CREDENTIALS_STORAGE_KEY) ?? {};
}
function saveCredentials(credentials: StoredMcpCredentialMap): void {
  sessionStore().setJSON(MCP_CREDENTIALS_STORAGE_KEY, stripClientSecrets(credentials));
}
function loadIgnoredServers(): IgnoredServerMap {
  return sessionStore().getJSON<IgnoredServerMap>(MCP_IGNORED_SERVERS_STORAGE_KEY) ?? {};
}
function saveIgnoredServers(servers: IgnoredServerMap): void {
  sessionStore().setJSON(MCP_IGNORED_SERVERS_STORAGE_KEY, servers);
}

/* ── expiry ────────────────────────────────────────────────────────────── */

export function isExpired(tokenInfo: StoredMcpToken | null | undefined): boolean {
  return Boolean(tokenInfo?.expires_at) && Date.now() > Number(tokenInfo?.expires_at);
}

/** True once the token has crossed 75% of its lifetime (proactive refresh threshold). */
export function needsProactiveRefresh(tokenInfo: StoredMcpToken | null | undefined): boolean {
  if (!tokenInfo?.expires_at || !tokenInfo?.issued_at) return false;
  const expiresAt = Number(tokenInfo.expires_at);
  const issuedAt = Number(tokenInfo.issued_at);
  const totalLifetime = expiresAt - issuedAt;
  const threshold = issuedAt + totalLifetime * PROACTIVE_REFRESH_THRESHOLD;
  return Date.now() > threshold;
}

/* ── token CRUD ────────────────────────────────────────────────────────── */

export function getTokenInfo(serverUrl?: string, toolkitType?: string): StoredMcpToken | null {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return null;
  return withRecalledSecret('token', key, loadTokens()[key] ?? null);
}

export function getSavedCredentials(serverUrl?: string, toolkitType?: string): StoredMcpCredential | null {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return null;
  return withRecalledSecret('credential', key, loadCredentials()[key] ?? null);
}

export interface SetSavedCredentialsParams {
  serverUrl?: string | undefined;
  clientId?: string | undefined;
  clientSecret?: string | undefined;
  toolkitType?: string | undefined;
}

export function setSavedCredentials({ serverUrl, clientId, clientSecret, toolkitType }: SetSavedCredentialsParams = {}): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;
  const credentials = loadCredentials();
  credentials[key] = { client_id: clientId };
  saveCredentials(credentials);
  rememberClientSecret('credential', key, clientSecret);
}

export function removeSavedCredentials(serverUrl?: string, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;
  forgetClientSecret('credential', key);
  const credentials = loadCredentials();
  if (credentials[key]) {
    delete credentials[key];
    saveCredentials(credentials);
  }
}

export function getAccessToken(serverUrl?: string, toolkitType?: string): string | null {
  const tokenInfo = getTokenInfo(serverUrl, toolkitType);
  if (!tokenInfo || isExpired(tokenInfo)) return null;
  return tokenInfo.access_token || null;
}

export function getRefreshToken(serverUrl?: string, toolkitType?: string): string | null {
  return getTokenInfo(serverUrl, toolkitType)?.refresh_token || null;
}

export function getSessionId(serverUrl?: string, toolkitType?: string): string | null {
  const tokenInfo = getTokenInfo(serverUrl, toolkitType);
  if (!tokenInfo || isExpired(tokenInfo)) return null;
  return tokenInfo.session_id || null;
}

/** True when the token is expired OR past its proactive-refresh threshold, AND a refresh_token exists to act on. */
export function needsRefresh(serverUrl?: string, toolkitType?: string): boolean {
  const tokenInfo = getTokenInfo(serverUrl, toolkitType);
  if (!tokenInfo) return false;
  return (isExpired(tokenInfo) || needsProactiveRefresh(tokenInfo)) && Boolean(tokenInfo.refresh_token);
}

export function setSessionId(serverUrl: string | undefined, sessionId: string, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;
  const tokens = loadTokens();
  const existing = tokens[key];
  if (existing) {
    existing.session_id = sessionId;
    saveTokens(tokens);
  }
}

export function setAccessToken(
  serverUrl: string | undefined,
  accessToken: string,
  expiresInSec: number | undefined | null,
  sessionId: string | undefined | null,
  idToken: string | undefined | null,
  refreshToken: string | undefined | null,
  oauthMeta: SetAccessTokenOAuthMeta = {},
  toolkitType?: string,
): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;

  const tokens = loadTokens();
  const logoutMarker = loadLogoutMarker(key);
  const now = Math.max(Date.now(), logoutMarker + 1);
  const expiresAt = expiresInSec ? now + Number(expiresInSec) * 1000 : null;
  const existingToken = tokens[key] ?? ({} as StoredMcpToken);

  function getOrExisting<K extends keyof SetAccessTokenOAuthMeta & keyof StoredMcpToken>(field: K): StoredMcpToken[K] {
    return (oauthMeta[field] as StoredMcpToken[K] | undefined) ?? existingToken[field];
  }

  const clientReference = rememberGrantClient(key, oauthMeta, existingToken);

  tokens[key] = {
    access_token: accessToken,
    authorization_reference: oauthMeta.authorization_reference,
    issued_at: oauthMeta.issued_at === undefined
      ? now
      : Math.max(Number(oauthMeta.issued_at), logoutMarker + 1),
    expires_at: expiresAt,
    ...(sessionId ? { session_id: sessionId } : {}),
    ...(idToken ? { id_token: idToken } : {}),
    ...(refreshToken ? { refresh_token: refreshToken } : {}),
    token_endpoint: getOrExisting('token_endpoint'),
    client_id: getOrExisting('client_id'),
    client_reference: clientReference,
    project_id: getOrExisting('project_id'),
    toolkit_id: getOrExisting('toolkit_id'),
    // Not routed through getOrExisting: toolkitType is a positional
    // parameter of setAccessToken, not part of SetAccessTokenOAuthMeta.
    toolkit_type: toolkitType ?? existingToken.toolkit_type,
    authorization_endpoint: getOrExisting('authorization_endpoint'),
    revocation_endpoint: getOrExisting('revocation_endpoint'),
    registration_endpoint: getOrExisting('registration_endpoint'),
    issuer: getOrExisting('issuer'),
    grant_types_supported: getOrExisting('grant_types_supported'),
    code_challenge_methods_supported: getOrExisting('code_challenge_methods_supported'),
    used_dcr: getOrExisting('used_dcr'),
    resource: getOrExisting('resource'),
  };

  saveTokens(tokens);
  dispatchTokenChangeEvent(key, 'login');

  const ignoredServers = loadIgnoredServers();
  if (ignoredServers[key]) {
    delete ignoredServers[key];
    saveIgnoredServers(ignoredServers);
  }
}

export function logout(serverUrl?: string, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;
  forgetClientSecret('token', key);
  const tokens = loadTokens();
  if (tokens[key]) {
    delete tokens[key];
    saveTokens(tokens);
  }
  publishLogout(key);
  dispatchTokenChangeEvent(key, 'logout');
}

/**
 * Marks a server/toolkit "connection verified" for header-based-auth MCP
 * servers that need no client-side OAuth token — makes `getAccessToken`
 * truthy (triggering `useMcpTokenChange` UI updates) without overwriting a
 * real OAuth token that may already be stored.
 */
export function setConnectionVerified(serverUrl: string | undefined, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (!key) return;
  if (getAccessToken(serverUrl, toolkitType)) return;

  const tokens = loadTokens();
  const now = Date.now();
  tokens[key] = {
    access_token: MCP_CONNECTION_VERIFIED,
    issued_at: now,
    expires_at: now + 24 * 60 * 60 * 1000,
    connection_verified: true,
    ...(toolkitType ? { toolkit_type: toolkitType } : {}),
  };
  saveTokens(tokens);
  dispatchTokenChangeEvent(key, 'login');

  const ignoredServers = loadIgnoredServers();
  if (ignoredServers[key]) {
    delete ignoredServers[key];
    saveIgnoredServers(ignoredServers);
  }
}

/* ── ignored-servers list (user chose "continue without auth") ──────────── */

export function addIgnoredServer(serverUrl: string): void {
  const ignoredServers = loadIgnoredServers();
  const key = canonicalizeServerUrl(serverUrl);
  ignoredServers[key] = { ignored_at: Date.now(), server_url: serverUrl };
  saveIgnoredServers(ignoredServers);
}

export function removeIgnoredServer(serverUrl: string): void {
  const ignoredServers = loadIgnoredServers();
  const key = canonicalizeServerUrl(serverUrl);
  if (ignoredServers[key]) {
    delete ignoredServers[key];
    saveIgnoredServers(ignoredServers);
  }
}

export function isServerIgnored(serverUrl: string): boolean {
  const ignoredServers = loadIgnoredServers();
  return Boolean(ignoredServers[canonicalizeServerUrl(serverUrl)]);
}

export function getIgnoredServers(): string[] {
  return Object.keys(loadIgnoredServers());
}

export function clearIgnoredServers(): void {
  saveIgnoredServers({});
}

/**
 * Union of (a) explicitly-ignored servers that still lack a valid token and
 * (b) any server from `mcpServerUrls` lacking a valid token — deduplicated
 * by canonical URL.
 */
export function getFilteredIgnoredServers(mcpServerUrls: readonly string[] = []): string[] {
  const authenticatedServerUrls = Object.keys(loadTokens());
  const allIgnoredServerUrls = Object.keys(loadIgnoredServers());
  if (allIgnoredServerUrls.length === 0) return [];

  const ignoredWithoutTokens = allIgnoredServerUrls.filter(
    (serverUrl) =>
      !authenticatedServerUrls.some((authUrl) => canonicalizeServerUrl(authUrl) === canonicalizeServerUrl(serverUrl)),
  );

  const mcpServersWithoutValidTokens = mcpServerUrls.filter((serverUrl) => serverUrl && getAccessToken(serverUrl) === null);

  const allIgnored = [...ignoredWithoutTokens];
  for (const url of mcpServersWithoutValidTokens) {
    const canonicalUrl = canonicalizeServerUrl(url);
    if (!allIgnored.some((existing) => canonicalizeServerUrl(existing) === canonicalUrl)) {
      allIgnored.push(url);
    }
  }
  return allIgnored;
}

/**
 * MCP server URLs (from `mcpServerUrls`) that lack a valid access token —
 * does NOT consult the ignored-servers list (unlike `getFilteredIgnoredServers`).
 */
export function getServersWithoutTokens(mcpServerUrls: readonly string[] = []): string[] {
  return mcpServerUrls.filter((serverUrl) => serverUrl && getAccessToken(serverUrl) === null);
}

/* ── internal accessor re-exported for tokenRefresh.ts's split half ─────── */
/* `saveTokens` has no cross-module consumer (only used internally, above) — */
/* not re-exported, so it doesn't carry a dead `_saveTokens` alias (knip).   */

export { loadTokens as _loadTokens };
