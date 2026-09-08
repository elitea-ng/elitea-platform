import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import {
  getListApplicationsMockHandler,
  getListPublicApplicationsMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

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

  it('shows the four public tabs on the public project, Admin hidden by default', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByTestId('agents-tab-latest')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-my-liked')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-trending')).toBeInTheDocument();
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

  it('shows the six private tabs on a private project', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    expect(await screen.findByTestId('agents-tab-all')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-drafts')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-published')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-moderation')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-approval')).toBeInTheDocument();
    expect(screen.getByTestId('agents-tab-rejected')).toBeInTheDocument();
  });

  it('clicking a tab navigates to its :tab URL and renders that tab\'s content', async () => {
    setConfig('1');
    const user = userEvent.setup();
    const { router } = renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    await screen.findByTestId('agents-tab-trending');
    await user.click(screen.getByTestId('agents-tab-trending'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/trending'));
    expect(await screen.findByTestId('agents-trending-unavailable')).toBeInTheDocument();
  });

  it('shows the tab-badge count when useApplicationsData resolves one, e.g. "Latest (3)"', async () => {
    setConfig('1');
    server.use(getListPublicApplicationsMockHandler({ rows: [], total: 3 }));
    renderAgentsRoute(<Applications />, '/agents/latest', { projectId: '1' });

    expect(await screen.findByText('Latest (3)')).toBeInTheDocument();
  });

  /**
   * COMPOSITION ROOT — the header is `widgets/page-header` now, and not this
   * page's own tab bar (issue 841). The tab test ids, the tablist aria label
   * and the import control must survive the move, because the journeys and
   * the visual baselines pin them.
   */
  it('renders its tabs and its import control inside the shared page header', async () => {
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '9' });

    const header = await screen.findByTestId('page-header');
    expect(header).toContainElement(await screen.findByTestId('agents-tab-all'));
    expect(header).toContainElement(screen.getByRole('tablist', { name: 'Agents' }));
    expect(header).toContainElement(screen.getByTestId('agents-import-button'));
  });
});

/*
 * The header's own controls — the composition root, not the components.
 * `ListSearchField.test.tsx` and `ListViewToggle.test.tsx` prove each control
 * works; these tests prove this page MOUNTS them, which is the half that was
 * missing (`ViewToggle` and the rail search box were both disclosed as
 * unported in this page's module comment) and the half a component test can
 * never see.
 */
describe('Applications — header controls', () => {
  it('mounts the search box and the view toggle on a tab that has a list', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '2' });

    expect(await screen.findByTestId('agent-search-input')).toBeInTheDocument();
    expect(screen.getByTestId('agent-table-view-button')).toBeInTheDocument();
    expect(screen.getByTestId('agent-card-view-button')).toBeInTheDocument();
  });

  it('omits both on a tab whose body has no rows to act on', async () => {
    setConfig('1');
    renderAgentsRoute(<Applications />, '/agents/trending', { projectId: '1' });

    expect(await screen.findByTestId('agents-tab-trending')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-search-input')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-table-view-button')).not.toBeInTheDocument();
  });

  it('hands the typed query to the tab body, which sends it to the server', async () => {
    setConfig('1');
    const seen: (string | null)[] = [];
    server.use(
      getListApplicationsMockHandler((info) => {
        seen.push(new URL(info.request.url).searchParams.get('query'));
        return emptyApplicationList();
      }),
    );
    const user = userEvent.setup();
    renderAgentsRoute(<Applications />, '/agents/all', { projectId: '2' });

    await user.type(await screen.findByTestId('agent-search-input'), 'alpha');

    await waitFor(() => expect(seen).toContain('alpha'));
  });
});
