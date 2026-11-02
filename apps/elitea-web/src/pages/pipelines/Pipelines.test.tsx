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

import { Pipelines } from './Pipelines';
import { renderPipelinesRoute } from './__tests__/testRouter';

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

describe('Pipelines', () => {
  it('redirects an unrecognised :tab to the first tab for the current project', async () => {
    setConfig('1');
    const { router } = renderPipelinesRoute(<Pipelines />, '/pipelines/bogus', { projectId: '1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/latest'));
  });

  it('shows Latest on the public project; the placeholder My liked / Trending tabs and Admin are hidden', async () => {
    setConfig('1');
    renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByTestId('pipelines-tab-latest')).toBeInTheDocument();
    expect(screen.queryByTestId('pipelines-tab-my-liked')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipelines-tab-trending')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipelines-tab-admin')).not.toBeInTheDocument();
  });

  it('shows the Admin tab on the public project when the caller has the applications-list permission', async () => {
    setConfig('1');
    server.use(
      getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: true }]),
    );
    renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByTestId('pipelines-tab-admin')).toBeInTheDocument();
  });

  it('shows only the single "All" tab on a private project', async () => {
    setConfig('1');
    renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '9' });

    expect(await screen.findByTestId('pipelines-tab-all')).toBeInTheDocument();
    expect(screen.queryByTestId('pipelines-tab-drafts')).not.toBeInTheDocument();
  });

  it("clicking a tab navigates to its :tab URL", async () => {
    setConfig('1');
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: true }]));
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    await user.click(await screen.findByTestId('pipelines-tab-admin'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/admin'));
  });

  it('sends an old link to a hidden placeholder tab to the first tab', async () => {
    setConfig('1');
    const { router } = renderPipelinesRoute(<Pipelines />, '/pipelines/trending', { projectId: '1' });

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/latest'));
  });

  it('shows the tab-badge count when usePipelinesData resolves one, e.g. "Latest (2)"', async () => {
    setConfig('1');
    server.use(
      getListPublicApplicationsMockHandler({
        rows: [
          {
            project_id: '1',
            id: '1',
            name: 'P1',
            description: '',
            version_id: 'v1',
            version_name: 'base',
            agent_type: 'pipeline',
            meta: null,
            tags: [],
            likes: 0,
            is_liked: false,
          },
          {
            project_id: '1',
            id: '2',
            name: 'P2',
            description: '',
            version_id: 'v1',
            version_name: 'base',
            agent_type: 'pipeline',
            meta: null,
            tags: [],
            likes: 0,
            is_liked: false,
          },
        ],
        total: 2,
      }),
    );
    renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByText('Latest (2)')).toBeInTheDocument();
  });

  /** COMPOSITION ROOT — see `pages/agents/Applications.test.tsx` (issue 841). */
  it('renders its tabs and its import control inside the page toolbar, and no heading of its own', async () => {
    renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '9' });

    const toolbar = await screen.findByTestId('page-toolbar');
    expect(toolbar).toContainElement(await screen.findByTestId('pipelines-tab-all'));
    expect(toolbar).toContainElement(screen.getByRole('tablist', { name: 'Pipelines' }));
    expect(toolbar).toContainElement(screen.getByTestId('pipelines-import-button'));
    expect(screen.queryByRole('heading', { level: 1 })).toBeNull();
  });

  it('offers "+ New pipeline" to a caller who may create, and opens the create page', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.create', enabled: true }]));
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '9' });

    const visited: string[] = [];
    router.subscribe('onBeforeNavigate', (event) => visited.push(event.toLocation.pathname));

    await user.click(await screen.findByRole('button', { name: 'New pipeline' }));

    // The fixture renders this page on every route, so the create route's own page is not there to stay on.
    await waitFor(() => expect(visited).toContain('/pipelines/create'));
  });

  it('offers no create action without the create permission', async () => {
    renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '9' });

    await screen.findByTestId('page-toolbar');
    expect(screen.queryByTestId('pipelines-create')).toBeNull();
  });
});

/*
 * The header's own controls, at the composition root — see
 * `pages/agents/Applications.test.tsx` for the same three claims and why they
 * cannot be made by a component test.
 */
describe('Pipelines — header controls', () => {
  it('mounts the search box and the view toggle on a tab that has a list', async () => {
    setConfig('1');
    renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '2' });

    expect(await screen.findByTestId('pipeline-search-input')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-table-view-button')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-card-view-button')).toBeInTheDocument();
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
    renderPipelinesRoute(<Pipelines />, '/pipelines/all', { projectId: '2' });

    await user.type(await screen.findByTestId('pipeline-search-input'), 'alpha');

    await waitFor(() => expect(seen).toContain('alpha'));
  });
});
