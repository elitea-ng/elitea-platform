/**
 * Start the real Elitea app inside the desktop shell, once the host reports a
 * signed-in session.
 *
 * Order matters and is the whole job of this file:
 *  1. register the native transport (bearer + fetch-based SSE + the host's
 *     pooled network layer, `shared/desktop/hostFetch.ts`), so the first
 *     request the app makes already carries a token — and start reading that
 *     token now, beside 2–3, rather than after them;
 *  2. publish the absolute-URL runtime config the app reads at boot;
 *  3. publish the deployment's brand pack (`shared/desktop/brandPack.ts`) —
 *     the app resolves its pack once, at mount, from `window.elitea_brand`;
 *  4. only then import `App` — a dynamic import, so none of it evaluates
 *     before 1–3, and the app's chunks load only after sign-in.
 */
import { StrictMode } from 'react';
import { createRoot, type Root } from 'react-dom/client';

import { setNativeTransport, type NativeSignOutReason } from '@/shared/api/nativeTransport';
import { loadDesktopBrand } from '@/shared/desktop/brandPack';
import { desktopRuntimeConfig, readPublicProjectId } from '@/shared/desktop/deploymentConfig';
import { hostLog, withRequestLogging } from '@/shared/desktop/diagnostics';
import { tauriInvoke, type HostBridge, type HostState } from '@/shared/desktop/hostBridge';
import { installExternalLinks } from '@/shared/desktop/externalLinks';
import { createHostFetch, type RawInvoke } from '@/shared/desktop/hostFetch';
import { createHostTransport } from '@/shared/desktop/hostTransport';

export interface LaunchOptions {
  bridge: HostBridge;
  state: HostState;
  container: HTMLElement;
  onSignedOut: (reason: NativeSignOutReason) => void;
  onUpgradeRequired: () => void;
}

/**
 * The host's fetch (`hostFetch.ts`): CORS-free, on one pooled connection, and
 * never following a redirect — the host refuses to, so a redirect cannot carry
 * the bearer token to wherever the server (or an attacker on its path) points.
 */
function desktopFetch(): ReturnType<typeof createHostFetch> {
  const invoke = tauriInvoke();
  if (invoke === undefined) throw new Error('desktop: launchApp needs the Tauri host');
  // Tauri's invoke takes a typed array as a raw body, and `headers` (the request metadata).
  return withRequestLogging(createHostFetch(invoke as RawInvoke));
}

export async function launchApp(options: LaunchOptions): Promise<Root> {
  const { bridge, state, container } = options;
  if (state.origin === null) throw new Error('desktop: launchApp needs a configured deployment origin');

  const noRedirectFetch = desktopFetch();
  const transport = createHostTransport({
    bridge,
    clientVersion: state.clientVersion,
    fetch: noRedirectFetch,
    origin: state.origin,
    onSignedOut: options.onSignedOut,
    onUpgradeRequired: options.onUpgradeRequired,
  });
  setNativeTransport(transport);
  // The app's first requests all need the token: read it (the host started
  // the launch refresh in its setup) beside the public reads below, not after.
  void transport.accessToken().catch(() => undefined);
  const [publicProjectId] = await Promise.all([
    readPublicProjectId(noRedirectFetch, state.origin),
    // Branding never blocks the launch: a failure leaves the compiled default.
    loadDesktopBrand({ fetch: noRedirectFetch, origin: state.origin }).catch(() => undefined),
  ]);
  hostLog('debug', `launch: public project "${publicProjectId}"`, 'boot');
  const config = desktopRuntimeConfig(state.origin, publicProjectId);
  (globalThis as { elitea_ui_config?: unknown }).elitea_ui_config = config;

  installExternalLinks(bridge, state.origin);

  const { App } = await import('@/app/App');
  hostLog('debug', `launch: app loaded, rendering (${Math.round(performance.now())} ms after page start)`, 'boot');
  const root = createRoot(container);
  root.render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
  return root;
}
