/**
 * Start the real Elitea app inside the desktop shell, once the host reports a
 * signed-in session.
 *
 * Order matters and is the whole job of this file:
 *  1. register the native transport (bearer + fetch-based SSE + the host's
 *     CORS-free network layer), so the first request the app makes already
 *     carries a token;
 *  2. publish the absolute-URL runtime config the app reads at boot;
 *  3. only then import `App` — a dynamic import, so none of it evaluates
 *     before 1 and 2, and the app's chunks load only after sign-in.
 */
import { fetch as hostFetch } from '@tauri-apps/plugin-http';
import { StrictMode } from 'react';
import { createRoot, type Root } from 'react-dom/client';

import { setNativeTransport, type NativeSignOutReason } from '@/shared/api/nativeTransport';
import { desktopRuntimeConfig, readPublicProjectId } from '@/shared/desktop/deploymentConfig';
import type { HostBridge, HostState } from '@/shared/desktop/hostBridge';
import { createHostTransport } from '@/shared/desktop/hostTransport';

export interface LaunchOptions {
  bridge: HostBridge;
  state: HostState;
  container: HTMLElement;
  onSignedOut: (reason: NativeSignOutReason) => void;
  onUpgradeRequired: () => void;
}

export async function launchApp(options: LaunchOptions): Promise<Root> {
  const { bridge, state, container } = options;
  if (state.origin === null) throw new Error('desktop: launchApp needs a configured deployment origin');

  setNativeTransport(
    createHostTransport({
      bridge,
      clientVersion: state.clientVersion,
      fetch: hostFetch,
      onSignedOut: options.onSignedOut,
      onUpgradeRequired: options.onUpgradeRequired,
    }),
  );
  const config = desktopRuntimeConfig(state.origin, await readPublicProjectId(hostFetch, state.origin));
  (globalThis as { elitea_ui_config?: unknown }).elitea_ui_config = config;

  const { App } = await import('@/app/App');
  const root = createRoot(container);
  root.render(
    <StrictMode>
      <App />
    </StrictMode>,
  );
  return root;
}
