import { createStorage } from '@/shared/lib/storage';
import { loadLogoutMarker, publishLogout } from '@/shared/lib/oauthLogoutSync';

/**
 * `mcpTokenStorage.helpers.ts` — a DISCLOSED, INTENTIONALLY PARTIAL local
 * duplicate of `apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/
 * mcpAuth.helpers.js`'s sessionStorage-backed OAuth-token primitives, ported
 * only as far as SharePoint's own two consumers
 * (`SharepointDelegatedLoginButton.jsx`/`SharepointOAuthStatus.jsx`) ever
 * call: `getAccessToken`, `logout`, `setConnectionVerified`, and the
 * `getStorageKey`/`canonicalizeServerUrl` key-resolution helpers underneath
 * them.
 *
 * WHY A LOCAL DUPLICATE, NOT AN IMPORT: `features/mcps` (this app's real,
 * fully-built port of `features/mcp` — see its own `src/features/mcps/
 * index.ts`) already implements this exact storage layer
 * (`src/features/mcps/lib/storage.ts`) plus the full OAuth PKCE/discovery
 * flow around it. `no-sideways-features` (`.dependency-cruiser.cjs`)
 * forbids `features/toolkits` importing `features/mcps` outright — "no
 * carve-out" per this batch's own mission brief. Sharepoint folds INTO
 * `features/toolkits` (this file's own directory), so it inherits that
 * same restriction.
 *
 * WHY THIS SPECIFIC SUBSET COUNTS AS "THIN" (the mission brief's own
 * bar — "duplicate the thin pieces you need, disclose the rest"): every
 * function below is a PURE `sessionStorage` read/write with no network
 * request, no popup window, no PKCE/crypto, no discovery-metadata fetch —
 * unlike `mcpAuthWindow.helpers.js`/`mcpDiscovery.helpers.js`/
 * `mcpCrypto.helpers.js`/`mcpClientFactory.helpers.js` (the OAuth FLOW
 * itself), which stay `features/mcps`-owned and are NOT duplicated here.
 * The refresh-queue/scheduler/ignored-servers machinery in the baseline
 * file is ALSO not ported: grepped, neither `SharepointDelegatedLoginButton`
 * nor `SharepointOAuthStatus` ever calls `markRefreshPending`/
 * `getServersNeedingRefresh`/`addIgnoredServer`/`startTokenRefreshScheduler`
 * or any of that family — only token presence (`getAccessToken`), removal
 * (`logout`), and the "verified without a real OAuth token" marker
 * (`setConnectionVerified`, used for the non-delegated/header-auth case in
 * `SharepointOAuthStatus.tsx`).
 *
 * Ported from `mcpAuth.helpers.js` for the functions kept (same
 * composite-key detection, same `MCP_CONNECTION_VERIFIED` sentinel and 24h
 * expiry) — this is a narrower SLICE of that file, not a redesign of the
 * logic it does contain. The one deliberate deviation from the baseline is
 * the storage BACKEND, for the security reasons set out immediately below.
 */

/**
 * STORAGE BACKEND (issue #22). This module used to write `sessionStorage`
 * directly under the baseline's raw key `'mcp_oauth_tokens'`. Two problems,
 * both fixed here by routing through `shared/lib/storage.ts`'s
 * `createStorage('session')` — the ONLY sanctioned `sessionStorage` accessor
 * (spec §5.4) — exactly as `features/mcps/lib/storage.ts` already does:
 *
 *  1. **Tokens survived logout.** `performLogout()` sweeps the `el.*`
 *     namespace via `clearNamespace()`; a raw, un-namespaced key is not in
 *     that namespace, so a SharePoint OAuth access token outlived sign-out
 *     and was inherited by the next user of the tab. §5.4's completeness
 *     test could not see it either: that test enumerates writes made THROUGH
 *     the wrapper, so a write that bypasses the wrapper is invisible to it.
 *  2. **It bypassed the R-A1/§5.4 lint fence.** `no-restricted-globals`
 *     bans the bare `sessionStorage` global, but this file reached it as
 *     `window.sessionStorage`, a member expression the rule does not match.
 *
 * The LOGICAL key is deliberately the same one `features/mcps/lib/constants.ts`
 * uses (`'mcp.tokens'`, i.e. `el.mcp.tokens` on the wire), not a SharePoint-
 * private one. SharePoint's delegated-login flow obtains its token from
 * `features/mcps`' real `<McpAuthModal>` (see `useSharepointAuthModal.hooks.ts`),
 * which writes through that feature's own storage layer — divergent keys
 * would mean this module could never read the token that flow produces. The
 * literal is duplicated rather than imported because `no-sideways-features`
 * forbids `features/toolkits` importing `features/mcps`; that is the same
 * disclosed-duplication trade-off the rest of this file documents.
 *
 * This is NOT encryption, and nothing here claims to be: see issue #22's PR
 * for why browser-local encryption of a browser-readable token buys nothing
 * against an attacker who already has script execution on this origin.
 */
const MC_TOKENS_STORAGE_KEY = 'mcp.tokens';
/**
 * `features/mcps/lib/constants.ts`'s `MCP_TOKEN_CHANGE_EVENT`, NOT the
 * baseline's `'mcp-token-change'` — for exactly the reason the storage-key
 * note above gives, one level further on. Issue #22 aligned WHERE the two
 * slices store the token; this aligns HOW they announce a change to it.
 *
 * `useSharepointTokenStatus` re-reads the token only when it hears this
 * event, and the real writer (`features/mcps/lib/storage.ts`, driven by
 * `<McpAuthModal>`) dispatches `elitea_mcp_token_change`. Listening for the
 * baseline's name meant a completed OAuth login wrote a token this module
 * could now read — and the UI still sat on its stale "not connected" render
 * until something else forced it to re-check.
 *
 * Duplicated by literal rather than imported, same `no-sideways-features`
 * trade-off as `MC_TOKENS_STORAGE_KEY`. `mcpTokenStorage.interop.test.ts`
 * pins both names against the real `features/mcps` writer, so drift in
 * either fails a test instead of silently disconnecting the feature.
 */
export const MCP_TOKEN_CHANGE_EVENT = 'elitea_mcp_token_change';
/** `mcAuth.constants.js`'s `MCP_CONNECTION_VERIFIED` sentinel access-token value. */
const MCP_CONNECTION_VERIFIED = '__connection_verified__';
/** `mcAuth.constants.js`'s `MCP_PREBUILD_PREFIX` — checked so `getStorageKey` matches the baseline's precedence order, even though SharePoint's own `type` ('sharepoint') is never prebuild-prefixed. */
const MCP_PREBUILD_PREFIX = 'mcp_';

interface StoredToken {
  readonly access_token?: string;
  readonly issued_at?: number;
  readonly expires_at?: number | null;
  readonly connection_verified?: boolean;
  readonly toolkit_type?: string;
  readonly [key: string]: unknown;
}

type TokenStore = Record<string, StoredToken>;

/**
 * Resolved lazily per call (not captured at module scope) so importing this
 * module never touches `window` — matching `features/mcps/lib/storage.ts`.
 *
 * The try/catch replaces the old `window.sessionStorage !== undefined` probe:
 * it covers the same "no web storage here" case (SSR, and the vitest `node`
 * project where `window` exists but its storage areas do not) without reaching
 * for the raw global the §5.4 fence bans.
 */
function loadTokens(): TokenStore {
  try {
    // `getJSON` already treats absent AND malformed JSON as absent, which is
    // what the baseline's own try/catch did.
    const stored = createStorage('session').getJSON<TokenStore>(MC_TOKENS_STORAGE_KEY) ?? {};
    const tokens = Object.fromEntries(Object.entries(stored).filter(([key, token]) => {
      const marker = loadLogoutMarker(key);
      return !marker || Number(token.issued_at) > marker;
    }));
    if (Object.keys(tokens).length !== Object.keys(stored).length) saveTokens(tokens);
    return tokens;
  } catch {
    return {};
  }
}

function saveTokens(tokens: TokenStore): void {
  try {
    createStorage('session').setJSON(MC_TOKENS_STORAGE_KEY, tokens);
  } catch {
    // Ignore storage errors (no web storage, quota exceeded, etc) — same as the baseline.
  }
}

/** `mcpAuth.helpers.js`'s `isPrebuildMcpType` — kept for `getStorageKey`'s precedence order, though SharePoint never actually hits this branch. */
export function isPrebuildMcpType(toolkitType: string | undefined): boolean {
  return typeof toolkitType === 'string' && toolkitType.startsWith(MCP_PREBUILD_PREFIX) && toolkitType !== 'mcp';
}

/** `mcpAuth.helpers.js`'s `isCredentialScopedKey` — a `"<uuid>:<url>"` composite key (SharePoint's own `getSharepointConnectionTokenKey` output shape, `../helpers/token.helpers.ts`). */
function isCredentialScopedKey(key: string | undefined): boolean {
  if (key === undefined || key === '' || !key.includes('://')) return false;
  const prefix = key.split('://')[0] ?? '';
  return prefix.includes(':');
}

/** `mcpAuth.helpers.js`'s `canonicalizeServerUrl`. */
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

export interface GetStorageKeyInput {
  readonly serverUrl?: string | undefined;
  readonly toolkitType?: string | undefined;
}

/** `mcpAuth.helpers.js`'s `getStorageKey`. */
export function getStorageKey({ serverUrl, toolkitType }: GetStorageKeyInput): string | null {
  if (isPrebuildMcpType(toolkitType)) return toolkitType as string;
  if (serverUrl && isCredentialScopedKey(serverUrl)) return serverUrl;
  if (serverUrl) return canonicalizeServerUrl(serverUrl);
  return null;
}

function isExpired(tokenInfo: StoredToken | undefined): boolean {
  return Boolean(tokenInfo?.expires_at) && Date.now() > Number(tokenInfo?.expires_at);
}

function dispatchTokenChangeEvent(key: string, type: 'login' | 'logout'): void {
  if (typeof window === 'undefined') return;
  const event = new CustomEvent(MCP_TOKEN_CHANGE_EVENT, { detail: { serverUrl: key, type } });
  window.dispatchEvent(event);
}

/** `mcpAuth.helpers.js`'s `getAccessToken`. */
export function getAccessToken(serverUrl: string | undefined, toolkitType?: string): string | null {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (key === null) return null;
  const tokenInfo = loadTokens()[key];
  if (!tokenInfo || isExpired(tokenInfo)) return null;
  return tokenInfo.access_token ?? null;
}

/** `mcpAuth.helpers.js`'s `logout`. */
export function logout(serverUrl: string | undefined, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (key === null) return;
  const tokens = loadTokens();
  if (tokens[key]) {
    const next = { ...tokens };
    delete next[key];
    saveTokens(next);
  }
  publishLogout(key);
  dispatchTokenChangeEvent(key, 'logout');
}

/**
 * `mcpAuth.helpers.js`'s `setConnectionVerified` — marks a header-based-auth
 * connection (no real OAuth token, e.g. SharePoint's non-delegated case) as
 * verified so `getAccessToken` returns truthy and `useSharepointTokenStatus`
 * flips to "connected".
 */
export function setConnectionVerified(serverUrl: string | undefined, toolkitType?: string): void {
  const key = getStorageKey({ serverUrl, toolkitType });
  if (key === null) return;
  if (getAccessToken(serverUrl, toolkitType) !== null) return;
  const tokens = loadTokens();
  const now = Date.now();
  tokens[key] = {
    access_token: MCP_CONNECTION_VERIFIED,
    issued_at: now,
    expires_at: now + 24 * 60 * 60 * 1000,
    connection_verified: true,
    ...(toolkitType !== undefined && { toolkit_type: toolkitType }),
  };
  saveTokens(tokens);
  dispatchTokenChangeEvent(key, 'login');
}
