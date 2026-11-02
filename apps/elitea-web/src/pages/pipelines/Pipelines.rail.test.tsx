import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import {
  getListApplicationsMockHandler,
  getListPublicApplicationsMockHandler,
} from '@/shared/api/generated/applications/applications.msw';
import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getGetSocialTrendingAuthorsMockHandler } from '@/shared/api/generated/social/social.msw';
import { getListTagsMockHandler } from '@/shared/api/generated/tags/tags.msw';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';
import { SidePanelHarness } from '@/test/sidePanelHarness';

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

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    getListApplicationsMockHandler({ rows: [], total: 0, page: 1, page_size: 20, total_pages: 0 }),
    getListPublicApplicationsMockHandler({ rows: [], total: 0 }),
    getPermissionListMockHandler([]),
    getListTagsMockHandler({ rows: [{ id: 1, name: 'alpha', data: {} }], total: 1 }),
    getGetSocialTrendingAuthorsMockHandler([{ id: '7', name: 'Grace Hopper', email: 'grace@example.com', avatar: '', likes: 3 }]),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
});

describe('Pipelines Filters tab', () => {
  it('offers the Filters tab with the project tag chips', async () => {
    setConfig('1');
    renderPipelinesRoute(<SidePanelHarness><Pipelines /></SidePanelHarness>, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByTestId('side-panel-tab-filters')).toContainElement(await screen.findByTestId('entity-list-filters'));
    expect(await screen.findByTestId('tags-panel-chip-alpha')).toBeInTheDocument();
  });

  it('shows Trending Authors on the public feed tabs', async () => {
    setConfig('1');
    renderPipelinesRoute(<SidePanelHarness><Pipelines /></SidePanelHarness>, '/pipelines/latest', { projectId: '1' });

    expect(await screen.findByText('Trending Authors')).toBeInTheDocument();
    expect(await screen.findByText('Grace Hopper')).toBeInTheDocument();
    expect(screen.queryByTestId('entity-rail-author')).toBeNull();
  });

  it('marks a clicked chip as selected', async () => {
    setConfig('1');
    const user = userEvent.setup();
    renderPipelinesRoute(<SidePanelHarness><Pipelines /></SidePanelHarness>, '/pipelines/latest', { projectId: '1' });

    await user.click(await screen.findByTestId('tags-panel-chip-alpha'));
    await waitFor(() => expect(screen.getByTestId('tags-panel-chip-alpha')).toHaveAttribute('aria-pressed', 'true'));
  });
});
