/**
 * Auth flow constants + the popup↔opener message contract (spec §5.4).
 * Storage keys are LOGICAL keys — `shared/lib/storage.ts` prefixes them with
 * the `el.` namespace, so a complete logout sweeps them automatically.
 */

export const AUTH_MESSAGE_TYPE = 'elitea-auth-result';
export const AUTH_STATE_PARAM = 'auth_state';
/** sessionStorage (logical): the CSRF state of the in-flight popup flow. */
export const AUTH_STATE_STORAGE_KEY = 'auth.state';
/**
 * sessionStorage (logical): `Date.now()` of the moment the flight started.
 *
 * The single-flight guard of the controller is closure state, so it dies with
 * the document. A full page load while the popup is open therefore built a
 * SECOND controller with an empty slot, and that controller opened a second
 * flight into the popup that was already open (issue #364, measured on
 * WebKit: two documents, two controllers, two `auth_state` values, one tab).
 *
 * sessionStorage is per tab and survives a same-tab navigation, so the new
 * document reads this marker and ADOPTS the flight that already runs. The
 * timestamp bounds the adoption: a marker that no document ever cleared, left
 * by a popup the user abandoned, must not block re-auth for ever.
 */
export const AUTH_FLIGHT_STARTED_KEY = 'auth.flight.started';
/**
 * localStorage (logical) prefix: cross-window result fallback channel. The
 * key is STATE-SCOPED, exactly like the BroadcastChannel name — with a single
 * shared key two controllers (two tabs) consume each other's result: the
 * loser discards it on state mismatch and its own flight then hangs to
 * `popup_closed`. Still under the `el.` namespace, so logout's sweep clears
 * every scoped key without enumerating states.
 */
const AUTH_RESULT_STORAGE_KEY_PREFIX = 'auth.result.';

export function authResultStorageKey(state: string): string {
  return AUTH_RESULT_STORAGE_KEY_PREFIX + state;
}
export const AUTH_CALLBACK_PATH = '/auth-callback'; // ROUTE-001
export const AUTH_CHANNEL_PREFIX = 'elitea-auth-';
/**
 * Window-name prefix for the re-auth popup. The name is STATE-SCOPED.
 *
 * `window.open` REPLACES the page of a window that already carries the given
 * name. A fixed name therefore lets a second flight re-navigate the popup the
 * user works in: the typed value disappears and the form submits empty
 * (issue #364, measured on a WebKit trace of J3 — two authorize hops, two
 * `auth_state` values, one popup page, 0.8 s apart). Per-flight names make
 * that impossible, and they also cover the case no in-page guard can reach:
 * two tabs, two controllers, one shared name.
 */
const AUTH_WINDOW_NAME_PREFIX = 'elitea-auth-popup-';

export function authWindowName(state: string): string {
  return AUTH_WINDOW_NAME_PREFIX + state;
}
/**
 * Browser authentication routes live under `/auth/` (elitea-main
 * `internal/api/auth_paths.go`).
 */
export const LOGOUT_PATH = '/auth/logout';

/** The session probe (`SessionHandler.Info`). */
export const SESSION_INFO_PATH = '/auth/info';

/**
 * Single sign-on login entry point: the sign-in page, and the `target_to`
 * query parameter it honours.
 *
 * `/auth/login` is the SSO plane's sign-in page
 * (`internal/api/browserauth/chooser.go`). With one usable provider it
 * redirects at once to that provider's login route (`/auth/oidc/login`
 * or `/auth/saml/login`) and KEEPS `target_to`. With OIDC and SAML
 * both usable it shows a button per provider and a work email field, and
 * every hop carries `target_to`. A fixed OIDC login route could
 * not reach a SAML provider at all.
 *
 * The re-auth popup opens THIS, not the callback page directly. Measured on
 * the E2E stack (issue #136 B): nothing gates `/app/*` at the edge — an
 * unauthenticated browser is served the SPA shell at any deep link — so a
 * popup pointed straight at `/app/auth-callback` is answered with the app
 * itself and no login round trip ever happens. Here, the provider login route
 * keeps `target_to` in its signed state (`state=<nonce>|<target_to>` for OIDC,
 * the signed request cookie for SAML) and the callback redirects the
 * freshly-authenticated browser back to it verbatim — query string included,
 * which is what carries `auth_state` through.
 */
export const SSO_LOGIN_PATH = '/auth/login';

/**
 * Form login entry point.
 *
 * `/auth/form/login` renders the form itself and 400s without a
 * transaction id; only this path creates one (`beginLogin`,
 * `internal/api/browserauth/handler.go`), so this is what a browser is sent to.
 */
export const FORM_LOGIN_PATH = '/auth/login';
export const TARGET_TO_PARAM = 'target_to';

export interface AuthResultMessage {
  type: typeof AUTH_MESSAGE_TYPE;
  state: string;
  success: boolean;
}

export function isAuthResultMessage(data: unknown): data is AuthResultMessage {
  if (typeof data !== 'object' || data === null) return false;
  const record = data as Record<string, unknown>;
  return (
    record['type'] === AUTH_MESSAGE_TYPE &&
    typeof record['state'] === 'string' &&
    typeof record['success'] === 'boolean'
  );
}
