/**
 * Router assembly (spec §2.3, §9.3 unit R1). `autoCodeSplitting: true` is
 * configured on the `@tanstack/router-plugin` vite plugin (`vite.config.ts`)
 * — this file just builds the `Router` instance from the generated route
 * tree, per-route `pendingComponent`/`errorComponent` are set on the leaf
 * route files themselves (task item 5: replaces the old app's one global
 * `<Suspense fallback={<LoadingPage/>}>`, `ProtectedRoutes.jsx:254`).
 *
 * `basepath`: spec §7.1 contract C3 ("Router basename = `VITE_BASE_URI`,
 * default `/app/`"), verified by this unit per C3's table. TanStack
 * Router's own option is named `basepath`, not `basename` (verified
 * against the installed `@tanstack/router-core@1.170.18`'s
 * `RouterOptions` — no `basename` field exists anywhere in the package;
 * `basepath` is the documented equivalent, "for mounting a router
 * instance at a subpath"). Sourced from unit R2's `getAppBasename()`
 * (`app/providers/basename.ts`) — landed after this unit started; R2's own
 * header flagged this exact call as "ready for whoever makes that edit",
 * so this wiring is that follow-up, not new R1-owned config logic
 * (`getAppBasename` itself, including its `import.meta.env.DEV` /
 * `MissingEnvPage`-can't-reach-this reasoning, stays R2's file).
 *
 * `context` is constructed with the R2-integration stub
 * (`router-context.ts`) — see that file's header. A real, session-backed
 * `auth` context (replacing `stubAuthContext`) is not yet available from
 * any landed unit (R2's Wave-1 scope is MUI/query/brand/i18n/error
 * boundary, not user/session state) — `<RouterProvider router={router} />`
 * (`app/App.tsx`) does not override `context`, so every guard runs against
 * the stub's safe "user not loaded" defaults until a session store lands.
 *
 * Route components arrive through `shared/lib/chunk-load-guard`'s
 * `lazyRouteComponent` (wired by `vite.config.ts`, pinned by
 * `router.lazy-guard.test.ts`): a chunk import cancelled because the page is
 * being navigated away from no longer makes the router reload the page and
 * replace that navigation.
 */
import { createRouter } from '@tanstack/react-router';

import { getAppBasename } from './providers';
import { routeTree } from '../routeTree.gen';
import { RouteError, RoutePending } from '../routes/-ui/RouteStatus';
import { stubAuthContext } from './router-context';
import type { RouterContext } from './router-context';

export function createAppRouter() {
  return createRouter({
    routeTree,
    basepath: getAppBasename(),
    context: { auth: stubAuthContext } satisfies RouterContext,
    defaultPendingComponent: RoutePending,
    defaultErrorComponent: RouteError,
    defaultPreload: 'intent',
    scrollRestoration: true,
  });
}

// Not exported (R-D1/knip): only this file's own `declare module`
// augmentation below references it, and TypeScript module augmentation
// does not require the referenced type to be part of the module's public
// surface. Add `export` back the moment a real external consumer (a test
// helper, a Storybook decorator) needs to name this type.
type AppRouter = ReturnType<typeof createAppRouter>;

declare module '@tanstack/react-router' {
  interface Register {
    router: AppRouter;
  }
}
