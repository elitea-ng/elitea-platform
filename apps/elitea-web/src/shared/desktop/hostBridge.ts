/**
 * The typed seam to the Tauri host (`apps/elitea-desktop/src-tauri`).
 *
 * Calls go through `window.__TAURI_INTERNALS__.invoke`, which Tauri injects
 * into the bundled webview, rather than the `@tauri-apps/api` package: the
 * surface is seven commands, and one fewer dependency keeps the default web
 * build's lockfile and audit surface unchanged. The host exposes only
 * SHORT-LIVED ACCESS TOKENS; the refresh token never leaves the OS keychain.
 */

/** What the host knows about this install. Mirrors `HostState` in `src-tauri/src/commands.rs`. */
export interface HostState {
  /** A deployment origin is configured. */
  configured: boolean;
  origin: string | null;
  displayName: string | null;
  signedIn: boolean;
  clientVersion: string;
  clientId: string;
  /** The server-driven `native_client_policy`, stored verbatim (enforcement of `local_work` is later work). */
  policy: Record<string, unknown> | null;
}

/** The result of `GET /.well-known/elitea-client` reduced to what the connect screen shows. */
export interface HostDeployment {
  origin: string;
  displayName: string;
  deploymentKind: string;
}

interface HostAccessToken {
  token: string;
  /** Seconds the token stays valid, as the host measured it. */
  expiresIn: number;
}

export type HostInvoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

export interface HostBridge {
  state(): Promise<HostState>;
  connect(url: string): Promise<HostDeployment>;
  signIn(): Promise<HostState>;
  /** Abandon the sign-in waiting for the browser; the pending `signIn()` rejects. */
  cancelSignIn(): Promise<void>;
  accessToken(): Promise<HostAccessToken | null>;
  /** Force a refresh-token exchange. `ended`: the server refused the refresh token. `unavailable`: try again later. */
  refresh(): Promise<'refreshed' | 'ended' | 'unavailable' | 'upgrade_required'>;
  /** Revoke the device session server-side, then forget it locally. */
  signOut(): Promise<void>;
  /** Forget the session and wipe local data without contacting the server (`device_revoked`). */
  wipe(): Promise<void>;
  /** Open an http(s) URL in the system browser (the opener plugin). */
  openExternal(url: string): Promise<void>;
}

export function createHostBridge(invoke: HostInvoke): HostBridge {
  return {
    state: () => invoke<HostState>('host_state'),
    connect: (url) => invoke<HostDeployment>('host_connect', { url }),
    signIn: () => invoke<HostState>('host_sign_in'),
    cancelSignIn: () => invoke<void>('host_sign_in_cancel'),
    accessToken: () => invoke<HostAccessToken | null>('host_access_token'),
    refresh: () => invoke<'refreshed' | 'ended' | 'unavailable' | 'upgrade_required'>('host_refresh'),
    signOut: () => invoke<void>('host_sign_out'),
    wipe: () => invoke<void>('host_wipe'),
    openExternal: (url) => invoke<void>('plugin:opener|open_url', { url }),
  };
}

interface TauriInternals {
  invoke: HostInvoke;
}

/** The host's `invoke`, or `undefined` outside the Tauri webview (a plain browser tab). */
export function tauriInvoke(): HostInvoke | undefined {
  const internals = (globalThis as { __TAURI_INTERNALS__?: TauriInternals }).__TAURI_INTERNALS__;
  return internals?.invoke.bind(internals);
}
