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

  it('shows the four public tabs on the public project, Admin hidden by default', async () => {
    setConfig('1');
    renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByTestId('pipelines-tab-latest')).toBeInTheDocument();
    expect(screen.getByTestId('pipelines-tab-my-liked')).toBeInTheDocument();
    expect(screen.getByTestId('pipelines-tab-trending')).toBeInTheDocument();
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

  it("clicking a tab navigates to its :tab URL and renders that tab's content", async () => {
    setConfig('1');
    const user = userEvent.setup();
    const { router } = renderPipelinesRoute(<Pipelines />, '/pipelines/latest', { projectId: '1' });

    await screen.findByTestId('pipelines-tab-trending');
    await user.click(screen.getByTestId('pipelines-tab-trending'));

    await waitFor(() => expect(router.state.location.pathname).toBe('/pipelines/trending'));
    expect(await screen.findByTestId('pipelines-trending-unavailable')).toBeInTheDocument();
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
});
