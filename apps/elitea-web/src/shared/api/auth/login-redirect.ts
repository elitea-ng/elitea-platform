import { FORM_LOGIN_PATH, OIDC_LOGIN_PATH, TARGET_TO_PARAM } from './constants';

/**
 * Where to send a browser that is not logged in.
 *
 * Two authentication planes exist in elitea-main and they are MUTUALLY
 * EXCLUSIVE by construction (`internal/api/production_router.go` mounts one or
 * the other, never both). They expose different entry points, and this module
 * exists because the app previously knew only one of them:
 *
 *   OIDC plane  `/forward-auth/info`, `/forward-auth/auth_oidc/login`
 *   Form plane  `/forward-auth/login` -> `/forward-auth/auth_form/login`
 *
 * The Form plane serves no `/forward-auth/info` at all, so on a Form
 * deployment the session probe 404s, the app concludes "not logged in" — which
 * is right — and then had nowhere to send the user. The measured result was a
 * permanent `<RoutePending />` spinner on every deep link, with the login form
 * one redirect away and nothing performing it.
 *
 * So the plane is INFERRED from the probe rather than configured: a 404 from
 * `/forward-auth/info` means that endpoint is not mounted, which means the Form
 * plane. Anything else means the OIDC plane answered. That keeps a single build
 * of this app correct on both, with no new runtime-config key to set wrong.
 */

export type AuthPlane = 'form' | 'oidc';

/**
 * `status` is the HTTP status of the `/forward-auth/info` probe, or undefined
 * when the request never produced one (network failure). Only 404 identifies
 * the Form plane: a 401 is the OIDC plane saying "no session", which is a
 * different answer and must not switch planes.
 */
export function authPlaneFromProbeStatus(status: number | undefined): AuthPlane {
  return status === 404 ? 'form' : 'oidc';
}

export function loginPathForPlane(plane: AuthPlane): string {
  return plane === 'form' ? FORM_LOGIN_PATH : OIDC_LOGIN_PATH;
}

/**
 * Builds the absolute login URL for `plane`, carrying the caller back to
 * `returnTo` afterwards.
 *
 * `returnTo` must be a same-origin absolute path: elitea-main validates it
 * through `browserflow.CanonicalReturnTarget` and silently falls back to the
 * deployment's `default_login` when it is anything else, so an absolute URL
 * here would quietly lose the user's place rather than fail loudly.
 */
export function buildLoginUrl(plane: AuthPlane, returnTo: string): string {
  const target = returnTo.startsWith('/') ? returnTo : `/${returnTo}`;
  return `${loginPathForPlane(plane)}?${TARGET_TO_PARAM}=${encodeURIComponent(target)}`;
}

/**
 * The login URL the app shell navigates to when the server states that the
 * session expired.
 *
 * `named` is the `login_url` `/forward-auth/info` answered with. It is USED —
 * the server knows which browser plane is mounted — but its `target_to` is
 * REPLACED with the page the browser is actually on. The probe is an XHR
 * issued from wherever the user happens to be, so the server sees no page to
 * return to; the client is the only side that knows.
 *
 * A server answer this client cannot parse falls back to `buildLoginUrl`, so
 * the expiry path and the boot path cannot disagree about the login start.
 */
export function buildExpiryLoginUrl(named: string, returnTo: string): string {
  try {
    const url = new URL(named, window.location.origin);
    url.searchParams.set(TARGET_TO_PARAM, returnTo);
    return url.pathname + url.search;
  } catch {
    // Handled (§3.6): a malformed hint is not a reason to strand the user on a
    // page whose every request now fails.
    return buildLoginUrl('oidc', returnTo);
  }
}
