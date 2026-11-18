import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type { MarketplaceUpstreamReview } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { UpstreamReviewDialog } from './UpstreamReviewDialog';

function review(overrides: Partial<MarketplaceUpstreamReview> = {}): MarketplaceUpstreamReview {
  return {
    mode: 'three_way',
    listing_id: '7',
    listing_name: 'Writer',
    base: {
      id: '1',
      name: 'v1',
      state: 'available',
      state_reason: '',
      release_notes: '',
      published_at: '2026-10-01T00:00:00Z',
    },
    theirs: {
      id: '2',
      name: 'v2',
      state: 'current',
      state_reason: '',
      release_notes: 'New wording.',
      published_at: '2026-11-01T00:00:00Z',
    },
    fields: [
      {
        key: 'instructions',
        label: 'Instructions',
        status: 'conflict',
        default: 'ours',
        base: 'base text',
        theirs: 'their text',
        ours: 'our text',
      },
      {
        key: 'welcome_message',
        label: 'Welcome message',
        status: 'theirs_only',
        default: 'theirs',
        base: 'hi',
        theirs: 'hello',
        ours: 'hi',
      },
      {
        key: 'agent_type',
        label: 'Agent type',
        status: 'same',
        default: 'ours',
        base: 'agent',
        theirs: 'agent',
        ours: 'agent',
      },
    ],
    changes: [{ path: '/definition/version/instructions', op: 'replace', before: 'base text', after: 'their text' }],
    widening: [],
    needs_approval: true,
    hireable: true,
    our_version_name: 'draft',
    open_change_request: false,
    ...overrides,
  };
}

let posts: unknown[] = [];

function mock(served: MarketplaceUpstreamReview): void {
  posts = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/marketplace_upstream/prompt_lib/:projectId/:applicationId', () =>
      HttpResponse.json(served),
    ),
    http.post('/api/v2/elitea_core/marketplace_upstream/prompt_lib/:projectId/:applicationId', async ({ request }) => {
      posts.push(await request.json());
      return HttpResponse.json(
        { application_id: '88', draft_version_id: '900', version_id: '2', applied: false, awaiting_approval: true },
        { status: 201 },
      );
    }),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <UpstreamReviewDialog open onClose={() => undefined} projectId="5" applicationId={88} />
    </QueryClientProvider>,
  );
}

describe('UpstreamReviewDialog', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('lists the conflict first with all three sides and cannot merge until it is answered', async () => {
    mock(review());
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('upstream-dialog');
    const rows = await within(dialog).findAllByTestId(/^upstream-field-/);
    expect(rows[0]).toHaveAttribute('data-testid', 'upstream-field-instructions');
    expect(rows[0]).toHaveTextContent('Conflict: both sides changed it');
    expect(within(dialog).getByTestId('upstream-base-instructions')).toHaveTextContent('base text');
    expect(within(dialog).getByTestId('upstream-theirs-instructions')).toHaveTextContent('their text');
    expect(within(dialog).getByTestId('upstream-ours-instructions')).toHaveTextContent('our text');
    // A field that is the same on both sides is not a row, and is named.
    expect(within(dialog).queryByTestId('upstream-field-agent_type')).toBeNull();
    expect(dialog).toHaveTextContent('Same on both sides: Agent type.');
    expect(within(dialog).getByRole('button', { name: 'Propose the merge' })).toBeDisabled();

    await user.click(within(dialog).getByTestId('upstream-keep-theirs-instructions'));
    expect(within(dialog).getByRole('button', { name: 'Propose the merge' })).toBeEnabled();
    await user.click(within(dialog).getByRole('button', { name: 'Propose the merge' }));
    await waitFor(() =>
      expect(posts).toEqual([{ version_id: '2', resolutions: { instructions: { choice: 'theirs' } } }]),
    );
    expect(await within(dialog).findByTestId('upstream-result')).toHaveTextContent('sent for approval');
  });

  it('merges an edited field', async () => {
    mock(review());
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('upstream-dialog');
    await user.click(await within(dialog).findByTestId('upstream-edit-instructions'));
    const box = within(dialog).getByRole('textbox', { name: 'Edited Instructions' });
    await user.clear(box);
    await user.type(box, 'merged text');
    await user.click(within(dialog).getByRole('button', { name: 'Propose the merge' }));
    await waitFor(() =>
      expect(posts).toEqual([
        { version_id: '2', resolutions: { instructions: { choice: 'edit', value: 'merged text' } } },
      ]),
    );
  });

  it('says a two-way review has no base', async () => {
    mock(
      review({
        mode: 'two_way',
        base: null,
        changes: [],
        fields: [
          {
            key: 'instructions',
            label: 'Instructions',
            status: 'differs',
            default: 'ours',
            base: null,
            theirs: 'their text',
            ours: 'our text',
          },
        ],
      }),
    );
    const dialog = await screen.findByTestId('upstream-dialog');
    expect(await within(dialog).findByTestId('upstream-review')).toHaveTextContent(
      'does not know which version it came from',
    );
    expect(within(dialog).queryByTestId('upstream-base-instructions')).toBeNull();
    // A two-way difference has a default, so the merge can go ahead without a choice.
    expect(within(dialog).getByRole('button', { name: 'Propose the merge' })).toBeEnabled();
  });

  it('cannot merge while the fork has an open change request', async () => {
    mock(review({ open_change_request: true }));
    const dialog = await screen.findByTestId('upstream-dialog');
    await within(dialog).findByTestId('upstream-review');
    expect(within(dialog).getByRole('button', { name: 'Propose the merge' })).toBeDisabled();
    expect(dialog).toHaveTextContent('already has an open change request');
  });
});
