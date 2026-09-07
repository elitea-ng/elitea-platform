/**
 * settingsUsersTab.test.tsx
 *
 * The COMPOSITION ROOT for the Settings drawer's personal-project gate.
 * `settingsSections.test.ts` proves the pure filter, and
 * `-guards/personalProject.test.tsx` proves the predicate; neither one can see
 * that `settings-layout.tsx` feeds the filter the WRONG fact, which is the way
 * this broke. The layout used a bare `selectedProjectId === personal_project_id`
 * comparison, and for every member of a single shared project — the shape both
 * E2E personas have, and the shape any account whose `project_user_<uid>` row
 * was never provisioned has — that says "personal". The Users tab vanished and
 * `/settings/users` redirected to General.
 *
 * So this test drives the real generated route tree with the real store and the
 * real router, and asserts on WHERE THE ROUTER ENDS UP.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { routeTree } from '../../routeTree.gen';
import { server } from '../../test/setup';

const BASE = 'http://elitea.test/api/v2';
/** Neither of the two projects below, so nothing here is the public project. */
const PUBLIC_PROJECT_ID = '11';

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * The list the server really answers on the E2E stack: one ordinary shared
 * project, which is also what `GET /social/author` names as this caller's
 * `personal_project_id` (`resolvePersonalProjectID`'s third branch), plus one
 * genuine personal project belonging to somebody else's account.
 */
const PROJECTS = [
  { id: 1, name: 'Default Project', owner_id: 1, suspended: false, create_success: true },
  { id: 7, name: 'project_user_7', owner_id: 7, suspended: false, create_success: true },
];

function authFor(personalProjectId: string): AuthContext {
  return {
    getUser: () => ({ id: '1', personal_project_id: personalProjectId }),
    getSelectedProjectId: () => undefined,
  };
}

function mountUsersTab(auth: AuthContext) {
  const history = createMemoryHistory({ initialEntries: ['/settings/users'] });
  const router = createRouter({ routeTree, history, context: { auth } satisfies RouterContext });
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

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', PUBLIC_PROJECT_ID);
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/projects/project/default/${PUBLIC_PROJECT_ID}`, () =>
      HttpResponse.json(PROJECTS),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  vi.unstubAllEnvs();
  useSelectedProjectStore.setState({ project: null });
  queryClient.clear();
});

describe('Settings › the Users tab and the personal-project gate', () => {
  it('keeps Users reachable in a SHARED project the server named as personal', async () => {
    useSelectedProjectStore.setState({ project: { id: '1', name: 'Default Project' } });
    const router = mountUsersTab(authFor('1'));

    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Users' })).toBeInTheDocument();
    });
    // The redirect is what actually broke the journeys: the dialog opened and
    // was then torn out of the DOM. Give the effect every chance to fire
    // before declaring the route stable.
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/users');
    });
    expect(router.state.location.pathname).toBe('/settings/users');
    // …and the Users ROUTE is what is mounted, not the General page rendered
    // under a URL the redirect had not rewritten yet.
    expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/users');
  });

  /*
   * The other half of the gate, which the fix must NOT have removed. The
   * redirect is the assertion, not the absent drawer row: an absent row is
   * also what the first frame shows, before the project list has arrived, so
   * `queryByRole(...)` alone would pass against a gate that never fires.
   */
  it('still hides Users, and redirects away, in a real personal project', async () => {
    useSelectedProjectStore.setState({ project: { id: '7', name: 'Private' } });
    const router = mountUsersTab(authFor('7'));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/project-general');
    });
    expect(screen.queryByRole('button', { name: 'Users' })).not.toBeInTheDocument();
  });
});
