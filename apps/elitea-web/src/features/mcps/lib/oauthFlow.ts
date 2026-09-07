/**
 * User-initiated OAuth authorization-code flow — port of
 * `startMcpAuthFlow` from
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpAuthFlow.helpers.js
 * (unit A5, spec §9.3). The background-refresh half lives in
 * `tokenLifecycle.ts` (split rationale there).
 *
 * `getRedirectUri()` deviates from the baseline's `RouteDefinitions.McpAuthPage`
 * + `getBasename()` (both live in `src/app/**`/`src/routes/**`, which
 * `features/` may not import — R-L1, layers flow strictly downward). The
 * `/mcp-auth-callback` path is a literal (matches `src/routes/_shell/
 * mcp-auth-callback.tsx`'s mounted pattern exactly — spec ROUTE-050) and the
 * basename is re-derived from `shared/config` using the SAME two-branch
 * logic `src/app/providers/basename.ts`'s `getAppBasename()` encodes
 * (`import.meta.env.DEV` ? '' : `config.vite_base_uri`) — both read the
 * identical source of truth, just from a layer this slice is allowed to
 * import.
 */
import { getConfig } from '@/shared/config';
import { normalizeBasename } from '@/shared/lib/basename';

import { exchangeMcpOAuthToken } from '../api/mcpOAuthClient';
import type { McpOAuthTokenResponse } from '../api/mcpOAuthClient';

import { MCP_OAUTH_ERRORS } from './constants';
import { normalizeScope, randomString, sha256, isOIDCFlow } from './crypto';
import { extractAuthServerMetadata } from './discoveryMetadata';
import { extractOAuthErrorDetail, registerDynamicClient } from './registerDynamicClient';
import { isPrebuildMcpType, setAccessToken } from './storage';
import type { OAuthServerMetadata } from './types';
import { createAuthorizationMonitor, navigateAuthPopup, openAuthPopup } from './window';

const MCP_AUTH_CALLBACK_PATH = '/mcp-auth-callback';

/**
 * The basename, normalized so the join below cannot make an empty path
 * segment. `vite_base_uri` ships as `/app/`, and the raw value produced
 * `https://host/app//mcp-auth-callback`. An authorization server compares the
 * `redirect_uri` as a simple string (RFC 6749 3.1.2.3). That URI therefore
 * never matched a callback registered as
 * `https://host/app/mcp-auth-callback`. The flow ended in
 * `redirect_uri_mismatch`.
 */
function getMcpAuthCallbackBasename(): string {
  if (import.meta.env.DEV) return '';
  const result = getConfig();
  return result.status === 'ok' ? normalizeBasename(result.config.vite_base_uri) : '';
}

function getRedirectUri(): string {
  const baseUrl = `${window.location.protocol}//${window.location.host}`;
  return `${baseUrl}${getMcpAuthCallbackBasename()}${MCP_AUTH_CALLBACK_PATH}`;
}

interface AuthorizationUrlOptions {
  resource?: string | undefined;
  authorizationEndpoint: string;
  clientId: string;
  redirectUri: string;
  state: string;
  nonce: string;
  codeChallenge?: string | undefined;
  usePKCE: boolean;
  scope?: string | undefined;
  isOIDC: boolean;
  prompt?: string | undefined;
}

function buildAuthorizationUrl(options: AuthorizationUrlOptions): string {
  const { authorizationEndpoint, clientId, redirectUri, state, nonce, codeChallenge, usePKCE, scope, isOIDC, prompt } = options;

  const params = new URLSearchParams({ response_type: 'code', client_id: clientId, redirect_uri: redirectUri, state });

  if (usePKCE && codeChallenge) {
    params.set('code_challenge', codeChallenge);
    params.set('code_challenge_method', 'S256');
  }
  if (isOIDC) params.set('nonce', nonce);
  if (scope) params.set('scope', scope);
  if (prompt) params.set('prompt', prompt);
  if (options.resource) params.set('resource', options.resource);

  return `${authorizationEndpoint}?${params.toString()}`;
}

function waitForAuthorizationResult(authWindow: Window, state: string): Promise<{ code?: string }> {
  return new Promise((resolve, reject) => {
    createAuthorizationMonitor(authWindow, state, resolve, reject);
  });
}

/**
 * The phases below split `startMcpAuthFlow`'s single-function body (§3.5
 * complexity budget: the inlined form measured 27) into one function per
 * flow step — popup acquisition, client-credential resolution, PKCE
 * material, the redirect-and-await, the token exchange, and the persisted
 * metadata shape. Each keeps its own well-under-budget complexity; the
 * orchestrating function just sequences them.
 */

/** Uses the caller-supplied window, or opens a fresh popup — throws `POPUP_BLOCKED` if that fails (baseline: browsers blocking a popup not opened synchronously from a click). */
function ensureAuthWindow(providedWindow: Window | null | undefined): Window {
  if (providedWindow) return providedWindow;
  const opened = openAuthPopup();
  if (!opened) {
    throw new Error(`${MCP_OAUTH_ERRORS.POPUP_BLOCKED}. Please allow popups for this site and try again.`);
  }
  return opened;
}

interface ClientCredentialsResolution {
  clientId: string;
  /** The DCR-issued secret, when DCR was used and the server issued one — `undefined` on the caller-provided-`client_id` branch, where there is no DCR-issued secret at all. See `startMcpAuthFlow`'s `resolveEffectiveClientSecret` for how this combines with a caller-supplied secret. */
  clientSecret: string | undefined;
  usedDCR: boolean;
}

/**
 * A caller-provided `client_id` wins outright; otherwise Dynamic Client
 * Registration if the server supports it; otherwise the flow cannot
 * proceed.
 *
 * `registerDynamicClient` returns `{clientId, clientSecret}` (baseline
 * post-`6ebe8ff7` — "fix: [EL-5697] Aha! mcp token issue": some servers,
 * e.g. Aha!, issue a `client_secret` alongside the `client_id` even for a
 * `token_endpoint_auth_method: none` request). That DCR-issued secret is
 * forwarded here and combined with the caller-supplied one in
 * `startMcpAuthFlow`'s `resolveEffectiveClientSecret` — the DCR-issued
 * secret wins whenever DCR was used, never a caller-supplied
 * pre-configured developer-app secret (that belongs to a different OAuth
 * client and would itself cause "unknown client"), matching the real
 * upstream commit's own `dcrClientSecret`/`effectiveClientSecret` split.
 */
async function resolveClientCredentials(
  registrationEndpoint: string | undefined,
  initialClientId: string | undefined,
  projectId: string | number | undefined,
): Promise<ClientCredentialsResolution> {
  if (initialClientId) return { clientId: initialClientId, clientSecret: undefined, usedDCR: false };
  if (registrationEndpoint) {
    const redirectUri = getRedirectUri();
    const { clientId, clientSecret } = await registerDynamicClient(registrationEndpoint, redirectUri, projectId);
    return { clientId, clientSecret, usedDCR: true };
  }
  throw new Error(
    `${MCP_OAUTH_ERRORS.MISSING_CLIENT_ID}. Server does not support Dynamic Client Registration. Please register an OAuth application manually and provide client credentials.`,
  );
}

/** DCR-issued secret wins whenever DCR was used; otherwise the caller-supplied one — never merged/fall-through between the two (see `resolveClientCredentials`'s own doc comment for why). Split out purely to keep `startMcpAuthFlow`'s own cyclomatic complexity under the §3.5 budget. */
function resolveEffectiveClientSecret(usedDCR: boolean, dcrClientSecret: string | undefined, providedClientSecret: string | undefined): string | undefined {
  return usedDCR ? dcrClientSecret : providedClientSecret;
}

interface PkceMaterial {
  usePKCE: boolean;
  codeVerifier: string | undefined;
  codeChallenge: string | undefined;
}

/** PKCE is used whenever the server advertises S256 support, or whenever no `client_secret` was supplied (a public client has no other way to prove possession). */
async function resolvePkceMaterial(asMetadata: OAuthServerMetadata, clientSecret: string | undefined): Promise<PkceMaterial> {
  const serverSupportsPKCE = asMetadata.code_challenge_methods_supported?.includes('S256') ?? false;
  const usePKCE = serverSupportsPKCE || !clientSecret;
  if (!usePKCE) return { usePKCE, codeVerifier: undefined, codeChallenge: undefined };
  const codeVerifier = randomString(64);
  const codeChallenge = await sha256(codeVerifier);
  return { usePKCE, codeVerifier, codeChallenge };
}

/** Navigates the popup to the authorization URL and resolves with the redirected `code` — throws if the callback carried none (an OAuth error, or a malformed redirect). */
async function awaitAuthorizationCode(authWindow: Window, authUrl: string, state: string): Promise<string> {
  navigateAuthPopup(authWindow, authUrl);
  const result = await waitForAuthorizationResult(authWindow, state);
  if (!result.code) {
    throw new Error('No authorization code received from popup');
  }
  return result.code;
}

interface ExchangeAuthorizationCodeParams {
  resource: string | undefined;
  projectId: string | number | undefined;
  tokenEndpoint: string | undefined;
  code: string;
  redirectUri: string;
  clientId: string;
  clientSecret: string | undefined;
  usedDCR: boolean;
  isPrebuildMcp: boolean;
  usePKCE: boolean;
  codeVerifier: string | undefined;
  normalizedScope: string | undefined;
  toolkitId: string | undefined;
  toolkitType: string | undefined;
}

/** `true || undefined` normalization for a wire field, split into its own function purely to keep the caller's own cyclomatic-complexity count under the §3.5 budget (12) — see `used_dcr`'s own doc comment at each call site for why this must be sent. */
function toWireFlag(used: boolean | undefined): boolean | undefined {
  return used || undefined;
}

/** Trades the authorization code for a token — credentials (`client_id`/`client_secret`) are sent only when DCR issued them or the toolkit isn't a pre-built MCP (baseline: pre-built MCPs with backend-resolved credentials must not leak them client-side). */
async function exchangeAuthorizationCode(params: ExchangeAuthorizationCodeParams): Promise<McpOAuthTokenResponse> {
  const shouldSendCredentials = params.usedDCR || !params.isPrebuildMcp;

  let tokenJson: McpOAuthTokenResponse;
  try {
    tokenJson = await exchangeMcpOAuthToken({
      projectId: params.projectId ?? 1,
      token_endpoint: params.tokenEndpoint,
      code: params.code,
      redirect_uri: params.redirectUri,
      client_id: shouldSendCredentials ? (params.clientId ?? undefined) : undefined,
      client_secret: shouldSendCredentials ? params.clientSecret : undefined,
      code_verifier: params.usePKCE ? params.codeVerifier : undefined,
      scope: params.normalizedScope || undefined,
      resource: params.resource,
      toolkit_id: params.toolkitId,
      toolkit_type: params.isPrebuildMcp ? params.toolkitType : undefined,
      // MUST be sent, not merely persisted locally — corrects a prior pass
      // here that removed it, reasoning the pinned baseline snapshot
      // (`apps/elitea-ui` submodule, `a55f36cf`) has no such field and no
      // backend route reads it. That reasoning was wrong on both counts:
      // (1) `a55f36cf` predates the real upstream fix, `frontends/EliteaUI`
      // commit `6ebe8ff7` ("fix: [EL-5697] Aha! mcp token issue"), which
      // adds exactly this field to the request body; (2) the CURRENTLY
      // RUNNING legacy pylon backend
      // (`legacy/plugins/elitea_core/api/v2/mcp_oauth_proxy.py`) DOES read
      // it — `if not client_secret and not data.used_dcr: client_secret =
      // settings.get('client_secret') or ...` (loads a DB-configured
      // client_secret for the toolkit). Omitting `used_dcr` here makes the
      // backend load and send a DB client_secret for what is actually a
      // DCR-registered PUBLIC client, which some providers (Aha! and
      // others, per the fix's own commit message) reject with "unknown
      // client" — the exact bug class `registerDynamicClient.ts`'s
      // sibling `client_secret`-preservation fix (finding 1,
      // A5-api-pages) exists to prevent, reintroduced here via the wire
      // omission instead.
      used_dcr: toWireFlag(params.usedDCR),
    });
  } catch (cause) {
    // Baseline: `mcpAuthFlow.helpers.js:446-448` — `errorData.error_description
    // || errorData.error || 'Token exchange failed'`. Before this fix, a
    // failed exchange's `EliteaApiError` (a generic "eliteaFetch: 400 from
    // ..." message) propagated as-is, losing the OAuth server's specific
    // reason (e.g. `invalid_grant: code already used`) that the modal
    // should show the user.
    throw new Error(extractOAuthErrorDetail(cause) ?? 'Token exchange failed', { cause });
  }

  if (!tokenJson.access_token) {
    throw new Error('No access token received from token exchange');
  }
  return tokenJson;
}

interface TokenPersistenceMetadataParams {
  resource: string | undefined;
  tokenEndpoint: string | undefined;
  clientId: string | undefined;
  clientSecret: string | undefined;
  projectId: string | number | undefined;
  toolkitId: string | undefined;
  providedOauthMetadata: Partial<OAuthServerMetadata> | null | undefined;
  usedDCR: boolean;
}

/** The extra fields `setAccessToken` persists alongside the token itself, so a later proactive refresh (`tokenLifecycle.ts`) has everything it needs without re-discovering the server. */
function buildTokenPersistenceMetadata(params: TokenPersistenceMetadataParams) {
  return {
    resource: params.resource,
    token_endpoint: params.tokenEndpoint,
    client_id: params.clientId,
    client_secret: params.clientSecret,
    project_id: params.projectId === undefined ? undefined : String(params.projectId),
    toolkit_id: params.toolkitId,
    // Retain the issued client's ownership. A later refresh must not replace
    // DCR credentials with the toolkit's unrelated stored OAuth client.
    used_dcr: params.usedDCR || undefined,
    ...(params.providedOauthMetadata
      ? {
          authorization_endpoint: params.providedOauthMetadata.authorization_endpoint,
          revocation_endpoint: params.providedOauthMetadata.revocation_endpoint,
          registration_endpoint: params.providedOauthMetadata.registration_endpoint,
          issuer: params.providedOauthMetadata.issuer,
          grant_types_supported: params.providedOauthMetadata.grant_types_supported,
          code_challenge_methods_supported: params.providedOauthMetadata.code_challenge_methods_supported,
        }
      : {}),
  };
}

export interface StartMcpAuthFlowOptions {
  serverUrl?: string | undefined;
  /** Protected MCP resource, independent of the credential-scoped storage key. */
  resourceUrl?: string | undefined;
  resourceMetadata: { authorization_servers?: readonly string[] | undefined; oauth_authorization_server?: OAuthServerMetadata | undefined };
  oauthMetadata?: Partial<OAuthServerMetadata> | null | undefined;
  clientId?: string | undefined;
  clientSecret?: string | undefined;
  scope?: string | undefined;
  authWindow?: Window | null | undefined;
  projectId?: string | number | undefined;
  toolkitId?: string | undefined;
  /** Pre-built MCP type (e.g. `mcp_github`) — used as the storage key when set. */
  toolkitType?: string | undefined;
}

/**
 * Full authorization-code + PKCE flow: resolve auth-server metadata ->
 * (DCR or require a caller-provided client_id) -> open/navigate the popup
 * -> await the redirected code -> exchange it -> persist the token.
 * Throws on any step failure (POPUP_BLOCKED, MISSING_CLIENT_ID, a rejected
 * exchange, ...) — the caller (an OAuth modal) surfaces this as an inline
 * error, matching the baseline.
 */
export async function startMcpAuthFlow(options: StartMcpAuthFlowOptions): Promise<{ access_token: string; expires_in?: number; session_id?: string; id_token?: string; refresh_token?: string }> {
  const { serverUrl, resourceMetadata, oauthMetadata: providedOauthMetadata, clientId: initialClientId, clientSecret, scope, authWindow: initialAuthWindow, projectId, toolkitId, toolkitType } = options;

  const isPrebuildMcp = isPrebuildMcpType(toolkitType);
  if (!serverUrl && !isPrebuildMcp) {
    throw new Error('Missing MCP server URL');
  }

  const authWindow = ensureAuthWindow(initialAuthWindow);

  const asMetadata = extractAuthServerMetadata(resourceMetadata);
  const { authorization_endpoint: authorizationEndpoint, token_endpoint: tokenEndpoint, registration_endpoint: registrationEndpoint } = asMetadata;

  const { clientId, clientSecret: dcrClientSecret, usedDCR } = await resolveClientCredentials(registrationEndpoint, initialClientId, projectId);
  const effectiveClientSecret = resolveEffectiveClientSecret(usedDCR, dcrClientSecret, clientSecret);
  const resource = options.resourceUrl ?? (usedDCR ? serverUrl : undefined);

  const state = randomString(32);
  const nonce = randomString(32);
  const redirectUri = getRedirectUri();
  const isOIDC = isOIDCFlow(asMetadata);

  // Deliberately the ORIGINAL caller-supplied `clientSecret`, not
  // `effectiveClientSecret` — matches the real upstream commit's own
  // behaviour (verified by reading its diff): PKCE necessity is judged
  // against whether the CALLER configured a confidential client, not
  // against whatever DCR happened to issue.
  const { usePKCE, codeVerifier, codeChallenge } = await resolvePkceMaterial(asMetadata, clientSecret);
  const normalizedScope = normalizeScope(scope, isOIDC);

  const authUrl = buildAuthorizationUrl({
    resource,
    authorizationEndpoint: authorizationEndpoint as string,
    clientId,
    redirectUri,
    state,
    nonce,
    codeChallenge,
    usePKCE,
    scope: normalizedScope,
    isOIDC,
    prompt: isOIDC ? 'consent' : undefined,
  });

  const code = await awaitAuthorizationCode(authWindow, authUrl, state);

  const tokenJson = await exchangeAuthorizationCode({
    resource,
    projectId,
    tokenEndpoint,
    code,
    redirectUri,
    clientId,
    clientSecret: effectiveClientSecret,
    usedDCR,
    isPrebuildMcp,
    usePKCE,
    codeVerifier,
    normalizedScope,
    toolkitId,
    toolkitType,
  });

  const sessionId = tokenJson.session_id ?? undefined;

  setAccessToken(
    serverUrl,
    tokenJson.access_token,
    tokenJson.expires_in,
    sessionId,
    tokenJson.id_token,
    tokenJson.refresh_token,
    buildTokenPersistenceMetadata({ resource, tokenEndpoint, clientId, clientSecret: effectiveClientSecret, projectId, toolkitId, providedOauthMetadata, usedDCR }),
    toolkitType,
  );

  return tokenJson;
}
