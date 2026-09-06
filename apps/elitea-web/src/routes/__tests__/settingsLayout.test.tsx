import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { routeTree } from '../../routeTree.gen';
import { stubAuthContext } from '@/app/router-context';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

/**
 * Shared QueryClient so tests don't re-create it each run.
 */
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false, throwOnError: true } },
});

/** Shared Elitea theme — provides palette.border, palette.background, etc. for sx functions. */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

// The shell this layout mounts under renders widgets that fetch through the
// generated client (the support assistant, for one). Unconfigured, that throws
// inside the shell; @tanstack/react-router 1.170.32 (#680) lets that throw
// reach the root boundary during the SettingsRedirect navigation, so the
// redirected page never rendered its tabs. Configure it, as the sibling route
// tests do; msw answers what the shell asks for.
beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'http://elitea.test/api/v2' });
});
afterEach(() => {
  resetGeneratedClient();
  queryClient.clear();
});

/**
 * ROUTE-051..066 nested layout (spec §9.3 R1 RED/GREEN (e)) + D4's
 * ROUTE-076 anomaly ("`/settings/:tab` with an unknown tab renders the
 * Settings layout with an empty outlet — no 404, no redirect") in one
 * suite: both exercise the SAME real, generated route tree
 * (`src/routeTree.gen.ts`), mounted through a real `RouterProvider` with an
 * in-memory history — no mocking of the router itself (§6.2).
 */
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
 * These four tests identify WHICH child the nested layout rendered. They used
 * `route-shell`'s `data-route-id` as that identifier; Phase 2 wired both
 * children to their real pages and deleted `RouteShell`, so they now assert
 * on a landmark unique to each child instead.
 *
 * Both landmarks are deliberately chosen to be unreachable from the settings
 * DRAWER, which lists "AI Configuration" and "Secrets" as nav items on every
 * settings route — a bare text query for either would pass without the child
 * rendering at all:
 *   - model-configuration -> the `OpenAI Template` TAB (role-scoped; the tab
 *     bar belongs to `AIConfiguration`, and no drawer item carries that name)
 *   - secrets -> the `Search secrets` input placeholder (rendered by
 *     `SecretsContent`'s own `DrawerPageHeader`)
 */
describe('settings nested layout', () => {
  it('RED/GREEN (e): renders the correct child for a known tab (model-configuration)', async () => {
    mountAt('/settings/model-configuration');

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: 'OpenAI Template' })).toBeInTheDocument();
    });
  });

  it('RED/GREEN (e): renders a different child for a different known tab (secrets)', async () => {
    mountAt('/settings/secrets');

    await waitFor(() => {
      expect(screen.getByPlaceholderText('Search secrets')).toBeInTheDocument();
    });
  });

  /*
   * The default tab is General, as it is in the reference
   * (`pages/settings/index.jsx:130`) and on a live deployment. It used to be
   * AI Providers here, which is why this assertion used to look for that
   * tab's "OpenAI Template" control.
   */
  it('index redirect: /settings alone lands on project-general', async () => {
    const router = mountAt('/settings');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/project-general');
    });
  });

  it('D4 ROUTE-076 anomaly: an unknown tab is handled by SettingsRedirect which redirects to project-general', async () => {
    const router = mountAt('/settings/this-tab-does-not-exist');

    await waitFor(() => {
      // SettingsRedirect fires an async redirect for unknown tabs
      expect(router.state.location.pathname).toBe('/settings/project-general');
    });

    await waitFor(() => {
      expect(router.state.matches.at(-1)?.routeId).toBe('/_shell/settings/project-general');
    });
  });
  /**
   * NAVIGATION GOES THROUGH THE ROUTER.
   *
   * The drawer used to move between tabs with
   * `window.history.replaceState(null, '', `/settings/${tabId}`)`. That wrote
   * a path with no base. So on a deployment served under /app/ the address bar
   * read `/settings/secrets`. nginx answers that URL with `404 page not found`
   * on reload, bookmark or share. It also replaced the history entry, so Back
   * could not return to the previous tab.
   *
   * The router's own location is the check: it only changes when the router
   * performed the navigation.
   */
  it('moves between tabs through the router, and keeps a history entry behind', async () => {
    const router = mountAt('/settings/model-configuration');
    await waitFor(() => {
      expect(screen.getByRole('tab', { name: 'OpenAI Template' })).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole('button', { name: 'Secrets' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/secrets');
    });

    // push, not replace: the tab the user came from is still one step back.
    router.history.back();
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });
});
