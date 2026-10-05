import { identity } from '@/config/identity';

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
 * Every other URL passes through unchanged.
 */
export function rewriteSystemPath(path: string, initial: boolean): string | null {
  if (!isOAuthRedirect(path)) return path;
  return initial ? '/' : null;
}

export function isOAuthRedirect(path: string): boolean {
  const lower = path.toLowerCase();
  const callback = identity.oauthRedirectPath.toLowerCase();
  if (lower.startsWith(`${identity.oauthRedirectScheme.toLowerCase()}:`)) return true;
  const bare = lower.replace(/^\/+/, '/');
  return bare === callback || bare.startsWith(`${callback}?`) || bare.startsWith(`${callback}/`);
}
