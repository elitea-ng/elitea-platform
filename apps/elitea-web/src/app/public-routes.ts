/**
 * The routes this SPA serves to a browser that has NO session, and must go on
 * serving after the session probe comes back empty.
 *
 * `App`'s boot effect sends an unauthenticated browser to the login form,
 * because nothing gates `/app/**` at the edge and every route guard fails open
 * (see the effect's own note). That redirect is right for the application, and
 * wrong for the two pages that exist for people who are not signed in — the
 * OIDC callback, which runs unauthenticated by definition, and the shared
 * conversation page, whose whole audience is a link holder with no account.
 *
 * The defect this closes: a share link opened by its recipient rendered for a
 * few hundred milliseconds and was then replaced by the identity provider's
 * login screen. The page itself was correct, its API read is anonymous by
 * design (`internal/api/shared_chat_routes.go` mounts it outside every auth
 * middleware), and the edge forwards it — the SPA navigated away from it on
 * its own.
 *
 * Matched on a SUBSTRING, not a whole-path equality: the router is mounted at
 * `VITE_BASE_URI` (`/app/` in every deployed build), so the real address is
 * `/app/shared/chat/<token>`, and this predicate reads
 * `window.location.pathname`, which carries that basename.
 */
import { AUTH_CALLBACK_PATH } from '@/shared/api/auth/constants';

/** The route segment `routes/shared.chat.$token.tsx` claims. */
const SHARED_CONVERSATION_PATH = '/shared/chat/';

/** True when `pathname` names a page that must render without a session. */
export function isPublicRoutePath(pathname: string): boolean {
  if (pathname.endsWith(AUTH_CALLBACK_PATH)) return true;
  return pathname.includes(SHARED_CONVERSATION_PATH);
}
