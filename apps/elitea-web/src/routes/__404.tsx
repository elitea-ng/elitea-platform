import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { StatusPage } from '@/shared/ui/status-page';

/**
 * ROUTE-071 (spec §8.1) — the "Page404" component, ported from
 * apps/elitea-ui/src/pages/Page404.jsx.
 *
 * DELIBERATELY NOT a TanStack file-route (no `createFileRoute`/`Route`
 * export), even though this module lives under `src/routes/`. This is a
 * verified, not guessed, judgment call — see the two facts below, both
 * checked against the exact installed packages (@tanstack/router-generator
 * 1.167.21, @tanstack/react-router 1.170.18; context7 was unavailable this
 * session — quota exhausted — so verification ran directly against the
 * installed source + a scratch `Generator.run()`):
 *
 * 1. TanStack Router's file-based generator treats ANY route segment whose
 *    name starts with `_` as a pathless-layout marker (the same convention
 *    that makes `__root.tsx` the root route). `__404.tsx` therefore does not
 *    map to a `/404`-shaped path — it collapses to an EMPTY path directly
 *    under root, i.e. `fullPath: '/'`. Once the real app also has an actual
 *    `/` (index/redirect) route — spec §9.3's E2E flow 1 says it does
 *    ("Cold load `/` → redirect chain → `/chat`") — the generator's
 *    `checkRouteFullPathUniqueness` throws a fatal
 *    "Conflicting configuration paths… '/'.'" build error. Reproduced with:
 *      routes/__root.tsx, routes/index.tsx (path '/'),
 *      routes/__404.tsx (createFileRoute('/__404')({...}))
 *    → `Generator.run()` throws exactly that error, naming both files.
 * 2. If this file instead exports NO `Route` (as it does now), the very same
 *    generator silently DROPS it from the route tree (a `pathless_layout`
 *    node with no children is skipped — verified: the generated
 *    `routeTree.gen.ts` contains no import of, or reference to, this file at
 *    all) and logs one benign warning: 'Route file ".../__404.tsx" does not
 *    export a Route. This file will not be included in the route tree.' Exit
 *    code 0, no collision, no thrown error.
 *
 * That second behaviour is exactly the parity shape §0 P11 and ROUTE-071
 * describe for the OLD app: `Page404.jsx` is a plain component, never
 * independently reachable through routing — the outer
 * `<Route path="*" element={<Page404/>}/>` in router.jsx never renders
 * (AppLayout has no `<Outlet/>`, P11), and the INNER
 * `<Route path="*" element={<Page404/>}/>` in ProtectedRoutes.jsx:389-392
 * (ROUTE-071) is ALSO dead code: react-router-dom's route-ranking algorithm
 * scores `/:projectId/*` (a dynamic segment + splat) higher than a bare `*`
 * for every non-empty pathname, so ROUTE-070 always wins the sibling
 * comparison and ROUTE-071 never gets picked (independently confirmed here
 * by hand-computing computeScore: `/:projectId/*` → 5, bare `*` → −1, for
 * ANY matching pathname — not path-dependent). `Page404` is reachable ONLY
 * because `ProjectSwitcher.jsx:86-88` imports and renders it directly when
 * the resolved project isn't found. This module reproduces that exactly:
 * `$projectId.$.tsx` imports `NotFoundPage` from here and renders it inline
 * — never through independent route matching, on either app.
 *
 * If TanStack Router ever grows a first-class way to attach a genuinely
 * unreachable id to a file (or R1 adds a `routeFileIgnorePattern` in
 * `src/app/router*` scoped to this exact file), the warning above can be
 * silenced without changing behaviour — it is cosmetic, not a build risk.
 *
 * Re-confirmed against the REAL pipeline once R1 landed `src/routes/
 * __root.tsx` + the router-plugin wiring mid-way through this unit:
 * `grep -c '404' src/routeTree.gen.ts` → 0. Not just the scratch probe above
 * — the actual generated tree this app ships.
 *
 * IMPORTANT for whoever adds more `_`-leading files under `src/routes/`
 * later: this exact fullPath-'/' collision hazard applies to ANY file whose
 * name segment starts with `_` and stays flat at the routes root (not
 * nested under a real directory) — `__404.tsx` is not a one-off footgun,
 * it's a pattern.
 *
 * The "Home page" link is a plain `<a href="/">`, not TanStack's `<Link>`,
 * on purpose: `<Link>` calls `useRouter()` and throws when rendered without
 * a mounted `RouterProvider` ancestor (verified — a real `TypeError` in
 * `useLinkProps`), which this module and `ProjectSwitcherView` must not
 * require just to be unit-testable. A full navigation to `/` is also exactly
 * as correct here as a client-side one — old `Page404.jsx`'s `RouterLink`
 * was a convenience, not an observable-behaviour parity requirement.
 */
export function NotFoundPage() {
  const canGoBack = window.history.length > 1;
  return (
    <StatusPage
      testId="not-found-page"
      code="404"
      title={t('route.notFound.title', 'Page not found')}
      minHeight="70vh"
      actions={
        <>
          <Button variant="contained" component="a" href="/">
            {t('route.notFound.homeButton', 'Go to the home page')}
          </Button>
          {canGoBack ? (
            <Button
              variant="outlined"
              onClick={() => {
                window.history.back();
              }}
            >
              {t('route.notFound.back', 'Go back')}
            </Button>
          ) : null}
        </>
      }
    >
      <Typography
        component="p"
        variant="bodyMedium"
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, lineHeight: 1.6, margin: 0 })}
      >
        {t(
          'route.notFound.message',
          'The page you are looking for does not exist or may have been moved. Check the address, or head back to the home page.',
        )}
      </Typography>
    </StatusPage>
  );
}
