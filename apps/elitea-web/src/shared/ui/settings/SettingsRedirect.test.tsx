/**
 * SettingsRedirect.test.tsx — regression coverage for the confirmed bug:
 * `shouldRedirect` used to be based on `useParams({ strict: false }).tab`,
 * which only `routes/_shell/settings/$tab.tsx` (the catch-all) ever
 * populates. Every explicit static settings leaf route (`notifications.tsx`,
 * `project-params.tsx`, `model-configuration.tsx`, `analytics.tsx`, ...)
 * never declares a `tab` param, so `tab` was always `undefined` for them and
 * the component fired a `replace: true` navigation back to
 * the default settings tab on every fresh mount — direct URL entry,
 * refresh, bookmark, back/forward, or any deep link straight into a
 * non-default tab.
 *
 * Exercises the REAL, generated route tree (`src/routeTree.gen.ts`) through
 * a real `RouterProvider` with in-memory history — no hand-built minimal
 * route tree, no mocking of the router itself — mirroring
 * `src/routes/__tests__/settingsLayout.test.tsx`'s established pattern.
 *
 * Assertions read `router.state` (matched leaf route id / final pathname)
 * rather than rendered DOM content: the settings child pages this mounts
 * (`notifications.tsx`, `analytics.tsx`, ...) do their own data fetching
 * that is out of scope here, and each has its own `errorComponent` boundary
 * isolating any of that from `SettingsRedirect` (a sibling of `<Outlet/>`,
 * one level up in `settings-layout.tsx`) — so router-state assertions are
 * both sufficient and immune to those pages' fetch behaviour.
 * (`secrets.tsx` is deliberately NOT used as a fixture here: its
 * `SecretsContent` gets stuck in a non-terminating render loop when mounted
 * against an unconfigured generated API client / no project store — a
 * pre-existing issue in that component, outside this file's scope to fix,
 * so `project-params.tsx` stands in as the second static-leaf fixture
 * instead.)
 *
 * The initial match is awaited, then microtasks/effects are flushed and the
 * match is re-checked a second time, before asserting "no redirect
 * happened" — a single-pass `waitFor` can resolve on the pre-redirect paint
 * and miss the effect-driven `navigate()` this component fires, exactly the
 * pitfall that hid this bug from `settingsLayout.test.tsx`'s existing
 * per-tab assertions. (`router.state.status` itself is NOT used as that
 * signal — see the second describe block below for why, and for the
 * `isLoading`-based race-condition fix this file also covers.
 * `router.state.matches`/`location` are the authoritative, always-fresh
 * signal instead.)
 *
 * The second describe block below ("in-flight navigation race") covers a
 * DIFFERENT, later-discovered bug in the same component: `shouldRedirect`
 * used to treat "the deepest currently-active match is the settings-layout
 * route" as conclusive proof that no child would ever match, but that same
 * condition is also transiently true while a navigation to a real child
 * route hasn't been promoted onto `state.matches` yet. See
 * `SettingsRedirect.tsx`'s `computeShouldRedirect` doc comment for the full
 * empirical writeup (verified against the installed
 * `@tanstack/router-core@1.171.15` source plus an instrumented throwaway
 * test) of which `RouterState` fields actually distinguish "settling" from
 * "settled".
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRouter, RouterProvider, useRouterState } from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { act, render, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { routeTree } from '../../../routeTree.gen';
import { stubAuthContext } from '@/app/router-context';
import {
  DEFAULT_SETTINGS_TAB,
  computeRedirectTarget,
  computeShouldRedirect,
} from './SettingsRedirect';

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, throwOnError: true } },
});

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

afterEach(() => {
  queryClient.clear();
});

function mountAt(path: string) {
  const history = createMemoryHistory({ initialEntries: [path] });
  const router = createRouter({ routeTree, history, context: { auth: stubAuthContext } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <CssBaseline />
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return router;
}

/**
 * Waits for the router's leaf match to settle at `expectedPathname`, flushes
 * a macrotask (long enough for `SettingsRedirect`'s `useEffect` to have run
 * and any `navigate()` it fired to have committed), then asserts the leaf
 * match is STILL `expectedPathname` — catching a redirect that fires only
 * after the first successful paint (see file header for why this two-step
 * shape is required and why it isn't built on `router.state.status`).
 */
async function expectSettledAt(router: ReturnType<typeof mountAt>, expectedPathname: string) {
  await waitFor(() => expect(router.state.location.pathname).toBe(expectedPathname));

  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 50));
  });

  expect(router.state.location.pathname).toBe(expectedPathname);
}

describe('SettingsRedirect — static leaf routes (mount shape: explicit file, no $tab param)', () => {
  it('does NOT redirect away from /settings/notifications on a fresh mount (the exact regression: tokenHref()-style deep links into a non-default tab)', async () => {
    const router = mountAt('/settings/notifications');

    await expectSettledAt(router, '/settings/notifications');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/notifications');
  });

  it('does NOT redirect away from /settings/project-params on a fresh mount', async () => {
    const router = mountAt('/settings/project-params');

    await expectSettledAt(router, '/settings/project-params');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-params');
  });

  it('does NOT redirect away from /settings/analytics on a fresh mount', async () => {
    const router = mountAt('/settings/analytics');

    await expectSettledAt(router, '/settings/analytics');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/analytics');
  });
});

describe('SettingsRedirect — settings index (mount shape: no settings child matched yet)', () => {
  /*
   * `project-general`, NOT `model-configuration`. The reference's
   * `DEFAULT_TAB` is `project-general` (`pages/settings/index.jsx:130`) and a
   * live deployment lands there: clicking Settings in the nav rail opens
   * `/app/settings/project-general`. This app used to send every such visit
   * to AI Providers, so the General tab was unreachable by default even
   * after it existed.
   */
  it('redirects /settings alone to the default tab, project-general', async () => {
    const router = mountAt('/settings');

    await expectSettledAt(router, '/settings/project-general');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-general');
  });
});

describe('SettingsRedirect — $tab.tsx catch-all (mount shape: only route that ever populates `tab`)', () => {
  it('redirects a legacy tab slug (configuration) to the default tab', async () => {
    const router = mountAt('/settings/configuration');

    await expectSettledAt(router, '/settings/project-general');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-general');
  });

  it('redirects the other legacy tab slug (information) to the default tab', async () => {
    const router = mountAt('/settings/information');

    await expectSettledAt(router, '/settings/project-general');
  });

  /*
   * PRODUCTION'S OWN SLUGS RESOLVE.
   *
   * The reference routes AI Providers at `ai-providers` and Project Context
   * at `project-context`; this app's route files are `model-configuration`
   * and `project-params`. Before the alias table, a URL copied out of
   * production fell through to the unknown-tab branch: `project-context`
   * landed the reader on AI Providers, silently, with no sign anything had
   * gone wrong. `ai-providers` happened to reach the right screen only
   * because AI Providers WAS the unknown-tab fallback — an accident that
   * changing the default tab would have broken.
   */
  it("resolves production's `ai-providers` slug to this app's model-configuration route", async () => {
    const router = mountAt('/settings/ai-providers');

    await expectSettledAt(router, '/settings/model-configuration');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/model-configuration');
  });

  it("resolves production's `project-context` slug to this app's project-params route", async () => {
    const router = mountAt('/settings/project-context');

    await expectSettledAt(router, '/settings/project-params');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-params');
  });

  it('redirects a genuinely unknown tab slug to the default tab (D4 ROUTE-076)', async () => {
    const router = mountAt('/settings/this-tab-does-not-exist');

    await expectSettledAt(router, '/settings/project-general');

    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-general');
  });
});

describe('SettingsRedirect — in-flight navigation race (matches not yet promoted for a valid child)', () => {
  it('does NOT redirect for a mid-transition snapshot: isLoading=true with matches truncated to the settings-layout route — the exact race a slow network/code-split chunk can produce', async () => {
    // Settle for real first, purely to obtain GENUINE match objects from the
    // real router (not hand-built fixtures) for a route that DOES have a
    // deeper, valid child.
    const router = mountAt('/settings/notifications');
    await expectSettledAt(router, '/settings/notifications');

    const settledState = router.state;
    expect(settledState.matches.at(-1)?.routeId).toBe('/_shell/settings/notifications');

    // Reconstruct exactly the transient shape described in the race: the
    // deepest ACTIVE match still ends at the settings-layout route (child
    // not promoted yet) while a navigation toward the real child is in
    // flight. Built from the router's own real match objects, truncated —
    // not fabricated — to reproduce the reported array shape.
    const midTransitionState = {
      ...settledState,
      isLoading: true,
      matches: settledState.matches.slice(0, -1),
    };
    expect(midTransitionState.matches.at(-1)?.routeId).toBe('/_shell/settings');

    expect(computeShouldRedirect(midTransitionState)).toBe(false);

    // Sanity check that `isLoading` is actually the thing doing the work:
    // the SAME truncated-matches snapshot, evaluated as if settled, DOES
    // read as "redirect" — proving the guard, not some other difference
    // between the two states, is what suppresses the premature redirect.
    expect(computeShouldRedirect({ ...midTransitionState, isLoading: false })).toBe(true);
  });

  it('does NOT redirect for a mid-transition snapshot with matches empty entirely (the pre-first-promotion window)', async () => {
    const router = mountAt('/settings/notifications');
    await expectSettledAt(router, '/settings/notifications');

    const midTransitionState = {
      ...router.state,
      isLoading: true,
      matches: [],
    };

    expect(computeShouldRedirect(midTransitionState)).toBe(false);
    // Confirms the empty-matches branch is genuinely gated by `isLoading`
    // too, not just the SETTINGS_ROUTE_ID branch.
    expect(computeShouldRedirect({ ...midTransitionState, isLoading: false })).toBe(true);
  });

  it('captures shouldRedirect on every real render for a fresh /settings/notifications mount and confirms it is never true before the router settles', async () => {
    const history = createMemoryHistory({ initialEntries: ['/settings/notifications'] });
    const router = createRouter({ routeTree, history, context: { auth: stubAuthContext } });

    // Records `computeShouldRedirect`'s value, and whether a settings-layout
    // match exists yet, on every render this hook instance produces — i.e.
    // every real `state.isLoading`/`state.matches` transition the router
    // actually goes through — using the SAME public, typed `useRouterState`
    // API `SettingsRedirect` itself uses (no reach into private store
    // internals).
    const observed: Array<{ shouldRedirect: boolean; hasSettingsLayoutMatch: boolean }> = [];
    function ShouldRedirectProbe() {
      const snapshot = useRouterState({
        router,
        select: (state) => ({
          shouldRedirect: computeShouldRedirect(state),
          hasSettingsLayoutMatch: state.matches.length > 0,
        }),
      });
      observed.push(snapshot);
      return null;
    }

    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <CssBaseline />
          <ShouldRedirectProbe />
          <RouterProvider router={router} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/notifications'));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 50));
    });

    // The real router settled without ever redirecting away.
    expect(router.state.location.pathname).toBe('/settings/notifications');
    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/notifications');

    // At least one render was actually observed (otherwise this assertion
    // would be vacuous).
    expect(observed.length).toBeGreaterThan(0);

    // The very first render this standalone probe produces happens
    // synchronously, before `RouterProvider`'s own mount effect has even
    // called `router.load()` — i.e. before `state.matches` has EVER been
    // populated (`isLoading: false, matches: []` is the router's literal
    // pre-load initial state). `computeShouldRedirect` reads that as "no
    // child will ever match" (its pre-existing, intentional fallback — see
    // its own doc comment) and reports `true`. That tick is real, but it is
    // NOT one `SettingsRedirect` itself can ever observe: the real
    // component only exists nested inside `settings-layout.tsx`'s rendered
    // output, which TanStack Router does not create until `state.matches`
    // contains at least the settings-layout match — confirmed by this same
    // `observed` log always going straight from "no settings-layout match"
    // to "settings-layout match present AND already the full resolved
    // chain" (see the other tests in this block), never anything
    // in-between. So the one meaningful invariant to assert is: from the
    // first render where a settings-layout match exists onward — i.e. every
    // render `SettingsRedirect` could actually have produced —
    // `shouldRedirect` is never `true` until the router is done settling.
    const fromFirstPossibleMount = (() => {
      const firstMountedIndex = observed.findIndex((snapshot) => snapshot.hasSettingsLayoutMatch);
      expect(firstMountedIndex).toBeGreaterThanOrEqual(0);
      return observed.slice(firstMountedIndex);
    })();

    for (const snapshot of fromFirstPossibleMount) {
      expect(snapshot.shouldRedirect).toBe(false);
    }
  });
});

/**
 * `computeRedirectTarget` is the pure half — the router-free view of the same
 * decision the mounted tests above exercise. It is worth its own cases
 * because the alias table lives in it, and an alias that resolves to the
 * WRONG screen is invisible in a mounted test that only checks "did we
 * redirect".
 */
describe('computeRedirectTarget (pure)', () => {
  /* A hand-built RouterState slice. `computeRedirectTarget` reads exactly two
   * fields, `isLoading` and `matches`; building the real 20-parameter generic
   * would say nothing more about the behaviour under test. */
  type StateSlice = Parameters<typeof computeRedirectTarget>[0];
  const settled = (routeId: string, params: Record<string, string> = {}): StateSlice =>
    ({ isLoading: false, matches: [{ routeId, params }] }) as unknown as StateSlice;

  it('says nothing while a navigation is still in flight', () => {
    expect(
      computeRedirectTarget({ isLoading: true, matches: [] } as unknown as StateSlice),
    ).toBeUndefined();
  });

  it('leaves an explicitly-declared settings child alone', () => {
    expect(computeRedirectTarget(settled('/_shell/settings/secrets'))).toBeUndefined();
    expect(computeRedirectTarget(settled('/_shell/settings/project-general'))).toBeUndefined();
  });

  it('sends the layout route itself, and a missing tab param, to the default tab', () => {
    expect(computeRedirectTarget(settled('/_shell/settings'))).toBe(DEFAULT_SETTINGS_TAB);
    expect(computeRedirectTarget(settled('/_shell/settings/$tab'))).toBe(DEFAULT_SETTINGS_TAB);
  });

  it("maps production's slugs onto this app's route names", () => {
    // The whole point of the table: a URL copied out of next.elitea.ai has to
    // reach the screen it named, not the unknown-tab fallback.
    expect(computeRedirectTarget(settled('/_shell/settings/$tab', { tab: 'ai-providers' }))).toBe(
      'model-configuration',
    );
    expect(computeRedirectTarget(settled('/_shell/settings/$tab', { tab: 'project-context' }))).toBe(
      'project-params',
    );
  });

  it('sends legacy and unknown slugs to the default tab', () => {
    for (const tab of ['configuration', 'information', 'this-tab-does-not-exist']) {
      expect(computeRedirectTarget(settled('/_shell/settings/$tab', { tab }))).toBe(
        DEFAULT_SETTINGS_TAB,
      );
    }
  });

  it('agrees with the boolean form it backs', () => {
    expect(computeShouldRedirect(settled('/_shell/settings/secrets'))).toBe(false);
    expect(computeShouldRedirect(settled('/_shell/settings/$tab', { tab: 'ai-providers' }))).toBe(true);
  });
});
