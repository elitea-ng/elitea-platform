/**
 * `MessageFeedbackControl` — the thumbs up/down + optional comment on an
 * assistant message (#880).
 *
 * MSW-backed: every assertion goes through the real
 * `/elitea_core/message_feedback/prompt_lib/{projectId}/{messageId}` route
 * shape (`getGetMessageFeedbackUrl` et al.), not a mocked hook, so a caller
 * that built the wrong URL or body would fail here.
 *
 * Harness matches `UserMessage.attachments.test.tsx`'s own recipe
 * (`configureGeneratedClient` + MSW `server.use`).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../../test/setup';

import { MessageFeedbackControl } from './MessageFeedbackControl';

const BASE = '/api/v2';
const FEEDBACK_URL = `${BASE}/elitea_core/message_feedback/prompt_lib/p1/msg-1`;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

interface CapturedRequest {
  readonly method: string;
  readonly body: unknown;
}

type FeedbackSummary = {
  readonly likes: number;
  readonly dislikes: number;
  readonly mine?: { readonly rating: 1 | -1; readonly comment?: string };
};

/**
 * A STATEFUL mock, not a static fixture: react-query's default
 * `refetchOnMount`/`refetchOnWindowFocus` can refetch the GET at any point
 * (jsdom's own focus handling during `userEvent` interactions included), and
 * a static fixture would silently overwrite a just-applied vote with the
 * pre-vote state the moment that happens — passing today and flaking the
 * next time a react-query default changes. This mirrors what the REAL
 * server does: POST/DELETE mutate one row, and every GET after answers the
 * current aggregate.
 */
function renderControl(initial: FeedbackSummary, sink: CapturedRequest[]): void {
  let current: FeedbackSummary = initial;
  server.use(
    http.get(FEEDBACK_URL, () => HttpResponse.json(current)),
    http.post(FEEDBACK_URL, async ({ request }) => {
      const body = (await request.json()) as { rating: 1 | -1; comment?: string };
      sink.push({ method: 'POST', body });
      const wasMine = current.mine?.rating;
      const likes = current.likes - (wasMine === 1 ? 1 : 0) + (body.rating === 1 ? 1 : 0);
      const dislikes = current.dislikes - (wasMine === -1 ? 1 : 0) + (body.rating === -1 ? 1 : 0);
      current = { likes, dislikes, mine: { rating: body.rating, ...(body.comment ? { comment: body.comment } : {}) } };
      return HttpResponse.json(current);
    }),
    http.delete(FEEDBACK_URL, () => {
      sink.push({ method: 'DELETE', body: undefined });
      const wasMine = current.mine?.rating;
      current = {
        likes: current.likes - (wasMine === 1 ? 1 : 0),
        dislikes: current.dislikes - (wasMine === -1 ? 1 : 0),
      };
      return HttpResponse.json(current);
    }),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <MessageFeedbackControl projectId="p1" messageId="msg-1" />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('MessageFeedbackControl', () => {
  it('renders the thumbs, unpressed, when nobody has voted yet', async () => {
    renderControl({ likes: 0, dislikes: 0 }, []);

    const like = await screen.findByRole('button', { name: 'Like this answer' });
    const dislike = screen.getByRole('button', { name: 'Dislike this answer' });
    expect(like).toHaveAttribute('aria-pressed', 'false');
    expect(dislike).toHaveAttribute('aria-pressed', 'false');
    // No vote yet — the comment trigger has nothing to attach to.
    expect(screen.queryByLabelText('Add a comment')).not.toBeInTheDocument();
  });

  it('clicking like POSTs a rating of 1 and highlights the button', async () => {
    const sink: CapturedRequest[] = [];
    renderControl({ likes: 0, dislikes: 0 }, sink);
    const user = userEvent.setup();

    await user.click(await screen.findByRole('button', { name: 'Like this answer' }));

    await waitFor(() => expect(sink).toHaveLength(1));
    expect(sink[0]).toEqual({ method: 'POST', body: { rating: 1 } });
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Like this answer' })).toHaveAttribute('aria-pressed', 'true'),
    );
  });

  it('clicking the already-selected thumb retracts the vote (DELETE)', async () => {
    const sink: CapturedRequest[] = [];
    renderControl({ likes: 1, dislikes: 0, mine: { rating: 1 } }, sink);
    const user = userEvent.setup();

    const like = await screen.findByRole('button', { name: 'Like this answer' });
    await waitFor(() => expect(like).toHaveAttribute('aria-pressed', 'true'));

    await user.click(like);

    await waitFor(() => expect(sink).toHaveLength(1));
    expect(sink[0]).toEqual({ method: 'DELETE', body: undefined });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Like this answer' })).toHaveAttribute('aria-pressed', 'false'));
  });

  it('switching from dislike to like replaces the vote in one POST (upsert)', async () => {
    const sink: CapturedRequest[] = [];
    renderControl({ likes: 0, dislikes: 1, mine: { rating: -1 } }, sink);
    const user = userEvent.setup();

    const dislike = await screen.findByRole('button', { name: 'Dislike this answer' });
    await waitFor(() => expect(dislike).toHaveAttribute('aria-pressed', 'true'));

    await user.click(await screen.findByRole('button', { name: 'Like this answer' }));

    await waitFor(() => expect(sink).toHaveLength(1));
    expect(sink[0]).toEqual({ method: 'POST', body: { rating: 1 } });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Like this answer' })).toHaveAttribute('aria-pressed', 'true'));
    expect(screen.getByRole('button', { name: 'Dislike this answer' })).toHaveAttribute('aria-pressed', 'false');
  });

  it('shows the aggregate counts as the hover tooltip title', async () => {
    renderControl({ likes: 3, dislikes: 1 }, []);

    const like = await screen.findByRole('button', { name: 'Like this answer' });
    // MUI Tooltip puts its text in the wrapped element's `title`-equivalent
    // accessible description once open; simplest stable assertion is the
    // underlying Tooltip child's `aria-label`-free title attribute via the
    // rendered tooltip content once triggered.
    const user = userEvent.setup();
    await user.hover(like);
    expect(await screen.findByText('Likes: 3')).toBeInTheDocument();
  });

  it('a vote reveals the comment control, and saving a comment POSTs it with the same rating', async () => {
    const sink: CapturedRequest[] = [];
    renderControl({ likes: 1, dislikes: 0, mine: { rating: 1 } }, sink);
    const user = userEvent.setup();

    const commentButton = await screen.findByRole('button', { name: 'Add a comment' });
    await user.click(commentButton);

    const textarea = await screen.findByPlaceholderText('What could be better? (optional)');
    await user.type(textarea, 'Great answer, thanks!');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(sink).toHaveLength(1));
    expect(sink[0]).toEqual({ method: 'POST', body: { rating: 1, comment: 'Great answer, thanks!' } });
  });

  it('is keyboard reachable: Tab lands on the like button and Enter activates it', async () => {
    const sink: CapturedRequest[] = [];
    renderControl({ likes: 0, dislikes: 0 }, sink);
    const user = userEvent.setup();

    await screen.findByRole('button', { name: 'Like this answer' });
    await user.tab();
    expect(screen.getByRole('button', { name: 'Like this answer' })).toHaveFocus();

    await user.keyboard('{Enter}');

    await waitFor(() => expect(sink).toHaveLength(1));
    expect(sink[0]).toEqual({ method: 'POST', body: { rating: 1 } });
  });
});
