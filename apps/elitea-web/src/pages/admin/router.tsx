/**
 * The admin route group (unit A14) — mounted by `src/entries/admin/main.tsx`.
 *
 * CODE-based TanStack Router, not file-based. `vite.config.ts` registers the
 * `tanstackRouter` plugin for the MAIN app target only ("admin and maintenance
 * are plain single-entry SPAs with no route tree"), so there is no
 * `routeTree.gen.ts` for this bundle to import and no `src/routes/**` scan.
 * `createRootRoute`/`createRoute` build the same router by hand, which is the
 * supported alternative and keeps the admin bundle's route set out of the main
 * app's generated tree.
 *
 * `basepath` is `/admin/app`, matching `vite.config.ts`'s `base` and the Go
 * adminui handler's `BasePath` — the handler serves this SPA at that prefix and
 * rewrites its asset URLs to match.
 *
 * All eleven reachable pages exist: Users, Audit Trail, Roles, Projects,
 * Secrets, Schedules & Tasks, App Requests, Configuration, Service Descriptors,
 * Features and LLM Governance. Issue #200 scopes ELEVEN; the eleventh is LiteLLM, which is not
 * ported because #201 replaces LiteLLM with Bifrost — porting an admin page for
 * a subsystem being removed would be building a control for something on its
 * way out. `adminNav.ts` therefore has no LiteLLM entry either, and its test
 * derives the legal nav targets from THIS route table so the two cannot drift.
 *
 * The index route RENDERS Users rather than redirecting to `/users`: a `redirect()`
 * here would be type-checked against the MAIN app's generated route ids (the
 * `Register` interface `routeTree.gen.ts` declares is global), which this
 * hand-built tree is not part of. Rendering also spares the extra history entry.
 *
 * ── Code splitting (issue #493) ──────────────────────────────────────────────
 * Every page below is loaded with `lazyRouteComponent`, so each one becomes its
 * own chunk that the browser fetches on navigation. Before this, all eleven
 * pages were static imports, the whole admin application was one 895 KiB gzip
 * entry chunk, and the bundle-budget gate (spec §3.5, 300 KiB initial) failed.
 * The main app gets the same result from `autoCodeSplitting` in the router vite
 * plugin; that plugin reads `src/routes/**` and cannot see this hand-built tree,
 * so the split is written out here.
 *
 * `AdminLayout` stays a STATIC import. It is the root route's component — the
 * nav plus the `<Outlet/>` — so it is on screen for every route, and a lazy load
 * of it would only delay the first paint. `AdminRoutePending` below is static
 * for the same reason: it is what the shell shows while a page chunk is in
 * flight, so a lazy fallback could not render.
 */
import {
  createRootRoute,
  createRoute,
  createRouter,
  lazyRouteComponent,
  type AnyRouter,
} from '@tanstack/react-router';

import { t } from '@/shared/i18n';

import { AdminLayout } from './AdminLayout';

const ADMIN_BASE_PATH = '/admin/app';

/**
 * The fallback the `<Outlet/>` shows while a page chunk loads. Same shape as
 * the main app's `routes/-ui/RouteStatus.tsx` `RoutePending`: `<output>` carries
 * an implicit `role="status"`, which is what `jsx-a11y/prefer-tag-over-role`
 * asks for and what Testing Library resolves `getByRole('status')` against.
 */
function AdminRoutePending() {
  return <output>{t('routes.pending', 'Loading…')}</output>;
}

/*
 * One lazy component per page. The second argument names the export to take
 * from the module — every admin page uses a named export, not a default one.
 * `AdminUsers` is bound once and shared by the index and `/users` routes, so
 * both resolve the same chunk and the second navigation is already loaded.
 */
const AdminUsers = lazyRouteComponent(() => import('./Users'), 'AdminUsers');
const AdminAuditTrail = lazyRouteComponent(() => import('./AuditTrail'), 'AdminAuditTrail');
const AdminRoles = lazyRouteComponent(() => import('./Roles'), 'AdminRoles');
const AdminProjects = lazyRouteComponent(() => import('./Projects'), 'AdminProjects');
const AdminSecrets = lazyRouteComponent(() => import('./Secrets'), 'AdminSecrets');
const AdminConfiguration = lazyRouteComponent(() => import('./Configuration'), 'AdminConfiguration');
const AdminFeatures = lazyRouteComponent(() => import('./Features'), 'AdminFeatures');
const AdminSchedulesTasks = lazyRouteComponent(
  () => import('./SchedulesTasks'),
  'AdminSchedulesTasks',
);
const AdminAppRequests = lazyRouteComponent(() => import('./AppRequests'), 'AdminAppRequests');
const AdminServiceDescriptors = lazyRouteComponent(
  () => import('./ServiceDescriptors'),
  'AdminServiceDescriptors',
);
const AdminToolkitTypes = lazyRouteComponent(() => import('./ToolkitTypes'), 'AdminToolkitTypes');
const AdminGatewayGovernance = lazyRouteComponent(
  () => import('./GatewayGovernance'),
  'AdminGatewayGovernance',
);
const AdminBudgets = lazyRouteComponent(() => import('./Budgets'), 'AdminBudgets');
const AdminBranding = lazyRouteComponent(() => import('./Branding'), 'AdminBranding');
const AdminEmail = lazyRouteComponent(() => import('./Email'), 'AdminEmail');

/**
 * The root route renders `AdminLayout` — the nav plus an `<Outlet/>` — rather
 * than the bare `<Outlet/>` it carried until issue #225. All ten pages were
 * reachable only by typing a URL; nothing in the SPA linked any of them.
 */
const rootRoute = createRootRoute({ component: AdminLayout });

const indexRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: AdminUsers,
});

const usersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/users',
  component: AdminUsers,
});

const auditTrailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/audit',
  component: AdminAuditTrail,
});

const rolesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/roles',
  component: AdminRoles,
});

const projectsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/projects',
  component: AdminProjects,
});

const secretsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/secrets',
  component: AdminSecrets,
});

const configurationRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/configuration',
  component: AdminConfiguration,
});

const featuresRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/features',
  component: AdminFeatures,
});

const schedulesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/schedules',
  component: AdminSchedulesTasks,
});

const appRequestsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/app-requests',
  component: AdminAppRequests,
});

/**
 * The path is `/service-descriptors`. The reference SPA has no route for this
 * page at all — `routes.js` never declares one and nothing imports
 * `ServiceDescriptorsPage`, so that file is dead code there; the surface an
 * operator reaches is the sibling section embedded in Configuration. This route
 * gives the page a home, and the Configuration section states the same
 * server-declared reason.
 */
const serviceDescriptorsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/service-descriptors',
  component: AdminServiceDescriptors,
});

/**
 * Admin › Toolkits (shared migration 0114). The path is `/toolkits`, and the
 * page decides which toolkit TYPES this platform offers and to which projects.
 * The reference SPA has no counterpart: the legacy platform's only operator
 * control over the toolkit catalogue is the guardrails deny-list, which is a
 * section of the Configuration page.
 */
const toolkitTypesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/toolkits',
  component: AdminToolkitTypes,
});

/**
 * Admin › LLM Governance (#218). The Configuration page's LLM Governance
 * section has always pointed the operator at `/admin/gateway/governance` — an
 * elitea-main REST route with no screen behind it in this SPA. This is that
 * screen; the REST path is unchanged and is what the page calls.
 */
const governanceRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/governance',
  component: AdminGatewayGovernance,
});

/**
 * Admin › Budgets (gap G4). The per-project and per-member LLM spend limits.
 * The REST routes have existed since #246 and had no caller at all; this is the
 * screen behind them, and the only writer of `gateway.project_budget`'s
 * `budget_period` and `nats_fail_mode` columns.
 */
const budgetsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/budgets',
  component: AdminBudgets,
});

/**
 * Admin › Branding (ADR-0024 WP4). The `branding` section of the platform
 * configuration edited as what it is — a brand pack — with a live preview,
 * asset uploads and font faces. The Configuration page keeps the section's
 * row and points here instead of rendering the generic form over the same
 * keys.
 */
const brandingRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/branding',
  component: AdminBranding,
});

/**
 * Admin › E-mail (gap G7). The relay this deployment sends invitations and
 * notices through. The Configuration page keeps the section's row and points
 * here, and both render the same editor component so they cannot drift.
 */
const emailRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/email',
  component: AdminEmail,
});

const adminRouteTree = rootRoute.addChildren([
  brandingRoute,
  emailRoute,
  indexRoute,
  usersRoute,
  auditTrailRoute,
  rolesRoute,
  projectsRoute,
  secretsRoute,
  schedulesRoute,
  appRequestsRoute,
  configurationRoute,
  serviceDescriptorsRoute,
  toolkitTypesRoute,
  featuresRoute,
  governanceRoute,
  budgetsRoute,
]);

export function createAdminRouter(): AnyRouter {
  return createRouter({
    routeTree: adminRouteTree,
    basepath: ADMIN_BASE_PATH,
    defaultPreload: 'intent',
    /*
     * Shown from the first frame a page chunk is missing, not after a delay:
     * `defaultPendingMs` defaults to 1000 ms, which would leave the content
     * area blank for a second on a cold load of any page (issue #493).
     * `defaultPendingMinMs` is 0 so a warm, already-loaded chunk does not hold
     * the fallback on screen for its own default of 500 ms.
     */
    defaultPendingComponent: AdminRoutePending,
    defaultPendingMs: 0,
    defaultPendingMinMs: 0,
  });
}
