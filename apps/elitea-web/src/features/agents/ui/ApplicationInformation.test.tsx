import type { ComponentProps } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { ApplicationInformation } from './ApplicationInformation';

/** `CopyToClipboardButton`/`StyledShowContextModal` read `theme.vars.palette.*` — this file drives its own `RouterProvider` (needed for `useSelectedProjectId`'s `useRouteContext`) rather than the shared `renderWithProviders` helper, so the theme has to be wired in here too. */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  // The trigger read is disabled while the route is unmounted — see
  // `shared/config/backendCapabilities`. The trigger-row cases need it.
  setBackendCapabilityForTests('pipelineTriggers', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

function renderInfo(props: Partial<ComponentProps<typeof ApplicationInformation>> = {}) {
  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ApplicationInformation
            id="app-1"
            versionId="v1"
            isPipeline={false}
            {...props}
          />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

describe('ApplicationInformation', () => {
  it('wraps its content in the titled, collapsible "Information" accordion', async () => {
    renderInfo();
    expect(await screen.findByTestId('agent-information-section')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Information' })).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'Information' })).toBeInTheDocument();
  });

  it('shows the agent id and copy affordance', async () => {
    renderInfo();
    expect(await screen.findByTestId('copy-id')).toBeInTheDocument();
    expect(screen.getByText('Agent ID:')).toBeInTheDocument();
  });

  it('shows the version id when present', async () => {
    renderInfo();
    expect(await screen.findByText('Version ID:')).toBeInTheDocument();
  });

  it('labels the id "Pipeline ID:" when isPipeline is set', async () => {
    renderInfo({ isPipeline: true });
    expect(await screen.findByText('Pipeline ID:')).toBeInTheDocument();
  });

  it('shows a "Forked from" row with the fallback label while the original application is unresolved', async () => {
    renderInfo({ isForked: true, forkedProjectId: 'p2', forkedApplicationId: 'app-2' });
    expect(await screen.findByText('Forked from:')).toBeInTheDocument();
    expect(screen.getByText('Original agent')).toBeInTheDocument();
  });

  it('shows the original application name once the fork lookup resolves', async () => {
    server.use(
      http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({
          id: 'app-2',
          name: 'Original Agent Name',
          description: '',
          icon: '',
          owner_id: 'u1',
          created_at: '2026-01-01T00:00:00Z',
          versions: [],
        }),
      ),
    );
    renderInfo({ isForked: true, forkedProjectId: 'p2', forkedApplicationId: 'app-2' });
    expect(await screen.findByText('Original Agent Name')).toBeInTheDocument();
  });

  it('renders a "Show" link that opens the pipeline modal', async () => {
    renderInfo({ showPipeline: true, pipelineInstructions: 'nodes: []' });
    const showLink = await screen.findByText('Show');
    expect(screen.getByText('Pipeline:')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    showLink.click();
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });

  /**
   * #899: the pylon trigger route these rows used to read is gone. The panel
   * now derives the type from the two Go facilities, and `timezone` /
   * `webhook type` are no longer rows at all — nothing stores them.
   */
  describe('pipeline trigger rows', () => {
    const SCHEDULE = '*/pipeline_schedules/prompt_lib/:projectId/:versionId';
    const TRIGGER = '*/pipeline_triggers/prompt_lib/:projectId/:versionId';

    it('renders no trigger row when neither facility is configured', async () => {
      server.use(
        http.get(SCHEDULE, () => HttpResponse.json({ configured: false, active: false })),
        http.get(TRIGGER, () => HttpResponse.json({ configured: false })),
      );
      renderInfo({ isPipeline: true, versionId: '1' });
      await screen.findByText('Version ID:');
      expect(screen.queryByText('Trigger:')).not.toBeInTheDocument();
    });

    it('renders cron and last-run rows for a configured schedule', async () => {
      server.use(
        http.get(SCHEDULE, () => HttpResponse.json({ configured: true, active: true, cron: '0 9 * * *', last_run: '2026-07-20T09:00:00Z' })),
        http.get(TRIGGER, () => HttpResponse.json({ configured: false })),
      );
      renderInfo({ isPipeline: true, versionId: '1' });
      expect(await screen.findByText('Trigger:')).toBeInTheDocument();
      expect(screen.getByText('Schedule')).toBeInTheDocument();
      expect(screen.getByText('Schedule:')).toBeInTheDocument();
      expect(screen.getByText('0 9 * * *')).toBeInTheDocument();
      expect(screen.getByText('Last run:')).toBeInTheDocument();
    });

    it('omits the cron/last-run rows a schedule does not carry', async () => {
      server.use(
        http.get(SCHEDULE, () => HttpResponse.json({ configured: true, active: true })),
        http.get(TRIGGER, () => HttpResponse.json({ configured: false })),
      );
      renderInfo({ isPipeline: true, versionId: '1' });
      expect(await screen.findByText('Trigger:')).toBeInTheDocument();
      expect(screen.queryByText('Schedule:')).not.toBeInTheDocument();
      expect(screen.queryByText('Last run:')).not.toBeInTheDocument();
    });

    it('names the webhook when only an inbound trigger is configured', async () => {
      server.use(
        http.get(SCHEDULE, () => HttpResponse.json({ configured: false, active: false })),
        http.get(TRIGGER, () => HttpResponse.json({ configured: true, token_id: 'tok', url: '/api/v2/pipeline_trigger/1/tok' })),
      );
      renderInfo({ isPipeline: true, versionId: '1' });
      expect(await screen.findByText('Trigger:')).toBeInTheDocument();
      expect(screen.getByText('Webhook')).toBeInTheDocument();
    });

    /** A revoked row is KEPT as evidence, and the inbound path refuses it — so it must not read as a live trigger here either. */
    it('treats a revoked trigger as no trigger', async () => {
      server.use(
        http.get(SCHEDULE, () => HttpResponse.json({ configured: false, active: false })),
        http.get(TRIGGER, () => HttpResponse.json({ configured: true, token_id: 'tok', revoked_at: '2026-07-20T09:00:00Z' })),
      );
      renderInfo({ isPipeline: true, versionId: '1' });
      await screen.findByText('Version ID:');
      expect(screen.queryByText('Trigger:')).not.toBeInTheDocument();
    });

    it('renders no trigger row at all when isPipeline is false, even with a versionId', async () => {
      renderInfo({ isPipeline: false, versionId: '1' });
      await screen.findByText('Version ID:');
      expect(screen.queryByText('Trigger:')).not.toBeInTheDocument();
    });
  });

  describe('forked-from permission tooltip', () => {
    it('is not disabled and has the "go to original" tooltip when the fork lookup succeeds', async () => {
      server.use(
        http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
          HttpResponse.json({
            id: 'app-2',
            name: 'Original Agent Name',
            description: '',
            icon: '',
            owner_id: 'u1',
            created_at: '2026-01-01T00:00:00Z',
            versions: [],
          }),
        ),
      );
      renderInfo({ isForked: true, forkedProjectId: 'p2', forkedApplicationId: 'app-2' });
      const nameNode = await screen.findByText('Original Agent Name');
      expect(nameNode).toHaveAttribute('aria-disabled', 'false');
    });

    it('marks the row aria-disabled on a 403 from the fork lookup', async () => {
      server.use(
        http.get('*/elitea_core/application/prompt_lib/:projectId/:applicationId', () =>
          HttpResponse.json({ error: 'forbidden' }, { status: 403 }),
        ),
      );
      renderInfo({ isForked: true, forkedProjectId: 'p2', forkedApplicationId: 'app-2' });
      const nameNode = await screen.findByText('Original agent');
      await waitFor(() => expect(nameNode).toHaveAttribute('aria-disabled', 'true'));
    });
  });
});
