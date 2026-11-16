import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type {
  MarketplaceFeedback,
  MarketplaceFeedbackItem,
  MarketplaceListingDetail,
  MarketplaceListingSummary,
} from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { ListingFeedback } from './ListingFeedback';

const LISTING = {
  id: '7',
  kind: 'agent',
  entity_id: '70',
  name: 'Flake Finder',
  description: '',
  category: '',
  tags: [],
  tier: 'low',
  hires: 12,
  rating_avg: 3.5,
  rating_count: 2,
  likes: 4,
  is_liked: false,
  featured: false,
  required_types: [],
  has_write_tools: false,
  state: 'listed',
  hired: [],
} as unknown as MarketplaceListingSummary;

const DETAIL = {
  listing: LISTING,
  welcome_message: '',
  conversation_starters: [],
  soul_summary: '',
  versions: [],
  can_manage: false,
  can_moderate: false,
} as MarketplaceListingDetail;

const SIGNALS = {
  day: '2026-11-15',
  active_hires: 12,
  hires_total: 15,
  dismissed: 3,
  dismissal_rate: 0.2,
  retention_cohort: 10,
  retention_30d: 0.8,
  upgrade_eligible: 5,
  upgrade_adoption: 0.6,
  live: false,
};

function item(overrides: Partial<MarketplaceFeedbackItem>): MarketplaceFeedbackItem {
  return {
    id: '1',
    kind: 'review',
    version_id: '311',
    version_name: 'v2',
    rating: 4,
    body: 'Works well.',
    author: { kind: 'person', name: 'Reviewer Eleven' },
    state: 'visible',
    flagged: false,
    reply: null,
    mine: false,
    created_at: '2026-11-10T09:00:00Z',
    updated_at: '2026-11-10T09:00:00Z',
    ...overrides,
  };
}

const REVIEW = item({});
const REPORT = item({
  id: '2',
  kind: 'workforce_report',
  version_id: '300',
  version_name: 'v1',
  rating: null,
  body: 'It kept failing on large repositories.',
  outcome: 'dismissed',
  stage: 'dismissal',
  reason_code: 'too_many_failures',
  scorecard: { failure_rate: 0.9, active_days: 14 },
  author: { kind: 'agent', name: 'Workforce report from a hiring project' },
});

function feedbackOf(overrides: Partial<MarketplaceFeedback> = {}): MarketplaceFeedback {
  return {
    items: [REVIEW, REPORT],
    total: 2,
    versions: [
      {
        version_id: '311',
        version_name: 'v2',
        rating_avg: 4,
        rating_count: 1,
        distribution: [0, 0, 0, 1, 0],
        report_count: 0,
      },
      {
        version_id: '300',
        version_name: 'v1',
        rating_avg: 3,
        rating_count: 1,
        distribution: [0, 0, 1, 0, 0],
        report_count: 1,
      },
    ],
    overall: {
      version_id: '0',
      version_name: '',
      rating_avg: 3.5,
      rating_count: 2,
      distribution: [0, 0, 1, 1, 0],
      report_count: 1,
    },
    signals: {
      listing: SIGNALS,
      versions: { '311': { ...SIGNALS, active_hires: 4, dismissal_rate: null } },
    },
    review_versions: [],
    can_review: false,
    my_review: null,
    can_reply: false,
    can_flag: false,
    can_moderate: false,
    ...overrides,
  };
}

let queries: string[] = [];
let posts: { url: string; body: unknown }[] = [];

function mockFeedback(feedback: MarketplaceFeedback): void {
  queries = [];
  posts = [];
  server.use(
    http.get('/api/v2/elitea_core/marketplace_feedback/prompt_lib/:projectId/:listingId', ({ request }) => {
      queries.push(new URL(request.url).search);
      const url = new URL(request.url);
      const version = url.searchParams.get('version_id');
      const kind = url.searchParams.get('kind');
      const items = feedback.items.filter(
        (entry) => (version === null || entry.version_id === version) && (kind === null || entry.kind === kind),
      );
      return HttpResponse.json({ ...feedback, items, total: items.length });
    }),
    http.post('/api/v2/elitea_core/marketplace_feedback/prompt_lib/:projectId/:listingId', async ({ request }) => {
      posts.push({ url: request.url, body: await request.json() });
      return HttpResponse.json({ id: '9', created: true }, { status: 201 });
    }),
    http.post(
      '/api/v2/elitea_core/marketplace_feedback_action/prompt_lib/:projectId/:listingId/:feedbackId',
      async ({ request, params }) => {
        posts.push({ url: request.url, body: { id: params['feedbackId'], ...((await request.json()) as object) } });
        return HttpResponse.json({ id: params['feedbackId'], action: 'x' });
      },
    ),
  );
}

function mount(detail: MarketplaceListingDetail = DETAIL): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <ListingFeedback detail={detail} projectId="5" />
    </QueryClientProvider>,
  );
}

describe('ListingFeedback', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('shows the rating as text with the distribution and the signals beside it', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf());
    mount();

    expect(await screen.findByTestId('marketplace-feedback-rating')).toHaveTextContent(
      'Rated 3.5 out of 5 by 2 reviews',
    );
    expect(screen.getByTestId('marketplace-feedback-stars-4')).toHaveTextContent('4 stars');
    expect(screen.getByTestId('marketplace-feedback-stars-4')).toHaveTextContent('1');
    const signals = screen.getByTestId('marketplace-feedback-signals');
    expect(within(signals).getByText('Active hires').nextElementSibling).toHaveTextContent('12');
    expect(within(signals).getByText('30-day retention').nextElementSibling).toHaveTextContent('80%');
    expect(within(signals).getByText('Dismissal rate').nextElementSibling).toHaveTextContent('20%');
    expect(within(signals).getByText('Upgrade adoption').nextElementSibling).toHaveTextContent('60%');
  });

  it('labels each item by kind and names no project: a review has its author, a report reads "from a hiring project"', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf());
    mount();

    const review = await screen.findByTestId('marketplace-feedback-item-1');
    expect(within(review).getByText('Review by Reviewer Eleven')).toBeInTheDocument();
    expect(within(review).getByText('Rated 4 out of 5')).toBeInTheDocument();
    expect(within(review).getByText('on v2')).toBeInTheDocument();
    const report = screen.getByTestId('marketplace-feedback-item-2');
    expect(within(report).getByText('Workforce report from a hiring project')).toBeInTheDocument();
    expect(within(report).getByText('Dismissed')).toBeInTheDocument();
    expect(within(report).getByText('on v1')).toBeInTheDocument();
    expect(within(report).getByText(/Failure rate 90%/)).toBeInTheDocument();
    expect(within(report).getByText('It kept failing on large repositories.')).toBeInTheDocument();
  });

  it('filters by version: the rating, the distribution and the signals follow, and the request carries the version', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf());
    mount();
    const user = userEvent.setup();

    await screen.findByTestId('marketplace-feedback-rating');
    await user.click(screen.getByRole('combobox', { name: 'Feedback for' }));
    await user.click(screen.getByRole('option', { name: 'v1' }));

    await waitFor(() =>
      expect(screen.getByTestId('marketplace-feedback-rating')).toHaveTextContent('Rated 3.0 out of 5 by 1 reviews'),
    );
    expect(queries.some((search) => search.includes('version_id=300'))).toBe(true);
    expect(screen.queryByTestId('marketplace-feedback-item-1')).not.toBeInTheDocument();
    expect(screen.getByTestId('marketplace-feedback-item-2')).toBeInTheDocument();
  });

  it('finds a workforce report from the kind filter (T7)', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf());
    mount();
    const user = userEvent.setup();

    await screen.findByTestId('marketplace-feedback-list');
    await user.click(screen.getByTestId('marketplace-feedback-kind-report'));
    await waitFor(() => expect(screen.queryByTestId('marketplace-feedback-item-1')).not.toBeInTheDocument());
    expect(screen.getByTestId('marketplace-feedback-item-2')).toBeInTheDocument();
    expect(queries.some((search) => search.includes('kind=workforce_report'))).toBe(true);
  });

  it('offers the review form only to a verified hire, and posts the rating, the text and the version', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf({ review_versions: [{ id: '311', name: 'v2' }], can_review: true }));
    mount();
    const user = userEvent.setup();

    await screen.findByTestId('marketplace-review-form');
    expect(screen.getByTestId('marketplace-review-save')).toBeDisabled();
    await user.click(screen.getByRole('radio', { name: '5 out of 5' }));
    await user.type(screen.getByRole('textbox', { name: /What worked/ }), 'Great.');
    await user.click(screen.getByTestId('marketplace-review-save'));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ rating: 5, body: 'Great.', version_id: '311' });
    expect(await screen.findByText('Your review is saved.')).toBeInTheDocument();
  });

  it('does not offer the review form to a project that did not hire the listing', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(feedbackOf());
    mount();

    await screen.findByTestId('marketplace-feedback-list');
    expect(screen.queryByTestId('marketplace-review-form')).not.toBeInTheDocument();
  });

  it('opens a saved review for editing, with its rating selected', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    const mine = item({ id: '8', mine: true, rating: 2, body: 'Slow.' });
    mockFeedback(
      feedbackOf({
        items: [mine],
        review_versions: [{ id: '311', name: 'v2' }],
        can_review: true,
        my_review: mine,
      }),
    );
    mount();

    await screen.findByTestId('marketplace-review-form');
    expect(screen.getByRole('radio', { name: '2 out of 5' })).toBeChecked();
    expect(screen.getByRole('textbox', { name: /What worked/ })).toHaveValue('Slow.');
    expect(screen.getByTestId('marketplace-review-save')).toHaveTextContent('Update review');
    expect(screen.getByText('Yours')).toBeInTheDocument();
  });

  it('gives the publisher Reply once and Flag, and sends the reply text', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(
      feedbackOf({
        items: [REVIEW, item({ id: '3', reply: { body: 'Fixed in v2.', created_at: '2026-11-11T09:00:00Z' } })],
        can_reply: true,
        can_flag: true,
      }),
    );
    mount();
    const user = userEvent.setup();

    await screen.findByTestId('marketplace-feedback-list');
    // The answered item has no Reply button and shows the reply as text.
    expect(screen.queryByTestId('marketplace-feedback-reply-open-3')).not.toBeInTheDocument();
    expect(screen.getByTestId('marketplace-feedback-reply-3')).toHaveTextContent('Fixed in v2.');
    await user.click(screen.getByTestId('marketplace-feedback-reply-open-1'));
    expect(screen.getByTestId('marketplace-feedback-submit-reply')).toBeDisabled();
    await user.type(screen.getByRole('textbox', { name: 'Your reply' }), 'Thanks.');
    await user.click(screen.getByTestId('marketplace-feedback-submit-reply'));

    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ id: '1', action: 'reply', text: 'Thanks.' });
  });

  it('gives an administrator Hide with a reason, and Show again on a hidden item', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(
      feedbackOf({
        items: [REVIEW, item({ id: '4', state: 'hidden', hide_reason: 'abuse' })],
        can_flag: true,
        can_moderate: true,
      }),
    );
    mount();
    const user = userEvent.setup();

    await screen.findByTestId('marketplace-feedback-list');
    expect(screen.getByText('Hidden by an administrator')).toBeInTheDocument();
    expect(screen.getByText('Hidden because: abuse')).toBeInTheDocument();
    expect(screen.queryByTestId('marketplace-feedback-hide-4')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('marketplace-feedback-hide-1'));
    await user.type(screen.getByRole('textbox', { name: 'Why hide it? (optional)' }), 'personal abuse');
    await user.click(screen.getByTestId('marketplace-feedback-submit-hide'));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]?.body).toEqual({ id: '1', action: 'hide', reason: 'personal abuse' });

    await user.click(screen.getByTestId('marketplace-feedback-unhide-4'));
    await waitFor(() => expect(posts).toHaveLength(2));
    expect(posts[1]?.body).toEqual({ id: '4', action: 'unhide' });
  });

  it('says what is missing when there is no feedback, with the way to get some', async () => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    mockFeedback(
      feedbackOf({
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
      }),
    );
    mount();

    expect(await screen.findByTestId('marketplace-feedback-rating')).toHaveTextContent('No reviews yet');
    expect(screen.getByTestId('marketplace-feedback-empty')).toHaveTextContent(
      'No feedback yet. Reviews come from people whose project hired this listing.',
    );
  });
});
