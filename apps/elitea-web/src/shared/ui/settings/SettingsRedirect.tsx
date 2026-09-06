import { useEffect } from 'react';

import { useNavigate, useRouterState } from '@tanstack/react-router';
import type { RegisteredRouter, RouterState } from '@tanstack/react-router';

/** Tab identifiers that have explicit files in `routes/_shell/settings/`. */
const VALID_TABS = [
  'model-configuration',
  'prompts',
  'environment',
  'project-params',
  'secrets',
  'users',
  'analytics',
  'usage',
  'personalization',
  'profile',
  'preferences',
  'ai-personality',
  'memory',
  'tokens',
  'notifications',
] as const;

/** Old tabs that mapped to `model-configuration` in the original app. */
const LEGACY_TABS = ['configuration', 'information'] as const;

/**
 * Route id of the `/settings/:tab` catch-all (`$tab.tsx`) — the ONLY
 * settings child route that declares a `tab` path param. Every other
 * settings child (`notifications.tsx`, `secrets.tsx`, `model-configuration.tsx`,
 * `create-configuration.tsx`, ...) is its own explicitly-declared route with
 * no `tab` param, so a `useParams({ strict: false })` read of `tab` is
 * `undefined` for ALL of them — not just for genuinely-unknown tabs. That
 * made `shouldRedirect` true on every fresh mount of a static settings leaf
 * (direct URL entry, refresh, back/forward, deep link), bouncing the user to
 * `/settings/model-configuration`. See this component's test file for the
 * reproduction against the real router.
 */
const TAB_CATCH_ALL_ROUTE_ID = '/_shell/settings/$tab';

/**
 * Route id of the settings layout route itself. The deepest match equals
 * this only when no settings child has matched yet at all (e.g. a
 * transient/degenerate state) — preserved as a defensive fallback for the
 * original "missing tab param" case.
 */
const SETTINGS_ROUTE_ID = '/_shell/settings';

/**
 * Decides whether {@link SettingsRedirect} should bounce the user to
 * `/settings/model-configuration`, given a `RouterState` snapshot.
 *
 * `leaf.routeId === SETTINGS_ROUTE_ID` is ambiguous on its own: it is true
 * both when NO child route will ever match (the genuine "missing tab param"
 * fallback case) AND, transiently, while a navigation to a real child route
 * (e.g. `/settings/notifications`) is still in flight and hasn't been
 * promoted onto `state.matches` yet. Treating those identically fires a
 * premature `navigate()` under timing pressure (slow network, a code-split
 * chunk still loading) for a URL that was always going to resolve to a
 * valid static leaf.
 *
 * `state.isLoading` is the field that disambiguates the two, verified
 * against the installed `@tanstack/router-core@1.171.15` (read
 * `node_modules/@tanstack/router-core/dist/esm/router.js` directly, plus an
 * instrumented throwaway test that subscribed to the raw router store and
 * logged every notification across a real mount):
 *
 * - `isLoading` is set `true` synchronously in `beforeLoad()` at the START
 *   of every navigation (including the very first load of the app), and set
 *   `false` inside the SAME `batch()` call that promotes the
 *   fully-resolved `pendingMatches` onto `state.matches` (see the
 *   `onReady` handler in `router.js`). Because both writes are batched
 *   together, any render that observes `isLoading === false` is guaranteed
 *   to also observe the final, post-navigation `matches` array — there is
 *   no tick where one has updated and the other hasn't.
 * - The instrumented test confirmed this empirically for a fresh mount at
 *   `/settings/notifications`: `state.matches` jumped directly from `[]` to
 *   the full resolved chain
 *   (`__root__,/_shell,/_shell/settings,/_shell/settings/notifications`) in
 *   the exact same store notification where `isLoading` flipped from
 *   `true` to `false` — no intermediate frame ever showed a partial chain
 *   ending at the settings-layout route.
 * - `state.status` ('pending' | 'idle') is NOT used for this even though it
 *   sounds like the obvious "is a navigation in flight" signal: contrary to
 *   an earlier (incorrect) assumption that it never resets, it DOES flip
 *   back to `'idle'` after settling — but in a render LATER than
 *   `isLoading` does (it's driven by a separate `Transitioner` effect that
 *   fires only after `isLoading`/`hasPending`/the router's own transition
 *   flag have ALL cleared). Using it would only widen the unsafe window,
 *   not close it.
 * - `state.isTransitioning` is NOT used because it is dead in this version:
 *   `router-core`'s own store field is initialised to `false` and is never
 *   written anywhere else in `router.js`. (There's a same-named but
 *   unrelated local `useState` inside `@tanstack/react-router`'s internal
 *   `Transitioner` component that toggles around `React.startTransition`,
 *   but that value isn't exposed on `RouterState`.)
 *
 * Net effect: the match-based logic below only runs once `!state.isLoading`
 * — i.e. once the router has genuinely settled with no navigation in
 * flight — so a still-resolving navigation toward a valid static leaf can
 * never be mistaken for "no child will ever match".
 *
 * Exported (not just used inline) so the race-condition guard can be
 * exercised directly against synthetic/real `RouterState` snapshots — see
 * `SettingsRedirect.test.tsx`.
 */
export function computeShouldRedirect(state: RouterState<RegisteredRouter['routeTree']>): boolean {
  if (state.isLoading) return false;

  const leaf = state.matches.at(-1);
  if (!leaf || leaf.routeId === SETTINGS_ROUTE_ID) return true;
  if (leaf.routeId !== TAB_CATCH_ALL_ROUTE_ID) return false;

  const { tab } = leaf.params;
  if (!tab) return true;
  if (LEGACY_TABS.includes(tab as (typeof LEGACY_TABS)[number])) return true;
  if (!VALID_TABS.includes(tab as (typeof VALID_TABS)[number])) return true;
  return false;
}

/**
 * Handles backwards-compatible navigation for settings routes.
 *
 * - No settings child route matched at all → redirect to default.
 * - The `$tab.tsx` catch-all matched (i.e. no explicit static route exists
 *   for the requested slug) with a legacy tab identifier (`configuration`,
 *   `information`) → redirect to `model-configuration`.
 * - The `$tab.tsx` catch-all matched with any other unrecognised tab (e.g.
 *   `integrations`, which has no explicit file) → redirect to
 *   `model-configuration`.
 * - Any explicitly-declared settings child route matched (whether or not its
 *   slug is in `VALID_TABS`) → no redirect; the router already resolved the
 *   URL correctly.
 * - A navigation is still in flight → no redirect (yet); see
 *   {@link computeShouldRedirect}'s doc comment for why and for the
 *   empirical findings about which `RouterState` fields actually
 *   distinguish "settling" from "settled" in the installed router version.
 *
 * Determines "what's actually active" from the router's own match state
 * (`useRouterState`) rather than a route param — `$tab` is populated only
 * when the catch-all route is the one that matched, which is exactly the
 * one case this component still needs to validate.
 *
 * Renders `null` — side-effect only.
 *
 * Ported from `apps/elitea-ui/src/[fsd]/features/settings/ui/settings-drawer/SettingsRedirect.jsx`.
 */
export function SettingsRedirect() {
  const navigate = useNavigate();

  const shouldRedirect = useRouterState({ select: computeShouldRedirect });

  useEffect(() => {
    if (shouldRedirect) {
      // oxlint-disable-next-line typescript/no-floating-promises -- navigate() returns Promise<void>; we fire-and-forget.
      void navigate({ to: '/settings/model-configuration', replace: true });
    }
  }, [navigate, shouldRedirect]);

  return null;
}
