/**
 * The client the app shell's own session probe uses.
 *
 * It lives beside `session-store.ts` rather than inside it because that file
 * is at its 400-line budget, and because this IS a separate decision: which
 * client is allowed to move the browser.
 */
import { buildExpiryLoginUrl } from '@/shared/api/auth/login-redirect';
import { createHttpClient, type HttpClient } from '@/shared/api/http';

/**
 * THE PRIMARY SESSION CHECK, and the only client in this app that navigates.
 *
 * `/forward-auth/info` answers `401 session_expired` when a cookie was
 * presented and no longer works. That is a statement about the SESSION, so the
 * shell acts on it with a full-page navigation to the login start, carrying
 * the page the user was on. Before this, an expired user sat on a screen whose
 * every request failed and nothing moved the browser — what the 1.60.0 smoke
 * run recorded.
 *
 * No other client is given the handler. A 401 from the notification bell or a
 * permission read must NOT navigate; see `shared/api/http.ts`.
 */
export function createProbeClient(navigate: (url: string) => void): HttpClient {
  return createHttpClient({
    baseUrl: '/',
    onSessionExpired: (loginUrl) => {
      navigate(buildExpiryLoginUrl(loginUrl, window.location.pathname + window.location.search));
    },
  });
}
