/**
 * Native-client transport registry (ADR-0029 decision 9, ADR-0025).
 *
 * A native client (the desktop app) differs from the browser in exactly two
 * transport facts: it authenticates with `Authorization: Bearer <access
 * token>` rather than a session cookie, and it talks to an absolute
 * deployment origin rather than the page origin. This module is the one seam
 * for both. `http.ts` and `sse/useEventSource.ts` read it; nothing else does.
 *
 * It is a registry, not a client: `createHttpClient` is called from several
 * places (the generated mutator, the session probe, the permission readers),
 * and all of them must pick the token up without being threaded a parameter.
 * Only the desktop entry (`entries/desktop`) ever registers a transport, so in
 * every other build `getNativeTransport()` is `undefined` and the browser
 * behaviour is untouched. The implementation (host IPC, fetch-based SSE) lives
 * under `shared/desktop/` and is imported by the desktop entry alone, so it is
 * tree-shaken out of the default bundle.
 */

/** Why the client gave up its session. */
export type NativeSignOutReason = 'refresh_failed' | 'device_revoked' | 'logout';

/**
 * How a refresh ended. `unavailable` (network down, server busy) is NOT a
 * reason to sign out: the refresh token is still good and the caller retries
 * later. Only `ended` (the server refused the refresh token) is.
 */
export type RefreshOutcome = 'refreshed' | 'ended' | 'unavailable';

export interface NativeTransport {
  /** The current access token, or `undefined` when signed out. */
  accessToken(): Promise<string | undefined>;
  /**
   * Obtain a new access token after `failedToken` was refused with a 401.
   * Resolves `refreshed` when a token different from `failedToken` is available
   * (an earlier concurrent refresh may already have produced it). The
   * implementation owns single-flight: refresh tokens rotate, so two
   * concurrent refreshes would burn the family.
   */
  refresh(failedToken: string | undefined): Promise<RefreshOutcome>;
  /** The session is over; `device_revoked` means local data must be wiped. */
  signOut(reason: NativeSignOutReason): void;
  /**
   * The person chose to log out: revoke the session server-side, forget it,
   * wipe local data, and return to the sign-in screen. When present,
   * `performLogout()` calls this instead of navigating to the server's logout
   * URL (the bundled app cannot serve it, and a reload would still be signed in).
   */
  logout?(): Promise<void>;
  /**
   * The deployment origin. The bearer token is attached only to URLs on this
   * origin, whatever a caller passes.
   */
  readonly origin?: string;
  /** Headers sent on every request, e.g. `X-Client-Version` (ADR-0025 426 gate). */
  readonly headers?: Readonly<Record<string, string>>;
  /** Network layer override; the desktop host routes it through Rust (no CORS). */
  readonly fetch?: typeof fetch;
  /**
   * Opens an event stream with the `EventSource` surface callers already use
   * (`addEventListener`, `readyState`, `close`) but with a bearer header, which
   * the browser constructor cannot send. See `shared/desktop/fetchEventSource.ts`.
   */
  readonly createEventSource?: (url: string) => EventSourceLike;
  /** A request answered 426: this build is older than the deployment's minimum. */
  onUpgradeRequired?(): void;
}

/** The part of `EventSource` that `useEventSource` touches. */
export interface EventSourceLike {
  readonly readyState: number;
  addEventListener(type: string, listener: (event: MessageEvent) => void): void;
  close(): void;
}

let active: NativeTransport | undefined;

/** Register (or, with `undefined`, clear) the process-wide native transport. */
export function setNativeTransport(transport: NativeTransport | undefined): void {
  active = transport;
}

export function getNativeTransport(): NativeTransport | undefined {
  return active;
}
