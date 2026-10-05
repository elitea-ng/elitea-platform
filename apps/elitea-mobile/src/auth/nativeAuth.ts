import * as WebBrowser from 'expo-web-browser';

import { assessDiscovery, type NativeAuthEndpoints } from '@/api/discovery';
import { getClientVersion } from '@/api/clientVersion';
import { deviceName, devicePlatform } from '@/auth/deviceInfo';
import { nativeClientId, startNativeSession } from '@/auth/nativeSession';
import { constantTimeEqual, createPkce, createState, type PkceDeps } from '@/auth/pkce';
import { exchangeAuthorizationCode, revokeToken, type TokenOutcome } from '@/auth/tokenEndpoint';
import { oauthRedirectUri } from '@/config/identity';
import { log } from '@/lib/log';
import { SECURE_KEYS, deleteSecureItem } from '@/security/secureStorage';
import { getSession, setSession } from '@/state/session';
import { getWorkspaceInfo, refreshWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Native sign-in (ADR-0025 WP2, RFC 8252): authorization code + PKCE S256 in
 * the SYSTEM browser (`openAuthSessionAsync`: ASWebAuthenticationSession on
 * iOS, Custom Tabs on Android), never an embedded web view. The workspace runs
 * its usual sign-in (SSO, OIDC, SAML or password), shows a confirmation naming
 * the app and the device, and redirects to `ca.zefir.agent:/oauth/callback`
 * with a single-use code (valid 60 s), the `state` and `iss`.
 *
 * Checks before the code is used: the redirect is exactly ours, `state`
 * matches the one this attempt generated, `iss` is the discovery issuer
 * (RFC 9207, mix-up defence), no parameter is repeated, and each code is
 * exchanged at most once, immediately.
 */

export type NativeSignInError =
  | 'busy'
  | 'not_available'
  | 'cancelled'
  | 'state_mismatch'
  | 'issuer_mismatch'
  | 'denied'
  | 'server_error'
  | 'invalid_grant'
  | 'invalid_client'
  | 'upgrade_required'
  | 'unavailable'
  | 'network'
  | 'storage'
  | 'workspace_changed';

export type NativeSignInResult = { ok: true } | { ok: false; error: NativeSignInError };

export interface AuthorizeRequest {
  endpoints: NativeAuthEndpoints;
  clientId: string;
  redirectUri: string;
  codeChallenge: string;
  state: string;
  deviceName: string;
  platform: string;
  clientVersion: string;
}

export function buildAuthorizeUrl(request: AuthorizeRequest): string {
  const params: [string, string][] = [
    ['response_type', 'code'],
    ['client_id', request.clientId],
    ['redirect_uri', request.redirectUri],
    ['code_challenge', request.codeChallenge],
    ['code_challenge_method', 'S256'],
    ['state', request.state],
    ['device_name', request.deviceName],
    ['platform', request.platform],
    ['client_version', request.clientVersion],
  ];
  const query = params.map(([k, v]) => `${encodeURIComponent(k)}=${encodeURIComponent(v)}`).join('&');
  return `${request.endpoints.authorization_endpoint}?${query}`;
}

export type RedirectParams = Record<string, string>;

function decodeComponent(value: string): string | null {
  try {
    return decodeURIComponent(value.replace(/\+/g, ' '));
  } catch {
    return null;
  }
}

/**
 * The query of an authorization response, or null when `url` is not our
 * redirect URI, carries a fragment, or repeats a parameter.
 */
export function parseRedirect(url: string, redirectUri: string = oauthRedirectUri): RedirectParams | null {
  const colon = redirectUri.indexOf(':');
  const scheme = redirectUri.slice(0, colon).toLowerCase();
  const path = redirectUri.slice(colon + 1);
  const lower = url.slice(0, colon + 1).toLowerCase();
  if (lower !== `${scheme}:`) return null;
  let rest = url.slice(colon + 1);
  // Some platforms normalise `scheme:/path` to `scheme:///path`.
  if (rest.startsWith('///')) rest = rest.slice(2);
  if (!rest.startsWith(`${path}?`)) return null;
  const query = rest.slice(path.length + 1);
  if (query.includes('#')) return null;
  const out: RedirectParams = {};
  for (const pair of query.split('&')) {
    if (pair === '') continue;
    const eq = pair.indexOf('=');
    const key = decodeComponent(eq < 0 ? pair : pair.slice(0, eq));
    const value = decodeComponent(eq < 0 ? '' : pair.slice(eq + 1));
    if (key === null || value === null || key in out) return null;
    out[key] = value;
  }
  return out;
}

/** Validates an authorization response for this attempt and returns its code. */
export function verifyRedirect(
  params: RedirectParams,
  expected: { state: string; issuer: string },
): { ok: true; code: string } | { ok: false; error: NativeSignInError } {
  if (params.state === undefined || !constantTimeEqual(params.state, expected.state)) {
    return { ok: false, error: 'state_mismatch' };
  }
  // The workspace always sends `iss` (RFC 9207); a missing or different one
  // means the response is not from the server this attempt was started with.
  if (params.iss === undefined || params.iss.replace(/\/+$/, '') !== expected.issuer.replace(/\/+$/, '')) {
    return { ok: false, error: 'issuer_mismatch' };
  }
  if (params.error !== undefined) {
    return { ok: false, error: params.error === 'access_denied' ? 'denied' : 'server_error' };
  }
  if (!params.code || params.code.length > 1024) return { ok: false, error: 'server_error' };
  return { ok: true, code: params.code };
}

export interface NativeSignInDeps {
  openAuthSession?: (url: string, redirectUri: string) => Promise<WebBrowser.WebBrowserAuthSessionResult>;
  exchange?: typeof exchangeAuthorizationCode;
  pkce?: PkceDeps;
}

/**
 * The system sign-in sheet runs as a private (ephemeral) web session
 * (DEF-R6): ASWebAuthenticationSession with prefersEphemeralWebBrowserSession,
 * so it neither reads nor keeps the system browser's cookies. Without it, a
 * fresh install on a device where someone had signed in to the workspace in
 * Safari skipped the identity provider and offered consent for that previous
 * user straight away. The cost is that the user signs in to the workspace each
 * time the app signs in (once per device session, not per launch), and iOS no
 * longer shows the "wants to use … to sign in" prompt, which only guards a
 * shared session. iOS only; Android's Custom Tabs ignore it.
 */
export const AUTH_SESSION_OPTIONS: WebBrowser.AuthSessionOpenOptions = { preferEphemeralSession: true };

const defaultOpenAuthSession = (url: string, redirectUri: string) =>
  WebBrowser.openAuthSessionAsync(url, redirectUri, AUTH_SESSION_OPTIONS);

let inFlight: Promise<NativeSignInResult> | null = null;
/** Codes already presented: a code is exchanged once, whatever the outcome. */
const usedCodes = new Set<string>();

export function signInWithWorkspace(deps: NativeSignInDeps = {}): Promise<NativeSignInResult> {
  if (inFlight) return Promise.resolve({ ok: false, error: 'busy' });
  inFlight = run(deps).finally(() => {
    inFlight = null;
  });
  return inFlight;
}

async function run(deps: NativeSignInDeps): Promise<NativeSignInResult> {
  const origin = getSession().serverOrigin;
  if (origin === null) return { ok: false, error: 'workspace_changed' };

  let info = getWorkspaceInfo();
  if (info?.origin !== origin) {
    await refreshWorkspaceInfo();
    info = getWorkspaceInfo();
  }
  if (info === null || info.origin !== origin) return { ok: false, error: 'not_available' };
  const assessment = assessDiscovery(info.discovery);
  if (assessment.kind === 'upgrade_required') {
    setSession({ upgradeRequired: { minVersion: assessment.minVersion } });
    return { ok: false, error: 'upgrade_required' };
  }
  const endpoints = info.discovery.native_auth;
  if (assessment.kind !== 'ready' || endpoints === null) return { ok: false, error: 'not_available' };

  const clientId = nativeClientId();
  const { verifier, challenge } = await createPkce(deps.pkce);
  const state = createState(deps.pkce);
  const url = buildAuthorizeUrl({
    endpoints,
    clientId,
    redirectUri: oauthRedirectUri,
    codeChallenge: challenge,
    state,
    deviceName: deviceName(),
    platform: devicePlatform(),
    clientVersion: getClientVersion(),
  });

  let result: WebBrowser.WebBrowserAuthSessionResult;
  try {
    result = await (deps.openAuthSession ?? defaultOpenAuthSession)(url, oauthRedirectUri);
  } catch (error) {
    log.warn('system browser sign-in could not start', { error });
    return { ok: false, error: 'not_available' };
  }
  if (result.type !== 'success') return { ok: false, error: 'cancelled' };

  const params = parseRedirect(result.url);
  if (params === null) return { ok: false, error: 'server_error' };
  const verified = verifyRedirect(params, { state, issuer: endpoints.issuer });
  if (!verified.ok) {
    log.warn('authorization response refused', { error: verified.error });
    return verified;
  }
  if (usedCodes.has(verified.code)) return { ok: false, error: 'invalid_grant' };
  usedCodes.add(verified.code);
  if (getSession().serverOrigin !== origin) return { ok: false, error: 'workspace_changed' };

  const outcome = await (deps.exchange ?? exchangeAuthorizationCode)(origin, endpoints.token_endpoint, {
    code: verified.code,
    codeVerifier: verifier,
    redirectUri: oauthRedirectUri,
    clientId,
  });
  return finish(outcome, origin, endpoints, clientId);
}

async function finish(
  outcome: TokenOutcome,
  origin: string,
  endpoints: NativeAuthEndpoints,
  clientId: string,
): Promise<NativeSignInResult> {
  switch (outcome.kind) {
    case 'ok':
      break;
    case 'device_revoked':
    case 'invalid_grant':
      return { ok: false, error: 'invalid_grant' };
    case 'invalid_client':
      return { ok: false, error: 'invalid_client' };
    case 'upgrade_required':
      return { ok: false, error: 'upgrade_required' };
    case 'unavailable':
      return { ok: false, error: 'unavailable' };
    case 'network':
      return { ok: false, error: 'network' };
    default:
      return { ok: false, error: 'server_error' };
  }
  if (getSession().serverOrigin !== origin) return { ok: false, error: 'workspace_changed' };
  try {
    await startNativeSession(outcome.response, outcome.receivedAt, {
      origin,
      clientId,
      tokenEndpoint: endpoints.token_endpoint,
      revocationEndpoint: endpoints.revocation_endpoint,
    });
  } catch (error) {
    log.warn('native sign-in could not store its tokens', { error });
    // Do not leave a half-written credential or an orphaned device session.
    await Promise.allSettled([
      deleteSecureItem(SECURE_KEYS.refreshToken),
      deleteSecureItem(SECURE_KEYS.accessToken),
      deleteSecureItem(SECURE_KEYS.credentialKind),
      revokeToken(origin, endpoints.revocation_endpoint, { token: outcome.response.refresh_token, clientId }),
    ]);
    return { ok: false, error: 'storage' };
  }
  setSession({ endReason: null, lastOriginHint: null });
  return { ok: true };
}

/** Test helper. */
export function __resetNativeAuthForTests(): void {
  inFlight = null;
  usedCodes.clear();
}
