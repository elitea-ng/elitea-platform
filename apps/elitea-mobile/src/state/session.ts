import { useSyncExternalStore } from 'react';

/**
 * The session: which server we talk to, with which credential, in which
 * project. A small external store (no dependency) so that non-React code (the
 * fetch middleware, `endSession`, later the SSE client) can read and change it,
 * and React reads it through `useSession`.
 *
 * The bearer secret is deliberately NOT part of the observable state: React
 * components only ever see the credential's kind. Code that must attach it
 * reads it through `getBearerToken()`.
 */

export type CredentialKind = 'pat' | 'oauth';

/** Why a session ended. Every exit goes through `endSession`. */
export type EndReason =
  | 'user_logout'
  | 'device_revoked'
  | 'credential_rejected'
  | 'server_switch'
  | 'policy'
  | 'reset';

export interface UpgradeRequired {
  /** Minimum client version the server asked for, when it said. */
  minVersion?: string;
}

export interface SessionState {
  /** True once launch-time storage work is done and routing may begin. */
  hydrated: boolean;
  /** Normalised origin of the workspace server (`https://host[:port]`), or null. */
  serverOrigin: string | null;
  /** The active workspace slot (src/security/workspaceRegistry.ts), set together with `serverOrigin`. */
  workspaceId: string | null;
  /** The kind of credential in use, or null when signed out. */
  credential: { kind: CredentialKind } | null;
  /** The selected project id, or null until one is chosen. */
  projectId: number | null;
  /** True while `endSession` is running: the UI shows "Signing out…", requests are refused. */
  ending: boolean;
  /** True while the app switches between connected workspaces (nothing is wiped). */
  switching: boolean;
  /**
   * The workspace that was active before "Add workspace" put it aside: the
   * connect screen offers to go back to it. Null otherwise.
   */
  returnWorkspaceId: string | null;
  /** Why the last session ended, for a one-line notice on the next screen. */
  endReason: EndReason | null;
  /** The origin of the workspace that ended (for that notice), or null. */
  endedOrigin: string | null;
  /** Set when the server answered 426; the root layout shows a blocking screen. */
  upgradeRequired: UpgradeRequired | null;
  /**
   * Non-secret hint of the last origin, kept in memory after a
   * `credential_rejected` end so the user can sign in again quickly.
   */
  lastOriginHint: string | null;
  /**
   * An origin the user chose to switch to (a connect link while connected).
   * Nothing is stored for it: the connect screen checks it and asks for
   * confirmation like any other address.
   */
  pendingConnectOrigin: string | null;
}

export const INITIAL_SESSION: Readonly<SessionState> = Object.freeze({
  hydrated: false,
  serverOrigin: null,
  workspaceId: null,
  credential: null,
  projectId: null,
  ending: false,
  switching: false,
  returnWorkspaceId: null,
  endReason: null,
  endedOrigin: null,
  upgradeRequired: null,
  lastOriginHint: null,
  pendingConnectOrigin: null,
});

let state: SessionState = { ...INITIAL_SESSION };
let bearerToken: string | null = null;
const listeners = new Set<() => void>();

function emit() {
  for (const listener of [...listeners]) listener();
}

export function getSession(): SessionState {
  return state;
}

export function setSession(patch: Partial<SessionState>): void {
  let changed = false;
  for (const key of Object.keys(patch) as (keyof SessionState)[]) {
    if (!Object.is(state[key], patch[key])) {
      changed = true;
      break;
    }
  }
  if (!changed) return;
  state = { ...state, ...patch };
  emit();
}

export function subscribeSession(listener: () => void): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

/** Sets the signed-in credential. The secret stays outside the observable state. */
export function setCredential(credential: { kind: CredentialKind; token: string }): void {
  bearerToken = credential.token;
  setSession({ credential: { kind: credential.kind } });
}

/** The secret to put in `Authorization: Bearer …`, or null when signed out. */
export function getBearerToken(): string | null {
  return bearerToken;
}

/**
 * Back to the signed-out, disconnected state. `hydrated` stays true (the app is
 * running); `endReason` and `lastOriginHint` are set by the caller.
 */
export function resetSession(
  patch: Pick<SessionState, 'endReason' | 'lastOriginHint'> &
    Partial<Pick<SessionState, 'pendingConnectOrigin' | 'endedOrigin' | 'switching' | 'returnWorkspaceId'>>,
): void {
  bearerToken = null;
  state = { ...INITIAL_SESSION, hydrated: state.hydrated, ...patch };
  emit();
}

/** React binding. Pass a selector to re-render only when that slice changes. */
export function useSession(): SessionState;
export function useSession<T>(selector: (session: SessionState) => T): T;
export function useSession<T>(selector?: (session: SessionState) => T): T | SessionState {
  const read = () => (selector ? selector(state) : state);
  return useSyncExternalStore(subscribeSession, read, read);
}

/** Test helper: restores the pristine module state. */
export function __resetSessionForTests(): void {
  bearerToken = null;
  state = { ...INITIAL_SESSION };
  listeners.clear();
}
