/**
 * Settings › Profile's "Log out" button (issue #136 A).
 *
 * The control moved: it used to be a NAV ITEM in the drawer's PERSONAL group,
 * which is not what production shows — the baseline
 * (`[fsd]/features/settings/ui/profile/Profile.jsx`) puts it on the Profile
 * page, under the identity rows. The behaviour asserted below is unchanged;
 * only where the click lands moved.
 *
 * `SettingsLayout.handleItemClick` used to treat it as a tab like every other
 * item and do nothing but `window.history.replaceState()` to
 * `/settings/logout` — a path with no route behind it, which
 * `SettingsRedirect` then bounced straight back to
 * `/settings/model-configuration`. Nothing was swept and the browser never
 * left the SPA, while `shared/api/auth/logout.ts`'s `performLogout()` — the
 * function that does both — had no call site anywhere in `src/`, only its own
 * unit test.
 *
 * Asserted here: the half that is observable in jsdom — the `el.` namespace
 * sweep across BOTH storage areas, plus the fact that keys OUTSIDE the
 * namespace survive (a blanket `clear()` would satisfy a sweep-only
 * assertion, and MUI's colour-scheme keys living outside `el.` is a
 * deliberate decision, not an oversight — see this file's second test).
 * The `/forward-auth/logout` handoff assigns `window.location.href`, which
 * jsdom cannot perform ("Not implemented: navigation to another Document");
 * that half is covered end to end by
 * `e2e/journeys/shell/shell.redirect.spec.ts`'s J4, which asserts the browser
 * genuinely leaves the app.
 *
 * Its own file, with its own `QueryClient`, rather than an addition to
 * `settingsLayout.test.tsx`: that suite's shared client sets
 * `throwOnError: true`, so `AppShell`'s unconfigured-client error propagates
 * to the route error boundary and detaches the settings subtree mid-test —
 * the drawer item is found, then the click lands on a node React no longer
 * owns and the handler never runs. Measured: the identical test passes with a
 * non-throwing client and fails with that one, for that reason and not
 * because of the code under test.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';

import { stubAuthContext } from '@/app/router-context';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { routeTree } from '../../routeTree.gen';
import { installWebStorageShim } from '../../test/webstorage';

installWebStorageShim();

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function mountSettings(path: string): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
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
}

/** Keys still present under the `el.` namespace in one area. */
function survivingElKeys(store: Storage): string[] {
  return Array.from({ length: store.length }, (_, index) => store.key(index)).filter(
    (key): key is string => key !== null && key.startsWith('el.'),
  );
}

async function clickLogOut(): Promise<void> {
  const logout = await screen.findByText('Log out', undefined, { timeout: 10_000 });
  await userEvent.click(logout);
}

afterEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
});

describe('settings profile — Log out', () => {
  it('sweeps the whole el. namespace in BOTH storage areas', async () => {
    mountSettings('/settings/profile');

    // A key the app really writes (`selectedProjectPersistence.ts`) and one
    // the auth popup really writes (`auth/constants.ts`), in both areas.
    window.localStorage.setItem('el.project.id', '1');
    window.localStorage.setItem('el.project.name', 'Default Project');
    window.sessionStorage.setItem('el.auth.state', 'abc');
    window.sessionStorage.setItem('el.project.id', '1');

    await clickLogOut();

    expect(survivingElKeys(window.localStorage)).toEqual([]);
    expect(survivingElKeys(window.sessionStorage)).toEqual([]);
  });

  it('leaves keys outside the el. namespace alone — it is a sweep, not a clear()', async () => {
    mountSettings('/settings/profile');

    window.localStorage.setItem('el.project.id', '1');
    // MUI's colour-scheme keys sit OUTSIDE the `el.` namespace
    // (`mui-color-scheme-*`, and `el-mode` — hyphen, not dot — from
    // `INIT_COLOR_SCHEME_PROPS.modeStorageKey`), so the theme a user picked
    // survives signing out. That is intentional: a display preference is
    // device state, not session state, and wiping it would silently reset
    // every returning user to the default dark scheme. Pinned here so the
    // decision is a test rather than a comment.
    window.localStorage.setItem('mui-color-scheme-dark', 'dark');
    window.localStorage.setItem('el-mode', 'light');
    window.sessionStorage.setItem('third-party-key', 'keep-me');

    await clickLogOut();

    expect(survivingElKeys(window.localStorage)).toEqual([]);
    expect(window.localStorage.getItem('mui-color-scheme-dark')).toBe('dark');
    expect(window.localStorage.getItem('el-mode')).toBe('light');
    expect(window.sessionStorage.getItem('third-party-key')).toBe('keep-me');
  });
});
