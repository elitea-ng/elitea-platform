/**
 * Complete logout (spec §5.4 behaviour 7).
 *
 * The old logout cleared 2 sessionStorage keys + the Redux user slice
 * (apps/elitea-ui/src/slices/user.js:24-27) and left `elitea_ui.project.id`,
 * `elitea_ui.project.name`, the MCP OAuth tokens (`elitea_mcp_tokens_v1`)
 * and the tour keys behind. The new logout sweeps EVERY key under the `el.`
 * namespace in BOTH storage areas via `clearNamespace()` — completeness is
 * proven by the write-enumeration test in logout.test.ts, not a key list.
 */
import { clearNamespace } from '../../lib/storage';
import { getNativeTransport } from '../nativeTransport';

import { FORM_LOGIN_PATH, LOGOUT_PATH, TARGET_TO_PARAM } from './constants';

/**
 * The logout hand-off target. It is `/auth/login`.
 *
 * Read the constant name as "the browser login entry point", not "the form
 * plane only". Both authentication planes register this one path, and no
 * other login path is registered on both:
 *  - Form plane: `browserauth.LoginPath` opens a login transaction
 *    (`internal/api/browserauth/handler.go`).
 *  - OIDC plane: `internal/api/router.go` answers it with a 302 to
 *    `/auth/oidc/login`.
 *
 * The `target_to`-dropping caveat on `FORM_LOGIN_PATH` applies to the re-auth
 * popup, which must carry `auth_state` back to the callback route. A logout
 * carries no return target, so the caveat does not apply here.
 */
const BROWSER_LOGIN_PATH = FORM_LOGIN_PATH;

export interface LogoutDeps {
  /** Navigation seam; default assigns `window.location.href`. */
  redirect?: (url: string) => void;
  /** Origin the edge-auth logout URL is built on; default page origin. */
  origin?: string;
}

/**
 * True from the moment `performLogout()` starts until this document dies.
 *
 * MEASURED DEFECT, not a precaution (issue #482). A logout does not end the
 * document: the browser stays on the app page for the whole redirect chain
 * `/auth/logout` → `/auth/oidc/login` → the provider's
 * authorize endpoint. The logout endpoint clears the session cookie on the
 * first hop, so any request the page still has open then answers 401. That
 * 401 reaches `shared/api/http.ts`'s `runReauth()`, which starts a re-auth
 * flight, and the flight writes `el.auth.state` and `el.auth.flight.started`
 * back into sessionStorage — AFTER `clearNamespace()` swept them.
 *
 * Two costs, and the second is the product one:
 *  1. the namespace the logout is contracted to clear is not clear;
 *  2. the user who asked to sign out is shown a sign-in popup.
 *
 * Observed on WebKit, in 2 of 60 runs of end-to-end journey J4, with exactly
 * those two keys surviving. It is a race against the network, so it is rare
 * and it never stops being possible.
 *
 * The flag is module state, not storage: its correct lifetime is exactly the
 * lifetime of the document that starts the logout, and storage would outlive
 * that and would itself be a key in the namespace.
 */
let loggingOut = false;

/** True once `performLogout()` has run in this document. */
export function isLoggingOut(): boolean {
  return loggingOut;
}

/**
 * Clears the entire `el.` namespace (local + session), then hands the browser
 * to the backend logout (old UserButton.jsx:32 preserved:
 * `{origin}/auth/logout`) with the browser login entry point
 * `/auth/login` as its `target_to`.
 *
 * The target is PLANE-NEUTRAL, and that is a correction (see
 * `BROWSER_LOGIN_PATH` above). The earlier revision named
 * `/auth/oidc/login` here. `internal/api/router.go` registers
 * that path inside `if cfg.OIDCHandler != nil`, so a form-auth deployment has
 * no such route. There the chain ran `/auth/logout` → 302
 * `/auth/form/logout` → 302 `/auth/oidc/login` →
 * chi's bare `404 page not found`. The user was signed out and parked on a
 * plain-text dead end with no link back.
 *
 * The `target_to` is what makes JRNY-004's "…and the login screen is reached"
 * true, and it is BEHAVIOURAL parity with the old app rather than a new idea.
 * The old app sent the browser to a bare `/auth/logout` and still
 * arrived at a login screen, because the old deployment gated the SPA at the
 * edge: the post-logout landing (`/` → the app) was itself answered with an
 * OIDC redirect. This stack does not gate `/app/*` at the edge (measured —
 * an unauthenticated browser is served the SPA shell at any deep link), so a
 * bare logout clears the cookie and then parks the signed-out user on the
 * index route's loading state forever. Naming the login endpoint explicitly
 * reproduces the old END STATE on a stack whose edge no longer supplies it,
 * and elitea-main accepts it: it is a same-origin absolute path, which is all
 * `browserflow.CanonicalReturnTarget` requires.
 *
 * Verified against the running E2E stack, as a real browser navigation chain:
 * `/auth/logout?target_to=…` → 302 `/auth/login` → 302
 * `/auth/oidc/login` → 302 the provider's `/oauth2/authorize`.
 * The OIDC end state is the same as before; one 302 hop is added. (On that
 * stack the last hop then fails DNS. The issuer is the compose hostname
 * `oidc-mock`, which the host browser cannot resolve. The same artifact
 * `e2e/auth.setup.ts` works around it by rewriting the hostname. That is a
 * property of the test stack, not of this function.)
 */
export function performLogout(deps: LogoutDeps = {}): void {
  // Set BEFORE the sweep, not after. A flight that starts between the two
  // lines would write its keys after `clearNamespace()` has passed them.
  loggingOut = true;
  clearNamespace();
  // The desktop app has no server-side logout page to visit: its session is a
  // refresh token in the OS keychain. The host revokes it, wipes, and the app
  // reloads onto the connect screen.
  const native = getNativeTransport();
  if (native?.logout !== undefined) {
    void native.logout();
    return;
  }
  const origin = deps.origin ?? window.location.origin;
  const redirect =
    deps.redirect ??
    ((url: string): void => {
      window.location.href = url;
    });
  redirect(
    `${origin}${LOGOUT_PATH}?${TARGET_TO_PARAM}=${encodeURIComponent(BROWSER_LOGIN_PATH)}`,
  );
}
