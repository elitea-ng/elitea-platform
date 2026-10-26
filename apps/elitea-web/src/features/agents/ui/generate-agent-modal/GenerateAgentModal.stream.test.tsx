import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { GenerateAgentModal } from './GenerateAgentModal';

/**
 * The streamed builder (ADR-0037): the draft arrives as `draft.token` frames,
 * then `draft.complete`; "Refine" sends the next turn with the conversation
 * and the current draft.
 */
const STREAM_PATH = '*/elitea_core/generate_application_draft_stream/prompt_lib/:projectId';

const draft = (name: string) => ({
  name,
  description: 'Triages incidents',
  instructions: 'Sort by severity.',
  welcome_message: 'What broke?',
  conversation_starters: ['Triage this'],
  suggested_toolkits: [],
  suggested_mcp: [],
  suggested_pipelines: [],
  suggested_agents: [],
  suggested_skills: [],
});

function sse(frames: string[]): HttpResponse<ReadableStream> {
  const encoder = new TextEncoder();
  const body = new ReadableStream({
    start(controller) {
      for (const frame of frames) controller.enqueue(encoder.encode(frame));
      controller.close();
    },
  });
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } });
}

const token = (text: string): string => `event: draft.token\ndata: ${JSON.stringify({ text })}\n\n`;
const complete = (value: unknown): string => `event: draft.complete\ndata: ${JSON.stringify(value)}\n\n`;

function renderModal(): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  renderWithTheme(
    (
      <QueryClientProvider client={queryClient}>
        <GenerateAgentModal open onClose={vi.fn()} projectId="p1" onAgentCreated={vi.fn()} />
      </QueryClientProvider>
    ) as ReactElement,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('GenerateAgentModal, streamed', () => {
  it('streams a draft into the review form, then refines it with the conversation and the current draft', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.post(STREAM_PATH, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return bodies.length === 1 ? sse([token('{"name": "Tri'), complete(draft('Triager'))]) : sse([complete(draft('On-call Triager'))]);
      }),
    );
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'An incident triager' } });
    fireEvent.click(screen.getByText('Generate'));
    expect(await screen.findByDisplayValue('Triager')).toBeInTheDocument();
    expect(bodies[0]).toEqual({ user_description: 'An incident triager' });

    fireEvent.change(screen.getByLabelText('Change the draft'), { target: { value: 'Name the on-call engineer too' } });
    fireEvent.click(screen.getByText('Refine'));
    expect(await screen.findByDisplayValue('On-call Triager')).toBeInTheDocument();
    expect(bodies[1]).toMatchObject({
      user_description: 'Name the on-call engineer too',
      conversation_history: [
        { role: 'user', content: 'An incident triager' },
        { role: 'assistant', content: 'Triager: Triages incidents' },
      ],
      current_draft: { name: 'Triager', instructions: 'Sort by severity.' },
    });
  });

  it('shows the draft.error message and keeps the prompt', async () => {
    server.use(
      http.post(STREAM_PATH, () =>
        sse([`event: draft.error\ndata: ${JSON.stringify({ code: 'agent_draft_invalid', status: 422, message: 'The draft has no name.' })}\n\n`]),
      ),
    );
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'Anything' } });
    fireEvent.click(screen.getByText('Generate'));
    expect(await screen.findByText('The draft has no name.')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Anything')).toBeInTheDocument();
  });

  it('names the server refusal before the stream opens', async () => {
    server.use(http.post(STREAM_PATH, () => HttpResponse.json({ error: 'the LLM gateway is not configured', code: 'llm_gateway_not_configured' }, { status: 503 })));
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'Anything' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText('the LLM gateway is not configured')).toBeInTheDocument());
  });
});
