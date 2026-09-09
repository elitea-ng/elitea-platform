/**
 * `RememberMemoryAction` — "Remember this" on an assistant message (#870).
 * MSW-backed: every assertion goes through the real
 * `POST /elitea_core/memories/prompt_lib/{projectId}` route shape, not a
 * mocked hook — same discipline `MessageFeedbackControl.test.tsx` (#880)
 * establishes for the sibling per-message action.
 */
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { RememberMemoryAction } from './RememberMemoryAction';

const BASE = '/api/v2';
const MEMORIES_URL = `${BASE}/elitea_core/memories/prompt_lib/proj-1`;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

function mount(content = 'This is the assistant answer to remember.') {
  return render(
    <AppProviders>
      <RememberMemoryAction projectId="proj-1" content={content} conversationId="conv-uuid-1" />
    </AppProviders>,
  );
}

describe('RememberMemoryAction', () => {
  it('renders nothing when there is no content to remember', () => {
    const { container } = mount('');
    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing when there is no project id', () => {
    render(
      <AppProviders>
        <RememberMemoryAction projectId="" content="something" />
      </AppProviders>,
    );
    expect(screen.queryByTestId('remember-memory-action')).not.toBeInTheDocument();
  });

  it('saves the message content as a memory, stamping the source conversation', async () => {
    let captured: unknown = null;
    server.use(
      http.post(MEMORIES_URL, async ({ request }) => {
        captured = await request.json();
        return HttpResponse.json(
          { id: 'mem-1', project_id: 'proj-1', content: 'This is the assistant answer to remember.', tags: [], enabled: true, created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z' },
          { status: 201 },
        );
      }),
    );

    mount();

    fireEvent.click(screen.getByTestId('remember-memory-action'));

    await waitFor(() =>
      expect(captured).toMatchObject({
        content: 'This is the assistant answer to remember.',
        source_conversation_id: 'conv-uuid-1',
        enabled: true,
      }),
    );
    expect(await screen.findByTestId('remember-memory-saved-icon')).toBeInTheDocument();
  });

  it('shows a toast when the save fails', async () => {
    server.use(
      http.post(MEMORIES_URL, () => HttpResponse.json({ error: 'content exceeds the maximum length' }, { status: 400 })),
    );

    mount();

    fireEvent.click(screen.getByTestId('remember-memory-action'));

    expect(await screen.findByText('content exceeds the maximum length')).toBeInTheDocument();
  });
});
