/**
 * The 401 policy: which refusals are session failures, which are not, and what
 * the server said about an EXPIRED session.
 *
 * Split out of `http.ts` because that file is at its 400-line budget and
 * because the three rules here are one subject: what a 401 MEANS. `http.ts`
 * decides what to DO about the answer.
 */

/* ── behaviour 2 (secondary): forward-auth redirect sniff ────────────────── */

/**
 * Old sniff retained as the SECONDARY signal (eliteaApi.js:26-28): the final
 * URL, with `target_to` removed first so its VALUE cannot fake a match,
 * containing both `/forward-auth/` and `/login`.
 */
function isAuthRedirect(finalUrl: string): boolean {
  const url = new URL(finalUrl);
  url.searchParams.delete('target_to');
  const stripped = url.toString();
  return stripped.includes('/forward-auth/') && stripped.includes('/login');
}

/**
 * 401 only, not 403.
 *
 * elitea-main splits the two cleanly: `middleware/auth.go` answers 401
 * `authentication_error` when it cannot establish a principal, and
 * `middleware/rbac.go` answers 403 `insufficient permissions` when it can but
 * the permission is missing. Re-authenticating the SAME user cannot add a
 * permission, so a 403 that re-auths always replays into the same 403 — the
 * flow is pure cost.
 *
 * It is worse than pure cost, which is why this changed. Measured against a
 * live standalone stack (issue 93, project 1 without
 * `models.applications.index_meta.details`): the indexes rail's 403 opened the
 * re-auth flow, the flow did not settle, and `useQuery` therefore stayed
 * PENDING — eight loading skeletons still on screen after nine seconds, with
 * the Indexes tab itself still rendered because tab visibility comes from the
 * toolkit type schema rather than from permissions. A refusal presented as a
 * hung fetch. Any future permission misconfiguration presented the same way.
 *
 * The escalation also DISCARDED the response body for every 403, since the
 * `kind: 'auth'` failure carries none — a gap already disclosed from four
 * separate call sites (`pages/admin/api/adminSecretsApi.ts`,
 * `adminSchedulesApi.ts`, `adminConfigurationApi.ts`, `adminAppRequestsApi.ts`)
 * and worked around in a fifth (`pages/settings/Secrets.tsx`, which tests
 * `kind === 'http' || kind === 'auth'` for its 403). A 403 now takes the
 * ordinary `kind: 'http'` path with its status and body intact; every consumer
 * of `HttpFailure` already switches exhaustively over both kinds.
 *
 * The redirect sniff is unchanged and still secondary: a forward-auth login
 * redirect is a session failure whatever status it carries.
 */
export function needsReauth(response: Response): boolean {
  if (response.status === 401) return true;
  return response.redirected && isAuthRedirect(response.url);
}

/* ── behaviour 2b: resource-authorization 401s are NOT session failures ──── */

/**
 * A 401 whose JSON body carries `requires_authorization: true` is a protocol
 * response about the RESOURCE, not about the caller's session: the backend is
 * saying "this toolkit/MCP server needs its own OAuth authorization", and the
 * body's `auth_metadata` is the authorization-server metadata the client needs
 * to start that flow (`POST /configurations/check_connection/...` is the live
 * example). Re-authenticating the user changes nothing about it, and — the
 * reason this exists — funnelling it into the `kind: 'auth'` branch DISCARDS
 * the body, which is the only place `auth_metadata` ever appears.
 *
 * That discard is what made SharePoint's delegated login unreachable even
 * once its UI was wired: `useSharepointCheckConnection`'s
 * `authRequiredErrorData` looks for exactly this body and could never see
 * one, so the OAuth modal was never asked to open. `features/agents/model/
 * useCreateConfiguration.ts`'s `isAuthRequiredError` disclosed the same gap
 * from the other side.
 *
 * Deliberately narrow: 401 only, JSON only, and only when the flag is
 * literally `true` — every other 401 keeps the existing re-auth behaviour
 * untouched. Reads a CLONE, so the original response is still
 * consumable by `toResult`.
 */
export async function resourceAuthorizationBody(response: Response): Promise<unknown> {
  if (response.status !== 401) return undefined;
  if (!(response.headers.get('content-type') ?? '').includes('application/json')) return undefined;
  try {
    const body: unknown = await response.clone().json();
    if (typeof body === 'object' && body !== null && (body as { readonly requires_authorization?: unknown }).requires_authorization === true) {
      return body;
    }
  } catch {
    // Handled (§3.6): a 401 that lies about its content-type is just a 401 —
    // fall through to the normal re-auth path.
  }
  return undefined;
}

/* ── behaviour 2c: the session-expiry contract ───────────────────────────── */

/** The one code the app shell navigates on. Written by `writeSessionExpired`. */
const SESSION_EXPIRED_CODE = 'session_expired';

/**
 * The login URL a `session_expired` 401 named, or `undefined` when this 401 is
 * not that answer.
 *
 * Deliberately narrow: 401 only, JSON only, and only when `error.code` is
 * literally `session_expired`. Every other 401 keeps the existing behaviour.
 * Reads a CLONE, so the original response is still consumable by `toResult`.
 *
 * The URL comes from the body's `login_url`, and falls back to the `Location`
 * header the same response carries. Both are written by the same handler and
 * hold the same value; reading either means a change to one of them cannot
 * silently disable this.
 */
export async function sessionExpiredLoginUrl(response: Response): Promise<string | undefined> {
  if (response.status !== 401) return undefined;
  if (!(response.headers.get('content-type') ?? '').includes('application/json')) return undefined;
  try {
    const body: unknown = await response.clone().json();
    const error = (body as { readonly error?: { readonly code?: unknown } } | null)?.error;
    if (error?.code !== SESSION_EXPIRED_CODE) return undefined;
    const named = (body as { readonly login_url?: unknown }).login_url;
    if (typeof named === 'string' && named !== '') return named;
    return response.headers.get('location') ?? undefined;
  } catch {
    // Handled (§3.6): a 401 that lies about its content-type is just a 401.
    return undefined;
  }
}

/* ── behaviour 2d: WHICH refusal this 401 is ─────────────────────────────── */

/**
 * The check that refused the request, as elitea-main's `middleware/auth.go`
 * named it — or `undefined` when this 401 named nothing.
 *
 * WHY IT IS READ AT ALL. The same 401 body — `{"code":"unauthenticated",
 * "message":"missing authorization header"}` — used to be written when the
 * browser sent no cookie, when it sent one signed by another deployment's
 * secret, when the cookie had expired, and when a server-side session had been
 * revoked. #538 is what that cost: a journey holding a valid session was
 * refused once in three runs and nobody could say which of the four it was,
 * because the response was the only evidence that outlived the run. The server
 * now writes a specific `error.code` for each, and this reads it.
 *
 * IT CHANGES NO DECISION. `needsReauth` still branches on the status alone, so
 * a code this app has never seen behaves exactly as it does today. The value
 * rides on the `kind: 'auth'` failure so a console, a log or an E2E report
 * names the cause instead of restating the status.
 *
 * Reads a CLONE, so the original response is still consumable by `toResult`.
 */
export async function credentialRefusalCode(response: Response): Promise<string | undefined> {
  if (response.status !== 401) return undefined;
  if (!(response.headers.get('content-type') ?? '').includes('application/json')) return undefined;
  try {
    const body: unknown = await response.clone().json();
    const code = (body as { readonly error?: { readonly code?: unknown } } | null)?.error?.code;
    return typeof code === 'string' && code !== '' ? code : undefined;
  } catch {
    // Handled (§3.6): a 401 that lies about its content-type names nothing.
    return undefined;
  }
}
