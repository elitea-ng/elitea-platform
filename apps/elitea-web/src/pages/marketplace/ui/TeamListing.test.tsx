import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { MarketplaceListingDetail, MarketplaceTeamMember } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';
import { MarketplaceListing } from '../MarketplaceListing';

const GITHUB = {
  items: [
    { key: 'toolkit:github:Repo', kind: 'toolkit', type: 'github', name: 'Repo', tools: ['list_issues'], has_write_tools: false },
  ],
  model_class: 'tool_calling',
  tier: 'low',
  has_write_tools: false,
};
const NOTHING = { items: [], model_class: 'chat', tier: 'low', has_write_tools: false };

function member(position: number, name: string, overrides: Partial<MarketplaceTeamMember> = {}): MarketplaceTeamMember {
  return {
    position,
    listing_id: String(10 + position),
    version_id: String(100 + position),
    name,
    description: `${name} does its part.`,
    icon_url: null,
    version_name: 'v1',
    hireable: true,
    requirements: NOTHING,
    hired_application_id: null,
    ...overrides,
  };
}

function detailOf(members: MarketplaceTeamMember[]): MarketplaceListingDetail {
  return {
    listing: {
      id: '9',
      kind: 'team',
      entity_id: '0',
      name: 'Release Crew',
      description: 'Ships the release notes.',
      category: '',
      tags: [],
      publisher: { id: '3', name: 'Dana Owner' },
      tier: 'low',
      current_version: { id: '1', name: 'v1', published_at: '2026-11-26T09:00:00Z' },
      hires: 0,
      rating_avg: null,
      rating_count: 0,
      likes: 0,
      is_liked: false,
      featured: false,
      required_types: ['github'],
      has_write_tools: false,
      state: 'listed',
      hired: [],
    },
    versions: [
      {
        id: '1',
        name: 'v1',
        state: 'current',
        release_notes: '',
        digest: 'abcdef0123456789',
        published_at: '2026-11-26T09:00:00Z',
        requirements: GITHUB,
      },
    ],
    can_manage: false,
    can_moderate: false,
    team: { version_id: '1', floor_policy: 'lead', lead_position: 2, members },
  };
}

const EMPTY_FEEDBACK = {
  items: [],
  total: 0,
  versions: [],
  overall: { version_id: '0', version_name: '', rating_avg: null, rating_count: 0, distribution: [0, 0, 0, 0, 0], report_count: 0 },
  signals: {
    listing: {
      day: '2026-11-26',
      active_hires: 0,
      hires_total: 0,
      dismissed: 0,
      dismissal_rate: null,
      retention_cohort: 0,
      retention_30d: null,
      upgrade_eligible: 0,
      upgrade_adoption: null,
      live: true,
    },
    versions: {},
  },
  review_versions: [],
  can_review: false,
  my_review: null,
  can_reply: false,
  can_flag: false,
  can_moderate: false,
};

let hireBodies: unknown[] = [];

function mock(detail: MarketplaceListingDetail, permissions: string[], hire?: () => Response): void {
  hireBodies = [];
  server.use(
    http.get('/api/v2/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json(permissions.map((name) => ({ name, enabled: true }))),
    ),
    http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', () => HttpResponse.json(detail)),
    http.get('/api/v2/elitea_core/marketplace_feedback/prompt_lib/:projectId/:listingId', () =>
      HttpResponse.json(EMPTY_FEEDBACK),
    ),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
      HttpResponse.json({ rows: [{ id: '12', type: 'github', name: 'GitHub (team)' }], total: 1 }),
    ),
    http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
    http.post('/api/v2/elitea_core/marketplace_team_hire/prompt_lib/:projectId', async ({ request }) => {
      hireBodies.push(await request.json());
      if (hire !== undefined) return hire();
      return HttpResponse.json(
        {
          team_id: '40',
          name: 'Release Crew',
          mode: 'install',
          members: [
            { position: 1, application_id: '81', version_id: '91', reused: false, tools_enabled: true, awaiting_approval: false, unbound: [] },
            { position: 2, application_id: '82', version_id: '92', reused: true, tools_enabled: true, awaiting_approval: false, unbound: [] },
          ],
        },
        { status: 201 },
      );
    }),
  );
}

function mount(path: string): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <Outlet />
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([
      createRoute({ getParentRoute: () => rootRoute, path: '/marketplace/$kind/$listingId', component: MarketplaceListing }),
      createRoute({ getParentRoute: () => rootRoute, path: '/toolkits/create', component: () => <p /> }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
    context: { auth: { getSelectedProjectId: () => '5' } },
  } as never);
  renderWithTheme(<RouterProvider router={router} />);
}

const HIRE_PERMISSIONS = ['models.applications.applications.create', 'models.teams.create'];

describe('a team listing', () => {
  beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
  afterEach(() => resetGeneratedClient());

  it('shows the members as their listings, the lead marked, and the members’ requirements', async () => {
    mock(detailOf([member(1, 'Writer', { requirements: GITHUB }), member(2, 'Reviewer')]), HIRE_PERMISSIONS);
    mount('/marketplace/teams/9');

    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('Release Crew');
    const members = screen.getByTestId('marketplace-team-members');
    expect(within(members).getByRole('link', { name: 'Writer' })).toHaveAttribute('href', '/marketplace/agents/11');
    expect(within(screen.getByTestId('marketplace-team-member-2')).getByText('Lead')).toBeInTheDocument();
    expect(screen.getByText('Reviewer leads the conversation.')).toBeInTheDocument();
  });

  it('needs the team create permission to hire', async () => {
    mock(detailOf([member(1, 'Writer')]), ['models.applications.applications.create']);
    mount('/marketplace/teams/9');
    await screen.findByTestId('marketplace-listing-name');
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeDisabled());
  });

  it('hires the team with each member’s connection, reusing an installed member', async () => {
    mock(
      detailOf([member(1, 'Writer', { requirements: GITHUB }), member(2, 'Reviewer', { requirements: GITHUB, hired_application_id: '77' })]),
      HIRE_PERMISSIONS,
    );
    mount('/marketplace/teams/9');
    const user = userEvent.setup();
    await screen.findByTestId('marketplace-listing-name');
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeEnabled());
    await user.click(screen.getByTestId('marketplace-hire'));
    const dialog = await screen.findByTestId('team-hire-dialog');

    // The reviewer is reused under Install; the writer still needs GitHub.
    expect(within(dialog).getByTestId('team-hire-reused-2')).toBeInTheDocument();
    expect(within(dialog).getByTestId('team-hire-confirm')).toBeDisabled();
    await user.click(within(dialog).getByRole('combobox', { name: 'github for Repo' }));
    await user.click(await screen.findByRole('option', { name: 'GitHub (team)' }));
    await user.click(within(dialog).getByTestId('team-hire-confirm'));

    await waitFor(() =>
      expect(hireBodies).toEqual([
        {
          listing_id: '9',
          version_id: '1',
          mode: 'install',
          name: 'Release Crew',
          members: [{ position: 1, bindings: [{ requirement_key: 'toolkit:github:Repo', toolkit_id: '12' }] }],
        },
      ]),
    );
    expect(await within(dialog).findByTestId('team-hire-done')).toHaveTextContent('The team Release Crew is in your Teams.');
  });

  it('names the member a refusal is about', async () => {
    mock(detailOf([member(1, 'Writer')]), HIRE_PERMISSIONS, () =>
      HttpResponse.json(
        { error: 'hire_needs_shared_project', msg: 'Writer: needs approval', position: 1, member: 'Writer' },
        { status: 409 },
      ),
    );
    mount('/marketplace/teams/9');
    const user = userEvent.setup();
    await screen.findByTestId('marketplace-listing-name');
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeEnabled());
    await user.click(screen.getByTestId('marketplace-hire-menu'));
    await user.click(await screen.findByTestId('marketplace-hire-fork'));
    const dialog = await screen.findByTestId('team-hire-dialog');
    expect(within(dialog).getByRole('radio', { name: 'Fork' })).toBeChecked();
    await user.click(within(dialog).getByTestId('team-hire-confirm'));
    expect(await within(dialog).findByTestId('team-hire-error')).toHaveTextContent(/^Writer: This hire needs a second person/);
  });
});
