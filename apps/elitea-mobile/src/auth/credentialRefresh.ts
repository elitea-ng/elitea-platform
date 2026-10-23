/**
 * The seam between the request layer (src/api/) and the native token manager
 * (src/auth/nativeSession.ts), which itself talks to the token endpoint
 * through src/api/. Registering instead of importing keeps that cycle out of
 * the module graph. With no refresher registered (PAT sessions, tests),
 * requests use the session token as is.
 */

export type RefreshVerdict =
  /** A new access token is in the session. */
  | 'refreshed'
  /** The server revoked the device session; `endSession('device_revoked')` is running. */
  | 'device_revoked'
  /** The refresh token or client was refused; `endSession('credential_rejected')` is running. */
  | 'rejected'
  /** The app is below the workspace minimum; the update screen is showing. */
  | 'upgrade_required'
  /** The token endpoint could not be reached; nothing changed, try again later. */
  | 'unavailable';

export interface CredentialRefresher {
  /** The access token to send now, refreshing first when it is (about to be) expired. */
  ensureFresh(): Promise<{ ok: true; token: string } | { ok: false; verdict: Exclude<RefreshVerdict, 'refreshed'> }>;
  /** The server answered 401 (not device_revoked) for `rejectedToken`: refresh once (single-flight). */
  afterRejection(rejectedToken: string): Promise<RefreshVerdict>;
}

let refresher: CredentialRefresher | null = null;

export function setCredentialRefresher(next: CredentialRefresher | null): () => void {
  refresher = next;
  return () => {
    if (refresher === next) refresher = null;
  };
}

export function getCredentialRefresher(): CredentialRefresher | null {
  return refresher;
}
