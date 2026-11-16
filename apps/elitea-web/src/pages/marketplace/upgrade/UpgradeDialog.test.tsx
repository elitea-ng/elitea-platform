import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type { MarketplaceUpgradePlan, MarketplaceVersion } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { UpgradeDialog } from './UpgradeDialog';

const NO_REQUIREMENTS = { items: [], model_class: 'chat', tier: 'low', has_write_tools: false };

function version(id: string, name: string, state = 'available'): MarketplaceVersion {
  return {
    id,
    name,
    state,
    release_notes: `Notes for ${name}.`,
    digest: 'abcdef0123456789abcdef',
    published_at: '2026-10-30T09:00:00Z',
    requirements: NO_REQUIREMENTS,
  };
}

function plan(overrides: Partial<MarketplaceUpgradePlan> = {}): MarketplaceUpgradePlan {
  return {
    current_version: {
      id: '1',
      name: 'v1',
      state: 'available',
      state_reason: '',
      release_notes: '',
      published_at: '2026-10-01T00:00:00Z',
    },
    target_version: {
      id: '3',
      name: 'v3',
      state: 'current',
      state_reason: '',
      release_notes: 'Shorter answers.',
      published_at: '2026-11-01T00:00:00Z',
    },
    direction: 'up',
    changes: [{ path: '/definition/version/instructions', op: 'replace', before: 'a', after: 'be brief' }],
    widening: [],
    requirements: NO_REQUIREMENTS,
    bindings: [],
    new_requirements: [],
    needs_approval: false,
    auto_eligible: true,
    hireable: true,
    feedback: { rating_avg: 4.5, rating_count: 12 },
    ...overrides,
  };
}

let posts: unknown[] = [];
let planUrls: string[] = [];

function mock(
  planned: MarketplaceUpgradePlan,
  post?: () => Response,
  listed: MarketplaceVersion[] = [version('3', 'v3', 'current'), version('2', 'v2'), version('1', 'v1')],
): void {
  posts = [];
  planUrls = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/marketplace_listing/prompt_lib/:projectId/:listingId', () =>
      HttpResponse.json({
        listing: { id: '7', kind: 'agent', name: 'Writer' },
        versions: listed,
      }),
    ),
    http.get('/api/v2/elitea_core/marketplace_upgrade/prompt_lib/:projectId/:applicationId', ({ request }) => {
      planUrls.push(new URL(request.url).search);
      return HttpResponse.json(planned);
    }),
    http.post('/api/v2/elitea_core/marketplace_upgrade/prompt_lib/:projectId/:applicationId', async ({ request }) => {
      posts.push(await request.json());
      return (
        post?.() ??
        HttpResponse.json({
          application_id: '88',
          version_id: '3',
          applied: !planned.needs_approval,
          awaiting_approval: planned.needs_approval,
        })
      );
    }),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
      HttpResponse.json({ rows: [{ id: '12', type: 'jira', name: 'Team Jira' }], total: 1 }),
    ),
    http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <UpgradeDialog
        open
        onClose={() => undefined}
        projectId="5"
        applicationId={88}
        listingId="7"
        pinnedVersionId="1"
        initialVersionId="3"
      />
    </QueryClientProvider>,
  );
}

describe('UpgradeDialog', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('says so when the listing has no other version, with the way on', async () => {
    mock(plan(), undefined, [version('1', 'v1')]);
    const dialog = await screen.findByTestId('upgrade-dialog');
    expect(await within(dialog).findByTestId('upgrade-no-versions')).toHaveTextContent('no other version');
    expect(within(dialog).getByRole('button', { name: 'Upgrade' })).toBeDisabled();
  });

  it('shows the diff, the release notes and the rating, and upgrades in one click at low tier', async () => {
    mock(plan());
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('upgrade-dialog');
    const body = await within(dialog).findByTestId('upgrade-plan');
    expect(body).toHaveTextContent('Upgrade to v3');
    expect(body).toHaveTextContent('Shorter answers.');
    expect(body).toHaveTextContent('Rated 4.5 out of 5 by 12 people.');
    expect(body).toHaveTextContent('replace /definition/version/instructions: be brief');
    expect(within(dialog).getByTestId('upgrade-approval-note')).toHaveTextContent('needs no approval');
    // The pinned version is not offered, and the plan asked for the chosen one.
    expect(within(dialog).queryByTestId('hire-version-1')).toBeNull();
    expect(planUrls[0]).toBe('?version_id=3');

    await user.click(within(dialog).getByRole('button', { name: 'Upgrade' }));
    await waitFor(() => expect(posts).toEqual([{ version_id: '3', bindings: [] }]));
    expect(await within(dialog).findByTestId('upgrade-result')).toHaveTextContent('The agent was upgraded.');
  });

  it('warns about what a wider version adds and sends it for approval', async () => {
    mock(
      plan({
        needs_approval: true,
        auto_eligible: false,
        widening: [{ kind: 'tool_added', detail: 'github: create_issue' }],
      }),
    );
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('upgrade-dialog');
    expect(await within(dialog).findByTestId('upgrade-widening')).toHaveTextContent('A new tool: github: create_issue');
    expect(within(dialog).getByTestId('upgrade-approval-note')).toHaveTextContent('needs a person to approve');

    await user.click(within(dialog).getByRole('button', { name: 'Request upgrade' }));
    expect(await within(dialog).findByTestId('upgrade-result')).toHaveTextContent('Sent for approval.');
  });

  it('says a downgrade is a downgrade, and shows the server refusal in words', async () => {
    mock(plan({ direction: 'down', target_version: { ...plan().target_version, name: 'v0' } }), () =>
      HttpResponse.json({ error: 'version_not_hireable' }, { status: 409 }),
    );
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('upgrade-dialog');
    expect(await within(dialog).findByTestId('upgrade-plan')).toHaveTextContent('Go back to v0');
    await user.click(within(dialog).getByRole('button', { name: 'Upgrade' }));
    expect(await within(dialog).findByTestId('upgrade-error')).toHaveTextContent('cannot be used now');
  });

  it('cannot confirm while a requirement the target adds is unbound', async () => {
    const needs = {
      ...NO_REQUIREMENTS,
      items: [
        {
          key: 'toolkit:jira',
          kind: 'toolkit',
          type: 'jira',
          name: 'Jira',
          tools: ['get_issue'],
          has_write_tools: false,
        },
      ],
    };
    mock(plan({ requirements: needs, new_requirements: needs.items }));
    const dialog = await screen.findByTestId('upgrade-dialog');
    await within(dialog).findByTestId('upgrade-plan');
    expect(within(dialog).getByRole('button', { name: 'Upgrade' })).toBeDisabled();
    expect(await within(dialog).findByRole('combobox', { name: 'jira for Jira' })).toBeInTheDocument();
  });
});
