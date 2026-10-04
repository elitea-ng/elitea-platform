import { type ReactNode, useMemo, useState } from 'react';

import { QueryClientProvider } from '@tanstack/react-query';

import { I18nProvider } from '@/shared/i18n';
import { getConfig } from '@/shared/config';
import { resolveBrandPack } from '@/shared/brand';
import {
  createNoopSocketClient,
  createSocketClient,
  SocketClientContext,
} from '@/shared/api/socket/client';

import { AppErrorBoundary } from './ErrorBoundary';
import { BrandThemeProvider } from './BrandThemeProvider';
import { createAppQueryClient } from './queryClient';

export interface AppProvidersProps {
  children: ReactNode;
}

/**
 * The app-wide provider tree (spec §3.2 "composition root ONLY: providers,
 * router assembly, query client, error boundary"; §9.3 unit R2).
 *
 * `App.tsx` renders this component ONLY after `shared/config`'s (unit F3)
 * gate passes — the `config.status === 'missing'` branch returns
 * `MissingEnvPage` and returns BEFORE `AppProviders` (and therefore every
 * provider below) ever mounts. That ordering is unchanged by this unit, not
 * merely reproduced alongside new code: see `App.tsx`.
 *
 * Nesting order, outermost to innermost, and the reasoning behind each step:
 *
 *  1. `AppErrorBoundary` — outermost of everything this unit owns, so it can
 *     catch a render-time error thrown by ANY provider below it (a theme
 *     build bug, an i18next init failure that throws during render, …), not
 *     only errors from `children`. The trade-off, taken deliberately: the
 *     fallback UI it renders cannot lean on `ThemeProvider` (which might be
 *     the very thing that failed) or on the `I18nProvider` context, so it is
 *     unstyled, dependency-free markup — the same posture
 *     `shared/config/ui/MissingEnvPage.tsx` already takes for the
 *     config-gate case one level up. `t()` still resolves correctly inside
 *     the fallback even though `I18nProvider` is mounted further in: S8's
 *     `t()` reads the configured i18next singleton directly and needs no
 *     provider ancestor (see `ErrorBoundary.tsx`).
 *  2. `BrandThemeProvider` — MUI's `ThemeProvider` + `CssBaseline` + the
 *     `InitColorSchemeScript` anti-flash script (spec §4.2, T1's snippet).
 *     Placed ahead of i18n and the query client so every descendant —
 *     including any provider-level loading/dev UI a later unit adds — is
 *     themed from the first commit.
 *  3. `I18nProvider` — S8's real `I18nextProvider`. Placed ahead of the
 *     query client because Wave-2 query-driven loading/error UI will want
 *     translated copy; there is no cost to having the provider available one
 *     level higher than its first real consumer.
 *  4. `QueryClientProvider` — innermost. TanStack Query's context has no
 *     rendering or styling dependency on anything above it, and it is the
 *     layer structurally closest to where `features/*` hooks actually read
 *     it (§2.3).
 *
 * The `QueryClient` itself is constructed once per mounted tree via a lazy
 * `useState` initializer, not at module scope (see `queryClient.ts`): every
 * test render gets a cache-isolated client, and React 19 StrictMode's
 * double-invocation of the initializer function is harmless because only
 * the first call's result is kept as state.
 */
export function AppProviders({ children }: AppProvidersProps) {
  const [queryClient] = useState(createAppQueryClient);

  /**
   * Channel C, resolved once per mounted tree (issue #136 C). This provider
   * used to render `<BrandThemeProvider>` with no `pack`, so the compiled
   * `DEFAULT_BRAND_PACK` always won and the pack elitea-main serves at
   * `/api/v2/branding/bootstrap.js` reached nothing — exactly the "later
   * concern" `BrandThemeProvider.tsx`'s prop doc described. `resolveBrandPack()`
   * returns that same default when no valid served pack exists, so the
   * no-pack deployment renders byte-identically to before.
   *
   * A `useState` initializer, not a `useMemo`: the pack is fixed for the
   * lifetime of the document (the global is written by a blocking script in
   * `index.html` before the bundle runs), and a stable object identity is
   * what keeps `BrandThemeProvider`'s `useMemo(..., [pack])` from rebuilding
   * the whole theme on unrelated re-renders.
   */
  const [brandPack] = useState(resolveBrandPack);

  // Create the socket client once per mount. When VITE_SOCKET_SERVER is absent
  // or empty (E2E compose, offline dev), a no-op client is provided so that
  // useSocketClient() callers render in degraded-but-functional state instead
  // of throwing "no SocketClientContext.Provider is mounted".
  const socketClient = useMemo(() => {
    const cfg = getConfig();
    if (cfg.status !== 'ok') return createNoopSocketClient();
    const url = cfg.config.vite_socket_server;
    const path = cfg.config.vite_socket_path;
    if (!url) return createNoopSocketClient();
    return createSocketClient({ url, ...(path !== undefined ? { path } : {}) });
  }, []);

  return (
    <AppErrorBoundary>
      <BrandThemeProvider pack={brandPack}>
        <I18nProvider>
          <QueryClientProvider client={queryClient}>
            <SocketClientContext.Provider value={socketClient}>
              {children}
            </SocketClientContext.Provider>
          </QueryClientProvider>
        </I18nProvider>
      </BrandThemeProvider>
    </AppErrorBoundary>
  );
}
