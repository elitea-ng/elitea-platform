/**
 * The admin bundle's composition root (smoke finding: a non-admin who opened
 * `/admin/app` saw the whole console shell rendered around an empty sidebar —
 * `router.tsx` mounted the route tree unconditionally, so `/admin/app/` and
 * every one of its eleven pages rendered for anyone the Go handler served the
 * SPA to, which is anyone at all: `adminui.Handler.ServeSPA` sits on the root
 * mux with no auth middleware in front of it — see `services/elitea-main/
 * internal/api/router.go`'s admin-mount comment. Every WRITE still came back
 * 403 from the server; the page never told a non-admin caller that before now.
 *
 * Extracted out of `main.tsx` so a real DOM test can mount exactly what
 * production renders, gate included — `main.tsx` itself is untestable, it
 * calls `createRoot(...).render(...)` at import time. Lives beside `main.tsx`
 * in `entries/admin/`, not in `pages/admin/`: it composes `@/app/providers`,
 * and the layer-cycle gate's `no-upward-from-pages` rule forbids `pages/*`
 * importing `app/*` — `entries/*` sits outside that layer chain, which is
 * exactly why `main.tsx` could always import `AppProviders` directly.
 *
 * `hasAnyAdminNavAccess()` reads the same server-injected, presentation-only
 * `admin_ui_config.permissions` the sidebar already filters on (see
 * `pages/admin/adminNavItems.ts` and `pages/admin/adminUiConfig.ts`'s headers
 * for why that is safe to gate RENDERING on: the server is still the only
 * thing that gates a mutation). The router is built lazily, and only when
 * access is granted, so a refused caller never even constructs the route
 * tree.
 */
import { lazy, Suspense, useState } from 'react';

import { RouterProvider } from '@tanstack/react-router';

import { AppProviders } from '@/app/providers';
import { hasAnyAdminNavAccess } from '@/pages/admin/adminNavItems';
import { AdminRefusalLazy } from '@/pages/admin/AdminRefusalLazy';
import { adminUiAccess, adminUiUserId } from '@/pages/admin/adminUiConfig';
import { createAdminRouter } from '@/pages/admin/router';

/**
 * The console has no theme control of its own: it follows the user's stored
 * preference, as the app does (`app/App.tsx`). The local copy (`el-mode`) is
 * shared already, the console being on the app's origin; this reads and
 * follows the server copy, so a choice made on another device applies here
 * too. A lazy chunk, as in the app.
 */
const ThemePreferenceSync = lazy(() => import('@/app/ThemePreferenceSync'));

/**
 * What the boot gate shows. The handler's `access` reason decides when it was
 * injected; an older handler (or dev/tests) injects none, and then the
 * permission list alone decides, as it always did.
 */
function resolveGate(): 'router' | 'denied' | 'unauthenticated' | 'unavailable' {
  const access = adminUiAccess();
  if (access === undefined) return hasAnyAdminNavAccess() ? 'router' : 'denied';
  return access === 'granted' ? 'router' : access;
}

export function AdminApp() {
  const [gate] = useState(resolveGate);
  const [router] = useState(() => (gate === 'router' ? createAdminRouter() : null));
  const [userId] = useState(adminUiUserId);

  return (
    <AppProviders>
      <Suspense fallback={null}>
        {router && userId !== undefined && <ThemePreferenceSync key={userId} userId={userId} />}
        {router ? <RouterProvider router={router} /> : <AdminRefusalLazy gate={gate === 'router' ? 'denied' : gate} />}
      </Suspense>
    </AppProviders>
  );
}
