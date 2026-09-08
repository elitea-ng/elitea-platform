import { StrictMode } from 'react';

import { createRoot } from 'react-dom/client';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { adminApiBaseUrl } from '@/pages/admin/adminUiConfig';

import { AdminApp } from './AdminApp';

/**
 * Admin build entry (spec §7.4, contract C15). Served by the Go adminui handler
 * (services/elitea-main/internal/api/adminui/handler.go), which replaces the
 * `<!-- admin_ui_config -->` marker in this entry's index.html with a script
 * defining `window.admin_ui_config`.
 *
 * Unit A14 replaced this entry's placeholder — a role-permission toy backed by
 * sessionStorage, with a hand-rolled `window.location.pathname` switch — with
 * the real admin route group in `src/pages/admin/`. `window.admin_ui_config` is
 * now read by `pages/admin/adminUiConfig.ts`, which also owns the `Window`
 * augmentation this file used to declare, and documents why its `permissions`
 * array is presentation state and never authorisation.
 *
 * Two things this entry does that the main app's `App.tsx` does differently,
 * both because the admin bundle is served by a different handler:
 *
 *  - The API base comes from `window.admin_ui_config.vite_server_url`, not from
 *    `shared/config`'s `getConfig()` — that reads `window.elitea_ui_config` and
 *    the `VITE_*` build env, neither of which the adminui handler injects. So
 *    there is no `MissingEnvPage` gate here; the base falls back to `/api/v2`,
 *    which is exactly what the handler passes in production.
 *  - The router is code-based (`pages/admin/router.tsx`) with `basepath`
 *    `/admin/app`, since the TanStack Router vite plugin generates a route tree
 *    for the main app target only.
 *
 * `AdminApp` (`./AdminApp.tsx`, beside this file) is what actually composes
 * `AppProviders` and the router now, rather than this file doing both inline —
 * it also runs the boot-time access gate that keeps the router from mounting
 * at all for a caller with no administration-mode permission. `AppProviders`
 * is shared with the main app unchanged: error boundary, MUI theme, i18n,
 * query client. Its socket client degrades to the no-op one when no socket
 * config resolves, which is the admin bundle's normal state.
 */

configureGeneratedClient({ baseUrl: adminApiBaseUrl() });

const container = document.getElementById('root');
if (!container) {
  throw new Error('elitea-web admin: #root container missing from index.html');
}

createRoot(container).render(
  <StrictMode>
    <AdminApp />
  </StrictMode>,
);
