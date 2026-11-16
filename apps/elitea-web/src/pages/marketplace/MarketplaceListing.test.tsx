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
import { afterEach, describe, expect, it } from 'vitest';

import type {
  MarketplaceListingDetail,
  MarketplaceListingSummary,
  MarketplaceVersion,
} from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../test/setup';

import { MarketplaceListing } from './MarketplaceListing';

const REQUIREMENTS = {
  items: [
    {
      key: 'toolkit:github',
      kind: 'toolkit',
      type: 'github',
      name: 'Release bot',
      tools: ['create_release'],
      has_write_tools: true,
    },
  ],
  model_class: 'tool_calling',
  tier: 'medium',
  has_write_tools: true,
};
const NO_REQUIREMENTS = { items: [], model_class: 'chat', tier: 'low', has_write_tools: false };

function version(
  id: string,
  name: string,
  state: string,
  overrides: Partial<MarketplaceVersion> = {},
): MarketplaceVersion {
  return {
    id,
    name,
    state,
    release_notes: '',
    digest: 'abcdef0123456789abcdef',
    published_at: '2026-10-30T09:00:00Z',
    requirements: REQUIREMENTS,
    ...overrides,
  };
}

const LISTING: MarketplaceListingSummary = {
  id: '7',
  kind: 'agent',
  entity_id: '70',
  name: 'Release Notes Writer',
  description: 'Drafts release notes from merged pull requests.',
  category: 'Development',
  tags: ['github'],
  publisher: { id: '3', name: 'Dana Owner' },
  tier: 'medium',
  current_version: { id: '311', name: 'v2.3', published_at: '2026-10-30T09:00:00Z' },
  hires: 12,
  rating_avg: null,
  rating_count: 0,
  likes: 4,
  is_liked: false,
  featured: false,
  required_types: ['github'],
  has_write_tools: true,
  state: 'listed',
  hired: [],
};

function detailOf(overrides: Partial<MarketplaceListingDetail> = {}): MarketplaceListingDetail {
  return {
    listing: LISTING,
    welcome_message: 'Hello there',
    conversation_starters: ['Summarise the last release'],
    soul_summary: '',
    versions: [
      version('311', 'v2.3', 'current', { release_notes: 'Adds a release tool.' }),
      version('300', 'v2.0', 'deprecated', { release_notes: 'First release.', requirements: NO_REQUIREMENTS }),
      version('250', 'v1.0', 'yanked', { state_reason: 'It leaked a token.' }),
    ],
    can_manage: false,
    can_moderate: false,
    ...overrides,
  };
}

const EMPTY_FEEDBACK = {
  items: [],
  total: 0,
  versions: [],
  overall: {
    version_id: '0',
    version_name: '',
    rating_avg: null,
    rating_count: 0,
    distribution: [0, 0, 0, 0, 0],
    report_count: 0,
  },
  signals: {
    listing: {
      day: '2026-11-15',
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

let requests: string[] = [];
let createBodies: unknown[] = [];
let participantBodies: unknown[] = [];
let hireBodies: unknown[] = [];
let stateBodies: unknown[] = [];

function mockListing(
  detail: MarketplaceListingDetail = detailOf(),
  permissions: string[] = ['models.applications.applications.create'],
): void {
  requests = [];
  createBodies = [];
  participantBodies = [];
  hireBodies = [];
  stateBodies = [];
  server.use(
    http.get('/api/v2/auth/permissions/prompt_lib/:projectId', () =>
      HttpResponse.json(permissions.map((name) => ({ name, enabled: true }))),
    ),
    http.get('/api/v2/elitea_core/public_applications/prompt_lib', ({ request }) => {
      requests.push(request.url);
      return HttpResponse.json({ rows: [], total: 0 });
    }),
    http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', ({ params }) => {
      if (params['listingId'] !== '7') return HttpResponse.json({ error: 'not found' }, { status: 404 });
      return HttpResponse.json(detail);
    }),
    http.get('/api/v2/elitea_core/marketplace_feedback/prompt_lib/:projectId/:listingId', () =>
      HttpResponse.json(EMPTY_FEEDBACK),
    ),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
      HttpResponse.json({
        rows: [
          { id: '12', type: 'github', name: 'GitHub (release bot)' },
          { id: '13', type: 'jira', name: 'Jira' },
        ],
        total: 2,
      }),
    ),
    http.get('/api/v2/elitea_core/applications/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
    http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
    http.post('/api/v2/elitea_core/marketplace_hire/prompt_lib/:projectId', async ({ request }) => {
      hireBodies.push(await request.json());
      return HttpResponse.json(
        {
          application_id: '88',
          version_id: '99',
          mode: 'install',
          tools_enabled: false,
          awaiting_approval: true,
          change_request_id: '5',
        },
        { status: 201 },
      );
    }),
    http.post(
      '/api/v2/elitea_core/marketplace_listing_version/prompt_lib/:projectId/:listingId/:versionId',
      async ({ request, params }) => {
        stateBodies.push({ version: params['versionId'], ...((await request.json()) as object) });
        return HttpResponse.json(version(String(params['versionId']), 'v', 'deprecated'));
      },
    ),
    http.post('/api/v2/elitea_core/conversations/prompt_lib/:projectId', async ({ request }) => {
      createBodies.push(await request.json());
      return HttpResponse.json({ id: '55', name: LISTING.name }, { status: 201 });
    }),
    http.post('/api/v2/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request }) => {
      participantBodies.push(await request.json());
      return HttpResponse.json([]);
    }),
    http.get('/api/v2/elitea_core/public_application/prompt_lib/70/v2.3', () =>
      HttpResponse.json({
        id: '70',
        name: LISTING.name,
        description: LISTING.description,
        version_details: {
          id: '311',
          application_id: '70',
          name: 'v2.3',
          status: 'published',
          agent_type: 'agent',
          instructions: 'Write kindly.',
          welcome_message: 'Hello there',
          conversation_starters: [],
        },
      }),
    ),
  );
}

function mount(path: string) {
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
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/marketplace/$kind/$listingId',
        component: MarketplaceListing,
      }),
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/marketplace',
        component: () => <p data-testid="marketplace-home" />,
      }),
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/chat/$conversationId',
        component: () => <p data-testid="chat-page" />,
      }),
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/agents/$tab/$agentId',
        component: () => <p data-testid="agent-page" />,
      }),
      createRoute({
        getParentRoute: () => rootRoute,
        path: '/toolkits/create',
        component: () => <p data-testid="toolkit-create" />,
      }),
    ]),
    history: createMemoryHistory({ initialEntries: [path] }),
    // The project the conversation starts in.
    context: { auth: { getSelectedProjectId: () => '5' } },
  } as never);
  renderWithTheme(<RouterProvider router={router} />);
  return router;
}

describe('MarketplaceListing', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows an agent as a page read by listing id, with versions, release notes and requirements, and no modal or list read', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('Release Notes Writer');
    expect(screen.getByTestId('marketplace-listing-version')).toHaveTextContent('v2.3');
    expect(screen.getByText('Drafts release notes from merged pull requests.')).toBeInTheDocument();
    expect(screen.getByText('Category: Development · by Dana Owner')).toBeInTheDocument();
    expect(screen.getByText('Summarise the last release')).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    // The by-id read replaces the paged public list.
    expect(requests).toEqual([]);

    // Versions, newest first, each with its state as TEXT, notes and digest.
    expect(screen.getByTestId('marketplace-version-state-311')).toHaveTextContent('State: current');
    expect(screen.getByTestId('marketplace-version-state-300')).toHaveTextContent('State: deprecated');
    expect(screen.getByTestId('marketplace-version-state-250')).toHaveTextContent('State: yanked');
    expect(screen.getByText('Adds a release tool.')).toBeInTheDocument();
    expect(screen.getByText('It leaked a token.')).toBeInTheDocument();
    expect(screen.getAllByText('Digest abcdef01').length).toBeGreaterThan(0);
    expect(screen.getByText('toolkit: github (create_release)')).toBeInTheDocument();
    expect(screen.getAllByText('toolkit: github').length).toBeGreaterThan(0);
  });

  it('keeps Try in chat: it starts a conversation with the public copy at its current version', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    const router = mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-try-in-chat'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat/55'));
    expect(createBodies).toEqual([{ name: 'Release Notes Writer', participants: [] }]);
    expect(participantBodies).toEqual([
      [
        {
          entity_name: 'application',
          entity_meta: { id: '70', name: 'Release Notes Writer', project_id: '1' },
          entity_settings: { version_id: '311' },
        },
      ],
    ]);
  });

  it('shows the instructions on request', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-show-context'));
    expect(await screen.findByText('Write kindly.')).toBeInTheDocument();
  });

  it('says so, with one way back, when the listing is gone', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/999');

    expect(await screen.findByTestId('marketplace-listing-not-found')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to the Marketplace' })).toHaveAttribute('href', '/marketplace');
  });

  it('shows a delisted banner and cannot be hired', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ listing: { ...LISTING, state: 'delisted' } }));
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-delisted')).toBeInTheDocument();
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeDisabled());
  });

  it('offers the publisher Deprecate and Yank, asks to confirm and sends the reason', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ can_manage: true }));
    mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-version-deprecated-311'));
    const confirm = await screen.findByTestId('marketplace-version-confirm');
    await user.type(within(confirm).getByLabelText('Reason (optional)'), 'Use v2.4');
    await user.click(within(confirm).getByRole('button', { name: 'Deprecate' }));
    await waitFor(() => expect(stateBodies).toEqual([{ version: '311', state: 'deprecated', reason: 'Use v2.4' }]));
    // A yanked version can only be restored.
    expect(screen.getByTestId('marketplace-version-available-250')).toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-version-yanked-250')).not.toBeInTheDocument();
  });

  it('gives an administrator Yank, Feature and Delist but not Deprecate', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ can_moderate: true }));
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-version-yanked-311')).toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-version-deprecated-311')).not.toBeInTheDocument();
    expect(screen.getByTestId('marketplace-feature')).toHaveTextContent('Feature');
    expect(screen.getByTestId('marketplace-delist')).toHaveTextContent('Delist');
  });

  it('hides the management buttons from everyone else', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/7');

    await screen.findByTestId('marketplace-listing-name');
    expect(screen.queryByTestId('marketplace-version-yanked-311')).not.toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-admin-bar')).not.toBeInTheDocument();
  });

  it('marks a listing this project hired and offers Hire again', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(
      detailOf({
        listing: {
          ...LISTING,
          hired: [{ application_id: '88', mode: 'install', version_id: '311', version_name: 'v2.3' }],
        },
      }),
    );
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-hired')).toHaveTextContent('Hired v2.3');
    expect(screen.getByTestId('marketplace-hire')).toHaveTextContent('Hire again');
    expect(within(screen.getByTestId('marketplace-used-by')).getByRole('link')).toHaveAttribute(
      'href',
      '/agents/all/88',
    );
  });
});

describe('the hire wizard', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  async function openWizard(): Promise<ReturnType<typeof userEvent.setup>> {
    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeEnabled());
    await user.click(screen.getByTestId('marketplace-hire'));
    await screen.findByTestId('hire-wizard');
    return user;
  }

  it('walks Version, Mode, Bindings, Review and hires an install with its binding', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    const router = mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = await openWizard();
    const wizard = screen.getByTestId('hire-wizard');

    // Version: the current one is chosen; a yanked one cannot be picked; a deprecated one carries a warning.
    expect(within(wizard).getByTestId('hire-step-label')).toHaveTextContent('Step 1 of 4: Version');
    expect(within(wizard).getByRole('radio', { name: 'v2.3 (current)' })).toBeChecked();
    expect(within(wizard).getByRole('radio', { name: 'v1.0 (yanked)' })).toBeDisabled();
    expect(
      within(wizard).getByText('The publisher deprecated this version. You can still hire it.'),
    ).toBeInTheDocument();
    await user.click(within(wizard).getByTestId('hire-next'));

    // Mode: Install is the default.
    expect(within(wizard).getByRole('radio', { name: 'Install (recommended)' })).toBeChecked();
    await user.click(within(wizard).getByTestId('hire-next'));

    // Bindings: cannot go on while the GitHub toolkit is unbound.
    expect(within(wizard).getByTestId('hire-next')).toBeDisabled();
    await user.click(within(wizard).getByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'GitHub (release bot)' }));
    await user.click(within(wizard).getByTestId('hire-next'));

    // Review: the tier, the tools, and the approval it will need.
    expect(within(wizard).getByTestId('hire-review-tools')).toHaveTextContent('create_release');
    expect(within(wizard).getByTestId('hire-review-approval')).toBeInTheDocument();
    await user.click(within(wizard).getByTestId('hire-confirm'));

    await waitFor(() =>
      expect(hireBodies).toEqual([
        {
          listing_id: '7',
          version_id: '311',
          mode: 'install',
          bindings: [{ requirement_key: 'toolkit:github', toolkit_id: '12' }],
        },
      ]),
    );
    expect(await within(wizard).findByTestId('hire-done')).toHaveTextContent('approves the hire change request');
    await user.click(within(wizard).getByTestId('hire-open-agent'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/agents/all/88'));
  });

  it('lets a person hire with a toolkit disabled for now', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = await openWizard();
    const wizard = screen.getByTestId('hire-wizard');

    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'Disable for now' }));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-confirm'));

    await waitFor(() =>
      expect(hireBodies).toEqual([
        {
          listing_id: '7',
          version_id: '311',
          mode: 'install',
          bindings: [{ requirement_key: 'toolkit:github', disabled: true }],
        },
      ]),
    );
  });

  it('Fork from the split button opens the wizard on Fork', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByTestId('marketplace-hire-menu')).toBeEnabled());

    await user.click(screen.getByTestId('marketplace-hire-menu'));
    await user.click(await screen.findByTestId('marketplace-hire-fork'));
    const wizard = await screen.findByTestId('hire-wizard');
    await user.click(within(wizard).getByTestId('hire-next'));
    expect(within(wizard).getByRole('radio', { name: 'Fork' })).toBeChecked();
  });

  it('says a personal project needs a shared one when the server refuses the hire', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ versions: [version('311', 'v2.3', 'current', { requirements: NO_REQUIREMENTS })] }));
    server.use(
      http.post('/api/v2/elitea_core/marketplace_hire/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'hire_needs_shared_project' }, { status: 409 }),
      ),
    );
    mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = await openWizard();
    const wizard = screen.getByTestId('hire-wizard');

    for (let step = 0; step < 3; step += 1) await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-confirm'));

    expect(await within(wizard).findByTestId('hire-error')).toHaveTextContent('Hire it in a shared project.');
  });

  it('cannot hire without the create permission', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf(), []);
    mount('/marketplace/agents/7');

    await screen.findByTestId('marketplace-listing-name');
    expect(screen.getByTestId('marketplace-hire')).toBeDisabled();
  });
});

describe('a skill listing', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('picks a version for Add: the current one by default, never a yanked one', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const skill = {
      ...LISTING,
      id: '7',
      kind: 'skill',
      entity_id: '41',
      name: 'PR Reviewer',
      description: 'Reviews pull requests',
      tags: ['Development'],
    };
    mockListing(
      detailOf({
        listing: skill,
        versions: [
          version('6', 'v1.1', 'current', { requirements: NO_REQUIREMENTS }),
          version('5', 'v1.0', 'yanked', { requirements: NO_REQUIREMENTS }),
        ],
      }),
      ['models.applications.fork.post'],
    );
    server.use(
      http.get('/api/v2/elitea_core/public_skill/prompt_lib/41', () =>
        HttpResponse.json({
          id: 41,
          name: 'PR Reviewer',
          description: 'Reviews pull requests',
          versions: [],
          version_details: {
            id: 6,
            name: 'v1.1',
            status: 'published',
            instructions: 'Check the diff.',
            tags: ['Development'],
          },
        }),
      ),
    );
    mount('/marketplace/skills/7');
    const user = userEvent.setup();

    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('PR Reviewer');
    await waitFor(() =>
      expect(screen.getByTestId('marketplace-skill-instructions')).toHaveTextContent('Check the diff.'),
    );
    await waitFor(() => expect(screen.getByTestId('marketplace-add-skill')).toBeEnabled());
    await user.click(screen.getByRole('combobox', { name: 'Version' }));
    expect(screen.getByRole('option', { name: 'v1.1 (current)' })).toHaveAttribute('aria-selected', 'true');
    expect(screen.getByRole('option', { name: 'v1.0 (yanked)' })).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('Try on a skill listing (skills epic 2169)', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('starts a conversation and pins the listing version by reference, forking nothing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const calls: { url: string; body: unknown }[] = [];
    mockListing(
      detailOf({
        listing: { ...LISTING, id: '7', kind: 'skill', entity_id: '41', name: 'PR Reviewer' },
        versions: [version('6', 'v1.1', 'current', { requirements: NO_REQUIREMENTS })],
      }),
      ['models.applications.fork.post'],
    );
    server.use(
      http.post('/api/v2/elitea_core/conversations/prompt_lib/:projectId', async ({ request }) => {
        calls.push({ url: new URL(request.url).pathname, body: await request.json() });
        return HttpResponse.json({ id: 55 }, { status: 201 });
      }),
      http.post('/api/v2/elitea_core/conversation_skills/prompt_lib/:projectId/:conversationId', async ({ request }) => {
        calls.push({ url: new URL(request.url).pathname, body: await request.json() });
        return HttpResponse.json({}, { status: 201 });
      }),
      http.post('/api/v2/elitea_core/attach_public_skill/prompt_lib/:projectId', () => {
        calls.push({ url: 'attach', body: null });
        return HttpResponse.json({});
      }),
      http.get('/api/v2/elitea_core/public_skill/prompt_lib/41', () =>
        HttpResponse.json({ id: 41, name: 'PR Reviewer', description: 'd', versions: [], version_details: null }),
      ),
    );
    mount('/marketplace/skills/7');

    await userEvent.setup().click(await screen.findByTestId('marketplace-try-skill'));
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[0]?.body).toMatchObject({ participants: [], meta: { origin: 'skill_try' } });
    expect(calls[1]?.url).toMatch(/conversation_skills\/prompt_lib\/[^/]+\/55$/);
    expect(calls[1]?.body).toEqual({ listing_id: 7, listing_version_id: 6 });
    expect(calls.some((call) => call.url === 'attach')).toBe(false);
  });
});

describe('Try on a skill listing that was yanked meanwhile', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('removes the empty conversation when the pin is refused, instead of leaving it behind', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const deleted: string[] = [];
    mockListing(
      detailOf({
        listing: { ...LISTING, id: '7', kind: 'skill', entity_id: '41', name: 'PR Reviewer' },
        versions: [version('6', 'v1.1', 'current', { requirements: NO_REQUIREMENTS })],
      }),
      ['models.applications.fork.post'],
    );
    server.use(
      http.post('/api/v2/elitea_core/conversations/prompt_lib/:projectId', () => HttpResponse.json({ id: 56 }, { status: 201 })),
      http.post('/api/v2/elitea_core/conversation_skills/prompt_lib/:projectId/:conversationId', () =>
        HttpResponse.json({ error: 'gone' }, { status: 404 }),
      ),
      http.delete('/api/v2/elitea_core/conversation/prompt_lib/:projectId/:conversationId', ({ params }) => {
        deleted.push(String(params['conversationId']));
        return new HttpResponse(null, { status: 204 });
      }),
      http.get('/api/v2/elitea_core/public_skill/prompt_lib/41', () =>
        HttpResponse.json({ id: 41, name: 'PR Reviewer', description: 'd', versions: [], version_details: null }),
      ),
    );
    mount('/marketplace/skills/7');

    await userEvent.setup().click(await screen.findByTestId('marketplace-try-skill'));
    await waitFor(() => expect(deleted).toEqual(['56']));
    expect(await screen.findByText('The chat could not be started. Try again.')).toBeInTheDocument();
  });
});

describe('listing load errors', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows an error with Retry for a 500, and the not-found page only for a 404', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing();
    let calls = 0;
    server.use(
      http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', () => {
        calls += 1;
        return calls === 1 ? HttpResponse.json({ error: 'boom' }, { status: 500 }) : HttpResponse.json(detailOf());
      }),
    );
    mount('/marketplace/agents/7');
    const user = userEvent.setup();

    expect(await screen.findByTestId('marketplace-listing-error')).toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-listing-not-found')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('marketplace-listing-retry'));
    expect(await screen.findByTestId('marketplace-listing-name')).toHaveTextContent('Release Notes Writer');
  });
});

describe('moderator restore and double submit', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('lets an administrator restore a yanked version, but not a deprecated one', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ can_moderate: true }));
    mount('/marketplace/agents/7');

    expect(await screen.findByTestId('marketplace-version-available-250')).toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-version-available-300')).not.toBeInTheDocument();
  });

  it('sends one request however often Deprecate is confirmed while it is in flight', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ can_manage: true }));
    server.use(
      http.post(
        '/api/v2/elitea_core/marketplace_listing_version/prompt_lib/:projectId/:listingId/:versionId',
        async ({ request, params }) => {
          stateBodies.push({ version: params['versionId'], ...((await request.json()) as object) });
          await new Promise((resolve) => setTimeout(resolve, 150));
          return HttpResponse.json(version(String(params['versionId']), 'v', 'deprecated'));
        },
      ),
    );
    mount('/marketplace/agents/7');
    const user = userEvent.setup();

    await user.click(await screen.findByTestId('marketplace-version-deprecated-311'));
    const confirm = within(await screen.findByTestId('marketplace-version-confirm')).getByRole('button', {
      name: 'Deprecate',
    });
    await user.click(confirm);
    // The button is off while the request is in flight, so a second click cannot send another.
    await waitFor(() => expect(confirm).toBeDisabled());
    await waitFor(() => expect(stateBodies).toHaveLength(1));
  });
});

describe('the hire wizard state', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  async function openAt(
    detail: MarketplaceListingDetail,
  ): Promise<{ user: ReturnType<typeof userEvent.setup>; wizard: HTMLElement }> {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detail);
    mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeEnabled());
    await user.click(screen.getByTestId('marketplace-hire'));
    return { user, wizard: await screen.findByTestId('hire-wizard') };
  }

  it('keeps the result after a hire, though the listing is read again', async () => {
    const { user, wizard } = await openAt(detailOf());
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'Disable for now' }));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-confirm'));

    expect(await within(wizard).findByTestId('hire-done')).toBeInTheDocument();
    // The success invalidates the listing; a new `versions` array must not restart the wizard.
    await new Promise((resolve) => setTimeout(resolve, 200));
    expect(within(wizard).getByTestId('hire-done')).toBeInTheDocument();
  });

  it('forgets the bindings when another version is picked, and a refusal when the person goes back', async () => {
    const other = version('300', 'v2.0', 'available', { requirements: REQUIREMENTS });
    const { user, wizard } = await openAt(detailOf({ versions: [version('311', 'v2.3', 'current'), other] }));
    server.use(
      http.post('/api/v2/elitea_core/marketplace_hire/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'binding_missing' }, { status: 409 }),
      ),
    );

    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'GitHub (release bot)' }));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-confirm'));
    expect(await within(wizard).findByTestId('hire-error')).toBeInTheDocument();

    // Back to the version step: the refusal is gone, and a new version starts with no binding.
    await user.click(within(wizard).getByTestId('hire-back'));
    expect(within(wizard).queryByTestId('hire-error')).not.toBeInTheDocument();
    await user.click(within(wizard).getByTestId('hire-back'));
    await user.click(within(wizard).getByTestId('hire-back'));
    await user.click(within(wizard).getByRole('radio', { name: 'v2.0 (available)' }));
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-next'));
    expect(within(wizard).getByTestId('hire-next')).toBeDisabled();
  });

  it('treats a nested agent as information, binds a skill only to a project skill, and never offers an agent-as-tool', async () => {
    const needs = {
      items: [
        { key: 'agent:helper', kind: 'agent', type: 'helper', name: 'helper', tools: [], has_write_tools: false },
        { key: 'skill:triage', kind: 'skill', type: 'triage', name: 'triage', tools: [], has_write_tools: false },
        { key: 'toolkit:jira', kind: 'toolkit', type: 'jira', name: 'jira', tools: [], has_write_tools: false },
      ],
      model_class: 'chat',
      tier: 'low',
      has_write_tools: false,
    };
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockListing(detailOf({ versions: [version('311', 'v2.3', 'current', { requirements: needs })] }));
    let agentListReads = 0;
    server.use(
      http.get('/api/v2/elitea_core/applications/prompt_lib/:projectId', () => {
        agentListReads += 1;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
        HttpResponse.json({
          rows: [
            { id: '13', type: 'jira', name: 'Jira' },
            { id: '14', type: 'application', name: 'Sub agent' },
          ],
          total: 2,
        }),
      ),
      http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () =>
        HttpResponse.json({ rows: [{ id: 4, name: 'Triage skill' }], total: 1 }),
      ),
    );
    mount('/marketplace/agents/7');
    await screen.findByTestId('marketplace-listing-name');
    const user = userEvent.setup();
    await waitFor(() => expect(screen.getByTestId('marketplace-hire')).toBeEnabled());
    await user.click(screen.getByTestId('marketplace-hire'));
    const wizard = await screen.findByTestId('hire-wizard');
    await user.click(within(wizard).getByTestId('hire-next'));
    await user.click(within(wizard).getByTestId('hire-next'));

    expect(within(wizard).getByTestId('hire-agent-agent:helper')).toHaveTextContent('comes with this agent');
    // The skill is optional: Next stays off only for the Jira toolkit.
    expect(within(wizard).getByTestId('hire-next')).toBeDisabled();
    await user.click(within(wizard).getByRole('combobox', { name: 'jira for jira' }));
    expect(screen.getByRole('option', { name: 'Jira' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Sub agent' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('option', { name: 'Jira' }));
    expect(within(wizard).getByTestId('hire-next')).toBeEnabled();
    await user.click(within(wizard).getByRole('combobox', { name: 'triage for triage' }));
    expect(screen.getByRole('option', { name: 'Triage skill' })).toBeInTheDocument();
    expect(agentListReads).toBe(0);
  });
});
