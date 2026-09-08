/**
 * Dynamic Client Registration (DCR) — port of the network-calling half of
 * apps/elitea-ui/src/[fsd]/features/mcp/lib/helpers/mcpDiscovery.helpers.js
 * (unit A5, manifest API-166). The pure metadata extractors from the same
 * baseline file live in `discoveryMetadata.ts`; split so this file (the one
 * that calls the network) stays a single, obviously-side-effecting unit.
 *
 * `client_secret` propagation: the baseline file itself shipped this exact
 * bug and was fixed upstream in `frontends/EliteaUI` commit `6ebe8ff7`
 * ("fix: [EL-5697] Aha! mcp token issue", PR #489, `git log --oneline -- '
 * src/[fsd]/features/mcp/lib/helpers/mcpDiscovery.helpers.js'` in that repo)
 * — some authorization servers (e.g. Aha!) issue a `client_secret` in the
 * DCR response even for a `token_endpoint_auth_method: none` request, and
 * the subsequent token exchange is rejected as "unknown client" unless that
 * secret is echoed back. Main now stores that secret and returns an opaque
 * reference. Code and refresh grants retain the registered client identity.
 *
 * `oauthFlow.ts`'s `resolveClientCredentials`/`resolveEffectiveClientSecret`
 * thread this reference through token exchange and token metadata. Legacy
 * responses retain secrets in document memory only, never falling back to a
 * caller-supplied pre-configured developer-app secret there, since that
 * belongs to a different OAuth client and would itself cause "unknown
 * client"), mirroring `mcpAuthFlow.helpers.js`'s own `dcrClientSecret`/
 * `effectiveClientSecret` split from the same upstream commit. See
 * `oauthFlow.ts`'s own doc comments for the full detail — this is no longer
 * a follow-up, it's wired end-to-end.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';

import { registerMcpDynamicClient } from '../api/mcpOAuthClient';

import { MCP_OAUTH_ERRORS } from './constants';

const DCR_REQUEST_DEFAULTS = {
  token_endpoint_auth_method: 'none',
  grant_types: ['authorization_code', 'refresh_token'],
  response_types: ['code'],
  client_name: 'ELITEA MCP Client',
  application_type: 'web',
} as const;

/** The RFC 6749 §5.2 / RFC 7591 §3.2.2 shape an OAuth/DCR error response body carries. */
interface OAuthErrorBody {
  error?: string;
  error_description?: string;
}

function isOAuthErrorBody(value: unknown): value is OAuthErrorBody {
  return typeof value === 'object' && value !== null;
}

/**
 * Extracts the OAuth server's own `error_description`/`error` text from a
 * failed proxy call — baseline: `mcpDiscovery.helpers.js:91-95`'s /
 * `mcpAuthFlow.helpers.js:445-448`'s `const errorData = result.error.data ||
 * result.error; errorData.error_description || errorData.error`. Both
 * baseline call sites read RTK Query's `result.error.data` (the parsed JSON
 * body of a non-2xx response); `eliteaFetch` (this port's transport, S4)
 * throws an `EliteaApiError` instead of returning an RTK-Query-shaped
 * `{error}` result, so an `EliteaApiError` whose `failure.kind === 'http'`
 * is this port's equivalent of `result.error.data` — `failure.body` is the
 * parsed (or raw-text) response body `http.ts`'s `toResult` already
 * captured. Returns `undefined` (never a generic HTTP-status string) when
 * the failure carries no such body — e.g. a network/auth/aborted failure,
 * or an 'http' failure whose body isn't the `{error, error_description}`
 * shape — so callers can fall back to their own baseline-matching literal
 * fallback text.
 *
 * Exported so `oauthFlow.ts`'s `exchangeAuthorizationCode` (same cluster,
 * same failure shape — both proxy calls go through `postOAuthProxy` in
 * `api/mcpOAuthClient.ts`) can reuse this instead of duplicating the
 * extraction logic.
 */
export function extractOAuthErrorDetail(cause: unknown): string | undefined {
  if (!(cause instanceof EliteaApiError) || cause.failure.kind !== 'http') return undefined;
  const body = cause.failure.body;
  if (!isOAuthErrorBody(body)) return undefined;
  return body.error_description || body.error || undefined;
}

/** The credentials a successful DCR registration yields — `clientSecret` is `undefined` for a true public client, defined when the server issues one anyway (baseline, post-`6ebe8ff7`: `mcpDiscovery.helpers.js`'s `{clientId, clientSecret}` shape). */
export interface RegisterDynamicClientResult {
  clientReference?: string | undefined;
  clientId: string;
  clientSecret: string | undefined;
}

/**
 * Registers a dynamic OAuth client via the backend's DCR proxy (avoids CORS
 * against the external OAuth server). Returns the issued client ID and
 * Main's reference for confidential clients. A legacy raw secret remains
 * supported in document memory, but it must never reach browser storage.
 */
export async function registerDynamicClient(registrationEndpoint: string, redirectUri: string, projectId: string | number | undefined, binding?: { tokenEndpoint: string | undefined; resource: string | undefined }): Promise<RegisterDynamicClientResult> {
  let registration: Awaited<ReturnType<typeof registerMcpDynamicClient>>;
  try {
    registration = await registerMcpDynamicClient({
      projectId: projectId ?? 1,
      registration_endpoint: registrationEndpoint,
      token_endpoint: binding?.tokenEndpoint,
      resource: binding?.resource,
      redirect_uris: [redirectUri],
      client_name: DCR_REQUEST_DEFAULTS.client_name,
      grant_types: DCR_REQUEST_DEFAULTS.grant_types,
      response_types: DCR_REQUEST_DEFAULTS.response_types,
      token_endpoint_auth_method: DCR_REQUEST_DEFAULTS.token_endpoint_auth_method,
      application_type: DCR_REQUEST_DEFAULTS.application_type,
    });
  } catch (cause) {
    // Baseline: `mcpDiscovery.helpers.js:93-95` — `Dynamic client
    // registration failed: ${errorData.error_description || errorData.error
    // || 'Unknown error'}`. Before this fix, a failed DCR call's
    // `EliteaApiError` (a generic "eliteaFetch: 400 from ..." message)
    // propagated as-is, losing whatever specific reason the OAuth server
    // gave (e.g. `invalid_redirect_uri`).
    const detail = extractOAuthErrorDetail(cause) ?? 'Unknown error';
    throw new Error(`${MCP_OAUTH_ERRORS.REGISTRATION_FAILED}: ${detail}`, { cause });
  }

  if (!registration.client_id) {
    throw new Error('Registration response missing client_id');
  }

  return {
    clientId: registration.client_id,
    clientSecret: registration.client_reference ? undefined : registration.client_secret,
    clientReference: registration.client_reference,
  };
}
