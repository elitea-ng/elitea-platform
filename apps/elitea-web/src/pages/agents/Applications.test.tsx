import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { delay, http, HttpResponse } from 'msw';

import {
  getListApplicationsMockHandler,
  getListPublicApplicationsMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { Applications } from './Applications';
import { renderAgentsRoute } from './__tests__/testRouter';

const globals = globalThis as unknown as Record<string, unknown>;

function setConfig(publicProjectId: string): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: publicProjectId,
  };
  resetConfigForTests();
}

function emptyApplicationList() {
  return { rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListApplicationsMockHandler(emptyApplicationList()),
    getListPublicApplicationsMockHandler({ rows: [], total: 0 }),
    getPermissionListMockHandler([]),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('Applications', () => {
  it('redirects an unrecognised :tab to the first tab for the current project', async () => {
    setConfig('1');
    const { router } = renderAgentsRoute(<Applications />, '/agents/bogus', { projectId: '1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/latest'));
  });

  it('shows Latest on the public project; the placeholder My liked / Trending tabs and Admin are hidden', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByTestId('agents-tab-latest')).toBeInTheDocument();
    expect(screen.queryByTestId('agents-tab-my-liked')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agents-tab-trending')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agents-tab-admin')).not.toBeInTheDocument();
  });

  it('shows the Admin tab on the public project when the caller has the applications-list permission', async () => {
    setConfig('1');
    server.use(
      getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: true }]),
    );
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByTestId('agents-tab-admin')).toBeInTheDocument();
  });

  it('shows no tabs on a private project: its agents are one list (ADR-0051)', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    expect(await screen.findByTestId('agent-summary-bar')).toBeInTheDocument();
    for (const tab of ['all', 'drafts', 'published', 'moderation', 'approval', 'rejected']) {
      expect(screen.queryByTestId(`agents-tab-${tab}`)).not.toBeInTheDocument();
    }
    expect(screen.queryByTestId('page-toolbar')).not.toBeInTheDocument();
  });

  it("clicking a tab navigates to its :tab URL", async () => {
    setConfig('1');
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: true }]));
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    await user.click(await screen.findByTestId('agents-tab-admin'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/admin'));
  });

  it('keeps a direct load of /agents/teams on the Teams mode while the permission list loads (issue 1620)', async () => {
    setConfig('1');
    server.use(
      http.get('*/auth/permissions/prompt_lib/:projectId', async () => {
        await delay(50);
        return HttpResponse.json([{ name: 'models.teams.list', enabled: true }]);
      }),
      http.get('*/elitea_core/agent_teams/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
    );
    const { router } = renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/teams', { projectId: '9' });

    expect(await screen.findByTestId('agents-mode-teams')).toHaveAttribute('aria-pressed', 'true');
    expect(router.state.location.pathname).toBe('/agents/teams');
  });

  it('shows the agents, not Teams, when the caller has no teams right', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/teams', { projectId: '9' });

    expect(await screen.findByText('No agents yet')).toBeInTheDocument();
  });

  it('sends an old status tab link to the list with its status filter on', async () => {
    setConfig('1');
    const { router } = renderAgentsRoute(<Applications />, '/agents/drafts', { projectId: '9' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/all'));
    expect(router.state.location.search).toMatchObject({ statuses: ['draft'] });
  });

  it('sends an old link to a hidden placeholder tab to the first tab', async () => {
    setConfig('1');
    const { router } = renderAgentsRoute(<Applications />, '/agents/trending', { projectId: '1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/latest'));
  });

  it('shows the tab-badge count when useApplicationsData resolves one, e.g. "Latest (3)"', async () => {
    setConfig('1');
    server.use(getListPublicApplicationsMockHandler({ rows: [], total: 3 }));
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByText('Latest (3)')).toBeInTheDocument();
  });

  /**
   * COMPOSITION ROOT — every control of the list is in the right sidebar's List
   * tab (ADR-0051), and not in a toolbar row. The import control and the search
   * input keep their test ids, because the journeys and the visual baselines
   * pin them.
   */
  it('renders its search and import control inside the List tab, with no toolbar row and no heading of its own', async () => {
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '9' });

    const tab = await screen.findByTestId('side-panel-tab-list');
    expect(await within(tab).findByTestId('agent-search-input')).toBeInTheDocument();
    expect(within(tab).getByTestId('agents-import-button')).toBeInTheDocument();
    expect(screen.queryByTestId('page-toolbar')).toBeNull();
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });

  it('offers "+ New agent" to a caller who may create, and opens the create page', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.create', enabled: true }]));
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    const visited: string[] = [];
    router.subscribe('onBeforeNavigate', (event) => visited.push(event.toLocation.pathname));

    await user.click(await screen.findByRole('button', { name: 'New agent' }));

    // The fixture renders this page on every route, so the create route's own page is not there to stay on.
    await waitFor(() => expect(visited).toContain('/agents/create'));
  });

  it('offers no create action without the create permission', async () => {
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    await screen.findByTestId('agent-summary-bar');
    expect(screen.queryByTestId('agents-create')).toBeNull();
    expect(screen.queryByTestId('agents-create-summary')).toBeNull();
  });
});

/*
 * The header's own controls — the composition root, not the components.
 * `PageSearch.test.tsx` and `ListViewToggle.test.tsx` prove each control
 * works; these tests prove this page MOUNTS them, which is the half that was
 * missing (`ViewToggle` and the rail search box were both disclosed as
 * unported in this page's module comment) and the half a component test can
 * never see.
 */
describe('Applications — list controls', () => {
  it('mounts the search box and the view toggle in the List tab', async () => {
    setConfig('1');
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '2' });

    expect(await screen.findByTestId('agent-search-input')).toBeInTheDocument();
    expect(screen.getByTestId('agent-table-view-button')).toBeInTheDocument();
    expect(screen.getByTestId('agent-card-view-button')).toBeInTheDocument();
  });

  it('hands the typed query to the list, which sends it to the server', async () => {
    setConfig('1');
    const seen: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seen.push(new URL(info.request.url).searchParams.get('query'));
        return emptyApplicationList();
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<SidePanelHarness><Applications /></SidePanelHarness>, '/agents/all', { projectId: '2' });

    await user.type(await screen.findByTestId('agent-search-input'), 'alpha');

    await waitFor(() => expect(seen).toContain('alpha'));
  });
});
