import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { MessageContextView } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { server } from '@/test/setup';

import { fetchMessageContextView } from '../../api/contextViewApi';
import { ApplicationAnswerActions } from './ApplicationAnswerActions';
import { ContextViewInspector } from './ContextViewInspector';

const BASE = '/api/v2';
const MESSAGE = '10000000-0000-4000-8000-000000001669';
const BEFORE = '10000000-0000-4000-8000-000000000001';
const AFTER = '10000000-0000-4000-8000-000000000002';
const URL = `${BASE}/elitea_core/message_context_view/prompt_lib/7/${MESSAGE}`;
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const VIEW: MessageContextView = {
  message_id: MESSAGE,
  reader_participant_id: 31,
  reader_name: 'Reviewer',
  history: 'since_joined',
  include_side_talk: 'party',
  participant_history: 'since_joined',
  window_tokens: 4096,
  digest: 'a'.repeat(64),
  recorded_at: '2026-11-09T10:00:00Z',
  entries: [
    { message_id: BEFORE, reason: 'before_joined', author_name: '<img src=x onerror=alert(1)>', author_kind: 'user', created_at: '2026-11-09T09:00:00Z' },
    { message_id: AFTER, reason: null, author_name: 'Alice', author_kind: 'user', created_at: '2026-11-09T09:59:00Z' },
  ],
  compaction: null,
};

function renderWithClient(node: React.ReactNode): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>{node}</ThemeProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('fetchMessageContextView', () => {
  it('reads a recorded view', async () => {
    server.use(http.get(URL, () => HttpResponse.json(VIEW)));
    await expect(fetchMessageContextView('7', MESSAGE)).resolves.toEqual({ kind: 'view', view: VIEW });
  });

  it('reads a 404 as not recorded, and a 403 as forbidden', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ error: 'not found' }, { status: 404 })));
    await expect(fetchMessageContextView('7', MESSAGE)).resolves.toEqual({ kind: 'not_recorded' });
    server.use(http.get(URL, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })));
    await expect(fetchMessageContextView('7', MESSAGE)).resolves.toEqual({ kind: 'forbidden' });
  });

  it('sends nothing for an id that is not one the server minted', async () => {
    const hit = vi.fn();
    server.use(http.get(`${BASE}/elitea_core/message_context_view/*`, () => { hit(); return HttpResponse.json(VIEW); }));
    await expect(fetchMessageContextView('7', '../../x')).resolves.toEqual({ kind: 'not_recorded' });
    await expect(fetchMessageContextView('0', MESSAGE)).resolves.toEqual({ kind: 'not_recorded' });
    expect(hit).not.toHaveBeenCalled();
  });
});

describe('ContextViewInspector', () => {
  it('lists what the agent saw and what it did not, with the reason', async () => {
    server.use(http.get(URL, () => HttpResponse.json(VIEW)));
    renderWithClient(<ContextViewInspector open projectId="7" messageId={MESSAGE} onClose={vi.fn()} />);

    expect(await screen.findByTestId('context-view')).toBeInTheDocument();
    expect(screen.getByTestId('context-view-mode')).toHaveTextContent('Since it joined');
    expect(screen.getByTestId('context-view-window')).toHaveTextContent('4096 tokens');
    expect(screen.getByTestId('context-view-digest')).toHaveTextContent('a'.repeat(64));
    expect(screen.getByTestId('context-view-compaction')).toHaveTextContent('Not reported');
    const included = screen.getAllByTestId('context-view-included');
    expect(included.map((row) => row.getAttribute('data-message-id'))).toEqual([AFTER]);
    const excluded = screen.getAllByTestId('context-view-excluded');
    expect(excluded.map((row) => row.getAttribute('data-message-id'))).toEqual([BEFORE]);
    expect(screen.getByTestId('context-view-reason')).toHaveTextContent('Sent before it joined');
  });

  it('shows an author name as text, never as markup', async () => {
    server.use(http.get(URL, () => HttpResponse.json(VIEW)));
    renderWithClient(<ContextViewInspector open projectId="7" messageId={MESSAGE} onClose={vi.fn()} />);

    const excluded = await screen.findByTestId('context-view-excluded');
    expect(excluded).toHaveTextContent('<img src=x onerror=alert(1)>');
    expect(document.querySelector('img[src="x"]')).toBeNull();
  });

  it('says so when no view was recorded', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ error: 'not found' }, { status: 404 })));
    renderWithClient(<ContextViewInspector open projectId="7" messageId={MESSAGE} onClose={vi.fn()} />);
    expect(await screen.findByTestId('context-view-not-recorded')).toBeInTheDocument();
  });
});

describe('ApplicationAnswerActions — What the agent saw', () => {
  const state = { hasContent: true, isProcessing: false, shouldDisableRegenerate: false, hasSpeakableText: false, isSpeaking: false };

  it('opens the inspector from the ⋯ menu of a settled answer', async () => {
    server.use(http.get(URL, () => HttpResponse.json(VIEW)));
    renderWithClient(<ApplicationAnswerActions state={state} handlers={{}} feedback={{ projectId: '7', messageId: MESSAGE }} memoryContent="x" alwaysVisible />);

    fireEvent.click(screen.getByTestId('answer-more-actions'));
    fireEvent.click(screen.getByTestId('answer-context-view'));
    expect(await screen.findByTestId('context-view')).toBeInTheDocument();
  });

  it('reads the view afresh on every open, so a failed load retries and a regenerated answer shows its new view', async () => {
    let calls = 0;
    server.use(
      http.get(URL, () => {
        calls += 1;
        return calls === 1 ? HttpResponse.json({ error: 'down' }, { status: 503 }) : HttpResponse.json(VIEW);
      }),
    );
    // No gcTime override here: the component itself must not keep the result.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <ApplicationAnswerActions state={state} handlers={{}} feedback={{ projectId: '7', messageId: MESSAGE }} memoryContent="x" alwaysVisible />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByTestId('answer-more-actions'));
    fireEvent.click(screen.getByTestId('answer-context-view'));
    expect(await screen.findByTestId('context-view-unavailable')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(screen.queryByTestId('context-view-unavailable')).not.toBeInTheDocument());

    fireEvent.click(screen.getByTestId('answer-more-actions'));
    fireEvent.click(screen.getByTestId('answer-context-view'));
    expect(await screen.findByTestId('context-view')).toBeInTheDocument();
    expect(calls).toBe(2);
  });

  it('offers no inspector without a resolved project and a settled answer', () => {
    renderWithClient(<ApplicationAnswerActions state={state} handlers={{ onDelete: vi.fn() }} memoryContent="x" alwaysVisible />);
    fireEvent.click(screen.getByTestId('answer-more-actions'));
    expect(screen.queryByTestId('answer-context-view')).not.toBeInTheDocument();
  });
});
