/**
 * The desktop `NativeTransport`: bearer auth from the Tauri host, fetch-based
 * SSE, and the host's network layer (ADR-0029 decision 9).
 *
 * Token handling. The host holds the refresh token in the OS keychain and
 * hands the webview an access token that lives 15 minutes. This module caches
 * it in memory only (never web storage — the logout sweep cannot reach what is
 * not there) and renews it a little early. Refresh tokens ROTATE, so
 * `refresh()` is single-flight and idempotent for a stale caller: a request
 * that was refused with an old token finds the new one already in the cache
 * and skips a second exchange.
 */
import type { NativeSignOutReason, NativeTransport, RefreshOutcome } from '@/shared/api/nativeTransport';

import { fetchEventSourceFactory } from './fetchEventSource';
import type { HostBridge } from './hostBridge';
import { clearAllLocalData } from './localData';

/** Renew this long before the token's stated expiry, so a request never races it. */
const EXPIRY_SKEW_MS = 30_000;

export interface HostTransportOptions {
  bridge: HostBridge;
  /** The connected deployment origin; the bearer goes nowhere else. */
  origin: string;
  /** Clears the webview's own storage; default: the full sweep in `localData.ts`. */
  clearLocalData?: () => Promise<void>;
  /** `X-Client-Version`, sent on every request (the 426 gate is per client id). */
  clientVersion: string;
  /** The network layer: the host's CORS-free fetch in the app, a stub in tests. */
  fetch?: typeof fetch;
  /** Called after the host has dropped the session; the shell returns to the connect screen. */
  onSignedOut: (reason: NativeSignOutReason) => void;
  onUpgradeRequired?: () => void;
  now?: () => number;
}

export function createHostTransport(options: HostTransportOptions): NativeTransport {
  const { bridge, onSignedOut } = options;
  const now = options.now ?? Date.now;
  let cached: { token: string; expiresAtMs: number } | undefined;
  let tokenInFlight: Promise<string | undefined> | undefined;
  let refreshInFlight: Promise<RefreshOutcome> | undefined;
  let signedOut = false;

  const load = async (): Promise<string | undefined> => {
    const result = await bridge.accessToken();
    if (result === null) {
      cached = undefined;
      return undefined;
    }
    cached = { token: result.token, expiresAtMs: now() + result.expiresIn * 1000 - EXPIRY_SKEW_MS };
    return result.token;
  };

  const accessToken = async (): Promise<string | undefined> => {
    if (signedOut) return undefined;
    if (cached !== undefined && cached.expiresAtMs > now()) return cached.token;
    tokenInFlight ??= load().finally(() => {
      tokenInFlight = undefined;
    });
    return tokenInFlight;
  };

  const refresh = (failedToken: string | undefined): Promise<RefreshOutcome> => {
    if (signedOut) return Promise.resolve('ended');
    if (cached !== undefined && failedToken !== undefined && cached.token !== failedToken && cached.expiresAtMs > now()) {
      return Promise.resolve('refreshed');
    }
    refreshInFlight ??= bridge
      .refresh()
      .then(async (outcome): Promise<RefreshOutcome> => {
        cached = undefined;
        if (outcome === 'upgrade_required') {
          // Not a session failure: the build is too old. Tell the shell, keep the session.
          options.onUpgradeRequired?.();
          return 'unavailable';
        }
        if (outcome !== 'refreshed') return outcome;
        return (await load()) === undefined ? 'ended' : 'refreshed';
      })
      // The IPC call itself failing is not the server refusing the session.
      .catch((): RefreshOutcome => 'unavailable')
      .finally(() => {
        refreshInFlight = undefined;
      });
    return refreshInFlight;
  };

  const clearLocalData = options.clearLocalData ?? clearAllLocalData;

  const signOut = (reason: NativeSignOutReason): void => {
    if (signedOut) return;
    signedOut = true;
    cached = undefined;
    // The session is already dead server-side (device revoked, or a refresh
    // token that no longer works), so there is nothing to revoke: the host
    // forgets it and wipes the keychain, the policy file and (authoritatively)
    // the webview's browsing data. The page's own sweep runs first.
    void clearLocalData()
      .catch(() => undefined)
      .then(() => bridge.wipe())
      .catch(() => undefined)
      .finally(() => onSignedOut(reason));
  };

  /** The person chose to log out: revoke server-side, forget, wipe, reload. */
  const logout = async (): Promise<void> => {
    if (signedOut) return;
    signedOut = true;
    cached = undefined;
    await clearLocalData().catch(() => undefined);
    await bridge.signOut().catch(() => undefined);
    onSignedOut('logout');
  };

  const base = {
    accessToken,
    refresh,
    signOut,
    logout,
    origin: options.origin,
    headers: { 'X-Client-Version': options.clientVersion },
    ...(options.fetch !== undefined ? { fetch: options.fetch } : {}),
    ...(options.onUpgradeRequired !== undefined ? { onUpgradeRequired: options.onUpgradeRequired } : {}),
  } satisfies NativeTransport;

  return { ...base, createEventSource: fetchEventSourceFactory({ transport: base }) };
}
