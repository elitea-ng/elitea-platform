/**
 * MCP OAuth proxy endpoints — hand-written port of
 * apps/elitea-ui/src/api/mcpOAuth.js (unit A5, manifest API-164/165/166).
 *
 * These proxies now have Main OpenAPI schemas. This compatibility adapter
 * remains hand-written and retains the existing flat response shape.
 * Hand-written against the SAME `eliteaFetch` mutator every generated hook
 * uses (spec §5.3: "a hand-written endpoint is indistinguishable from a
 * generated one at the call site") — this file imports it the same way
 * every `shared/api/generated/<tag>/<tag>.ts` file does
 * (`import { eliteaFetch } from '.../mutator'`), just from a different
 * caller. `shared/api/generated/` is `shared/` layer, not a `SLICED` slice
 * (`.dependency-cruiser.cjs`'s `no-deep-slice-import*` rules only cover
 * `processes|widgets|features|entities`), so importing `mutator.ts`
 * directly is not a deep-slice violation.
 *
 * KNOWN GAP (documented, not silently skipped — see the A5 final report):
 * these 3 endpoints are NOT registered in
 * `src/shared/api/endpoints.manifest.json` (R-A5's registry). That file
 * lives outside this unit's owned paths (`src/features/mcps/`,
 * `src/pages/mcps/`) — spec's own "APPEND CONVENTION for later Wave-2
 * units" comment in `scripts/check-endpoint-manifest.mjs` invites exactly
 * this kind of addition, but the A5 task brief's explicit "you may ONLY
 * write inside your own owns paths... leave that specific piece undone
 * rather than reaching outside your ownership fence" instruction takes
 * precedence for this unit. Left for a follow-up/integration pass.
 *
 * Baseline behaviour preserved: both `exchangeMcpOAuthToken` and
 * `refreshMcpOAuthToken` POST to the SAME URL
 * (`/elitea_core/mcp_oauth_proxy/{projectId}`) — they differ only in the
 * request body's `grant_type` (`authorization_code` vs `refresh_token`,
 * the latter forced by `refreshMcpOAuthToken` regardless of what the
 * caller passes — `mcpOAuth.js:33`). Both filter out `null`/`undefined`
 * body values before sending (`mcpOAuth.js:10,24`).
 *
 * `eliteaFetch<T>` resolves to the ENVELOPED `{data: T, status, headers}`
 * shape (every generated hook's own response type is `{data, status} &
 * {headers}` — see `shared/api/generated/mutator.ts`'s own header, "T is
 * always the ENVELOPED type orval generates for every operation"; verified
 * empirically against the live mutator, not assumed). `postOAuthProxy`
 * unwraps `.data` once here so every function below keeps returning the
 * flat response body its own callers already expect.
 */
import { eliteaFetch } from '@/shared/api/generated/mutator';

const MCP_OAUTH_PROXY_PATH = '/elitea_core/mcp_oauth_proxy';
const MCP_DCR_PROXY_PATH = '/elitea_core/mcp_dcr_proxy';

/** Drops `null`/`undefined` values so they are never serialised into the request body (baseline: `mcpOAuth.js:10,24,41`). */
function withoutNullish<T extends Record<string, unknown>>(body: T): Partial<T> {
  const filtered: Partial<T> = {};
  for (const [key, value] of Object.entries(body)) {
    if (value !== null && value !== undefined) {
      (filtered as Record<string, unknown>)[key] = value;
    }
  }
  return filtered;
}

async function postOAuthProxy<T>(path: string, projectId: string | number, body: Record<string, unknown>): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(`${path}/${projectId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(withoutNullish(body)),
  });
  return envelope.data;
}

/** The wire shape the backend's OAuth-proxy responses share, whichever grant type was used. */
export interface McpOAuthGrantResponse {
  authorization_resource?: string;
  authorization_reference?: string;
  authorization_expires_at?: string;
  access_token?: string;
  token_type?: string;
  expires_in?: number;
  refresh_token?: string;
  id_token?: string;
  session_id?: string;
  scope?: string;
  error?: string;
  error_description?: string;
}

export interface McpOAuthTokenResponse extends McpOAuthGrantResponse {
  access_token: string;
}

export interface ExchangeMcpOAuthTokenParams {
  authorization_reference_only?: boolean | undefined;
  client_reference?: string | undefined;
  resource?: string | undefined;
  projectId: string | number;
  token_endpoint?: string | undefined;
  code: string;
  redirect_uri: string;
  client_id?: string | undefined;
  client_secret?: string | undefined;
  code_verifier?: string | undefined;
  scope?: string | undefined;
  toolkit_id?: string | undefined;
  toolkit_type?: string | undefined;
  /**
   * NOT in the pinned `apps/elitea-ui` submodule snapshot (`a55f36cf`) —
   * added by the real upstream `frontends/EliteaUI` fix, commit `6ebe8ff7`
   * ("fix: [EL-5697] Aha! mcp token issue"), which postdates that pin. See
   * `oauthFlow.ts`'s `exchangeAuthorizationCode` for the full evidence
   * chain (including confirmation the currently-running legacy pylon
   * backend actually reads this field to decide whether to load a
   * DB-configured `client_secret`).
   */
  used_dcr?: boolean | undefined;
}

/** API-164 — `POST /elitea_core/mcp_oauth_proxy/{projectId}`, `grant_type: authorization_code`. */
export function exchangeMcpOAuthToken({ projectId, ...body }: ExchangeMcpOAuthTokenParams): Promise<McpOAuthGrantResponse> {
  return postOAuthProxy(MCP_OAUTH_PROXY_PATH, projectId, { ...body, grant_type: 'authorization_code' });
}

export interface RefreshMcpOAuthTokenParams {
  client_reference?: string | undefined;
  resource?: string | undefined;
  projectId: string | number;
  token_endpoint?: string | undefined;
  refresh_token: string;
  client_id?: string | undefined;
  client_secret?: string | undefined;
  toolkit_id?: string | undefined;
  /**
   * Baseline: `mcpAuthFlow.helpers.js` `refreshAccessToken`/`getValidAccessToken`
   * (`used_dcr: usedDcr || undefined`, threaded through by commit `6ebe8ff7`
   * "fix: [EL-5697] Aha! mcp token issue") — a proactive/reactive refresh needs
   * the same `used_dcr` signal the exchange call sends, so the backend proxy
   * (and any caller-side credential-fallback logic) can tell a DCR-issued
   * client apart from a toolkit-DB one on EVERY grant type, not just the
   * initial `authorization_code` exchange. Without this field on the params
   * type, `refreshMcpOAuthToken()` had no way to forward it even if a caller
   * wanted to — this was a TS-port-only regression: the untyped JS baseline's
   * `mcpOAuth.js` request body is a plain object spread, so it silently
   * accepted whatever fields the caller passed; the hand-written TS port's
   * explicit params interface is what dropped this one.
   */
  used_dcr?: boolean | undefined;
}

/** API-165 — same endpoint as the exchange call; `grant_type` is force-set to `refresh_token` regardless of caller input (baseline parity: `mcpOAuth.js:33`). */
export function refreshMcpOAuthToken({ projectId, ...body }: RefreshMcpOAuthTokenParams): Promise<McpOAuthTokenResponse> {
  return postOAuthProxy(MCP_OAUTH_PROXY_PATH, projectId, { ...body, grant_type: 'refresh_token' });
}

export interface RegisterMcpDynamicClientParams {
  token_endpoint?: string | undefined;
  resource?: string | undefined;
  projectId: string | number;
  registration_endpoint: string;
  redirect_uris: readonly string[];
  client_name: string;
  grant_types: readonly string[];
  response_types: readonly string[];
  token_endpoint_auth_method: string;
  application_type: string;
}

export interface McpDynamicClientRegistration {
  client_reference?: string;
  client_id: string;
  client_secret?: string;
  error?: string;
  error_description?: string;
}

/** API-166 — `POST /elitea_core/mcp_dcr_proxy/{projectId}` (Dynamic Client Registration proxy). */
export function registerMcpDynamicClient({ projectId, ...body }: RegisterMcpDynamicClientParams): Promise<McpDynamicClientRegistration> {
  return postOAuthProxy(MCP_DCR_PROXY_PATH, projectId, body);
}
