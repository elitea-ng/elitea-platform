import { RouterProvider } from '@tanstack/react-router';
import { useEffect, useState } from 'react';

import { getConfig, MissingEnvPage } from '@/shared/config';
import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { createAuthPopupController } from '@/shared/api/auth';
import { AUTH_CALLBACK_PATH } from '@/shared/api/auth/constants';
import { authPlaneFromProbeStatus, buildLoginUrl, loginPathForPlane } from '@/shared/api/auth/login-redirect';

import { useSelectedProjectStore } from '@/widgets/app-shell';

import { AppProviders, getAppBasename } from './providers';
import { createAppRouter } from './router';
import { sessionAuthContext, useSessionStore } from './session-store';

/**
 * The smallest gap between two primary session probes.
 *
 * `visibilitychange` and `focus` both fire on an ordinary tab switch, so an
 * unthrottled re-check would issue a burst on every window change.
 */
const SESSION_RECHECK_INTERVAL_MS = 60_000;

/**
 * App shell (spec §9.3 units F1/F3/R1/R2).
 *
 * Unit F3 wired the runtime-config gate: when any of the three required
 * config vars is missing or invalid, the shell renders MissingEnvPage
 * instead of the app — parity with old App.jsx:11 (MISSING_ENVS.length > 0
 * ? <EnvMissingPage/> : <RouterProvider/>). Unit R1 wires the TanStack
 * Router `RouterProvider` in on the non-missing branch, preserving that
 * exact gate: the router (and therefore every `beforeLoad` guard) never
 * mounts while required config is absent, matching the old app's ordering
 * exactly (`App.jsx:11` gates BEFORE `<RouterProvider>` construction). Unit
 * R2 preserves this gate unchanged: `MissingEnvPage` still returns BEFORE
 * `AppProviders` — and therefore before theme/i18n/query-client/error
 * boundary — ever mounts.
 *
 * `router` is created once per mount via `useState`'s lazy initializer
 * (not module scope — R-S2 discipline extends to router construction the
 * same way it does to zustand stores: no shared mutable singleton created
 * at import time in a file `app/` also imports).
 *
 * R2 INTEGRATION NOTE (read together with `./router.tsx` and
 * `../routes/__root.tsx`'s headers, both unit R1's files): R1's root route
 * documents wrapping `<Outlet/>` in `Providers` INSIDE `__root.tsx` as one
 * possible integration shape, but `src/routes/**` is R1's exclusive Wave-1
 * ownership (concurrent unit; not this unit's path to edit). `AppProviders`
 * is wired here instead, one level further out, around `<RouterProvider>`
 * itself. This is equivalent in coverage — everything the router renders,
 * including `router.tsx`'s `defaultPendingComponent`/`defaultErrorComponent`
 * (`RoutePending`/`RouteError`) and every route's own component, is a
 * descendant of `<RouterProvider>` and therefore of `AppProviders` — and it
 * needs no change to any file under `src/routes/**` or `src/app/router*`.
 *
 * NOT done here, and flagged rather than silently skipped: `router.tsx`'s
 * `createAppRouter()` does not yet call `basename` on `createRouter()` (spec
 * §7.1 C3), and `<RouterProvider>` below does not override the router's
 * stub `context.auth` (`./router-context.ts`) with a real session-backed
 * one. Both are real integration gaps this unit's task list (QueryClient /
 * ThemeProvider / I18nProvider / error boundary / exposing `getAppBasename`
 * / composing `AppProviders`) does not cover, and both require editing
 * `src/app/router.tsx`, which this unit does not own. `getAppBasename()`
 * (`./providers`) is ready for whoever makes that edit.
 */
export function App() {
  const config = getConfig();
  const [router] = useState(() => createAppRouter());
  const fetchSession = useSessionStore((state) => state.fetchSession);
  /**
   * ONE controller for the whole app lifetime (issue #136 B). A controller
   * created per render would defeat its single-flight slot — `flight` is
   * closure state — so it is built once by a lazy `useState` initializer,
   * the same R-S2 shape `router` above uses. Created before the config gate
   * below because hooks must be unconditional; construction itself touches
   * neither config nor the DOM.
   *
   * `basePath` is trimmed of the trailing slash `VITE_BASE_URI` carries
   * (`/app/`), because the callback path it is concatenated with already
   * starts with one — `/app//auth-callback` is a different URL and would not
   * match ROUTE-001.
   */
  const [authPopup] = useState(() =>
    createAuthPopupController({
      basePath: getAppBasename().replace(/\/$/, ''),
      /**
       * The login entry point differs per authentication plane, and the two
       * planes are mutually exclusive. A popup opened at the OIDC path on a
       * form-auth deployment shows a 404. The callback route therefore never
       * loads, and no result is ever posted. Every request behind the
       * single-flight slot stays pending until the user closes the window.
       *
       * A GETTER, not a value. This initializer runs before the first
       * `fetchSession()` resolves, so `probeStatus` is still undefined here
       * and `authPlaneFromProbeStatus(undefined)` answers `'oidc'`. The
       * getter runs once per flight, by which time the probe has answered.
       */
      loginPath: () =>
        loginPathForPlane(authPlaneFromProbeStatus(useSessionStore.getState().probeStatus)),
    }),
  );

  /**
   * Re-read the permission list when the user switches project, then re-run
   * every active `beforeLoad`.
   *
   * A permission list answers for ONE project. Without this the list read at
   * boot stays attached to the old project. `routes/-guards/requirePermission.ts`
   * then keeps deferring for the rest of the session. The guards go inert
   * again after the first switch.
   *
   * The boot fetch already reads the list for the first selection, so this
   * skips the run before the session is loaded.
   */
  const selectedProjectId = useSelectedProjectStore((state) => state.project?.id);
  const refreshPermissions = useSessionStore((state) => state.refreshPermissions);
  useEffect(() => {
    if (selectedProjectId === undefined) return;
    if (!useSessionStore.getState().loaded) return;
    void refreshPermissions().then(() => router.invalidate());
  }, [selectedProjectId, refreshPermissions, router]);

  /**
   * Re-run the PRIMARY session check when the tab comes back into view.
   *
   * The boot probe answers once. A session that expires while the page is
   * open is never noticed by it, which is the state the 1.60.0 smoke run
   * recorded: the user came back to a tab whose every request failed and
   * nothing moved the browser. The probe is the one call that can say
   * "expired" rather than "this request failed", and the session store
   * navigates on that answer.
   *
   * ONE PROBE PER MINUTE AT MOST. `visibilitychange` and `focus` both fire on
   * an ordinary tab switch, and a user moving between windows would otherwise
   * issue a burst.
   */
  useEffect(() => {
    let lastProbe = 0;
    const recheck = (): void => {
      if (document.visibilityState !== 'visible') return;
      if (!useSessionStore.getState().loaded) return;
      const now = Date.now();
      if (now - lastProbe < SESSION_RECHECK_INTERVAL_MS) return;
      lastProbe = now;
      void fetchSession();
    };
    window.addEventListener('focus', recheck);
    document.addEventListener('visibilitychange', recheck);
    return () => {
      window.removeEventListener('focus', recheck);
      document.removeEventListener('visibilitychange', recheck);
    };
  }, [fetchSession]);

  useEffect(() => {
    void fetchSession().then(() => {
      // Re-run all active beforeLoad guards now that the session is known.
      void router.invalidate();

      // Not logged in -> send the browser to the login form.
      //
      // Without this the app renders `<RoutePending />` forever: nothing gates
      // /app/** at the edge, so an unauthenticated deep link is served the SPA
      // shell, the session probe correctly reports "no session", every route
      // guard fails open, and the index guard returns {kind:'loading'}. The
      // login form was one redirect away and nothing performed it.
      //
      // A full-page assign rather than the router: the login form is served by
      // elitea-main and is outside this SPA's route tree.
      //
      // The callback path is exempt. It is where the OIDC popup lands to hand
      // its result back, and it runs while the opener is still unauthenticated
      // by definition — redirecting it would cancel the flight it completes.
      const { user, probeStatus } = useSessionStore.getState();
      if (user !== undefined) return;
      if (window.location.pathname.endsWith(AUTH_CALLBACK_PATH)) return;

      /*
       * A probe that never ANSWERED is not a probe that said "no session".
       *
       * `probeStatus` is `undefined` only for the two `HttpFailure` kinds that
       * carry no status: `network` and `aborted` (`shared/api/http.ts:48-51`).
       * Every real answer has one — 200, 401 and the Form plane's 404 alike.
       *
       * DEFECT this repairs (J20c, webkit). The browser cancels every in-flight
       * request the instant it starts leaving a page. `/forward-auth/info` is
       * issued from this effect, so a reload or a link click during boot aborts
       * it. `fetchSession` then stored `user: undefined`, this continuation ran
       * inside the UNLOADING document, and the assign below fought the
       * navigation already under way. Measured on the E2E stack: `page.reload()`
       * failed with "Frame load interrupted" and the browser landed on the OIDC
       * authorize page instead of the page it was loading. A signed-in user who
       * reloads early was thrown into a re-login.
       *
       * Not redirecting is also the right answer away from that race: when the
       * probe cannot reach elitea-main, the login form it would send the browser
       * to is served by that same unreachable elitea-main.
       */
      if (probeStatus === undefined) return;

      const returnTo = window.location.pathname + window.location.search;
      window.location.assign(buildLoginUrl(authPlaneFromProbeStatus(probeStatus), returnTo));
    });
  }, [fetchSession, router]);

  if (config.status === 'missing') {
    return <MissingEnvPage missing={config.missing} />;
  }

  // Wire the generated API client with the runtime server URL (R2 gap).
  // Called on every render, but idempotent — configureGeneratedClient replaces
  // the singleton in-place, which is fine since the URL never changes at runtime.
  //
  // `reauthenticate` is what makes §5.4 behaviour 2/3 live: without it
  // `http.ts`'s `runReauth()` returns false before doing anything, so
  // `needsReauth()` was dead for every 401/403 in the app and
  // `createAuthPopupController` had no production call site at all (issue
  // #136 B). Passing the CONTROLLER's method (not a fresh flight per client)
  // keeps single-flight intact across the re-configurations this render-time
  // call performs.
  configureGeneratedClient({
    baseUrl: config.config.vite_server_url,
    // Called through the controller rather than passed as a bare method
    // reference: the flight slot is closure state on that one object, so it
    // must stay the receiver (and `typescript(unbound-method)` says so too).
    reauthenticate: () => authPopup.reauthenticate(),
  });

  return (
    <AppProviders>
      <RouterProvider router={router} context={{ auth: sessionAuthContext }} />
    </AppProviders>
  );
}
