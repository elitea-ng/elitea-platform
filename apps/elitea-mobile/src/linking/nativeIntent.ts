import { identity } from '@/config/identity';
import { parseSurfaceLink, routeForSurfaceLink } from '@/features/surfaces/surfaceLinks';
import { getSession } from '@/state/session';

/**
 * expo-router hands every URL the OS opens the app with to
 * `src/app/+native-intent.tsx`. The OAuth redirect
 * (`ca.zefir.agent:/oauth/callback?code=…`) is not a screen: the system
 * browser session (`openAuthSessionAsync`) consumes it. On Android the same
 * intent also reaches the router, which would otherwise try to open a
 * non-existent `/oauth/callback` route.
 *
 *  - while the app is running: ignore it (null = stay on the current screen);
 *  - as the launch URL (the app was killed during sign-in, the attempt's PKCE
 *    verifier is gone): open the app's start, which routes to sign-in.
 * A widget or Live Activity link (`agentzefir://surface/…`,
 * src/features/surfaces/surfaceLinks.ts) becomes the screen it names when it
 * belongs to the active workspace (and, for a conversation, the selected
 * project), else the app's start.
 * Every other URL passes through unchanged.
 */
export function rewriteSystemPath(
  path: string,
  initial: boolean,
  session: () => { workspaceId: string | null; hydrated: boolean; projectId?: number | null } = getSession,
): string | null {
  if (isOAuthRedirect(path)) return initial ? '/' : null;
  const surface = parseSurfaceLink(path);
  if (surface !== null) {
    const { workspaceId, hydrated, projectId = null } = session();
    return routeForSurfaceLink(surface, workspaceId, hydrated, projectId);
  }
  return path;
}

export function isOAuthRedirect(path: string): boolean {
  const lower = path.toLowerCase();
  const callback = identity.oauthRedirectPath.toLowerCase();
  if (lower.startsWith(`${identity.oauthRedirectScheme.toLowerCase()}:`)) return true;
  const bare = lower.replace(/^\/+/, '/');
  return bare === callback || bare.startsWith(`${callback}?`) || bare.startsWith(`${callback}/`);
}
