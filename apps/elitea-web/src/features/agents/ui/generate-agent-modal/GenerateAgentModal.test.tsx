import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getCreateApplicationMockHandler } from '@/shared/api/generated/applications/applications.msw';
import type { ApplicationDraft } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { GenerateAgentModal, type GenerateAgentModalProps } from './GenerateAgentModal';

/**
 * The endpoint is served (#254 P1) and orval generates
 * `getGenerateApplicationDraftMockHandler` again, but this local factory is
 * kept: it takes a PARTIAL draft over a realistic default, so each test names
 * only the field it is asserting on instead of restating the whole contract.
 */
const DEFAULT_DRAFT: ApplicationDraft = {
  name: 'Support Bot',
  description: 'Answers support questions',
  instructions: 'draft',
  welcome_message: 'How can I help?',
  conversation_starters: ['Where are my orders?'],
};

function generateAgentDraftHandler(body?: Partial<ApplicationDraft>) {
  return http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', () =>
    HttpResponse.json({ ...DEFAULT_DRAFT, ...body }),
  );
}

function renderModal(overrides: Partial<GenerateAgentModalProps> = {}): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const props: GenerateAgentModalProps = {
    open: true,
    onClose: vi.fn(),
    projectId: 'p1',
    onAgentCreated: vi.fn(),
    ...overrides,
  };
  return renderWithTheme((<QueryClientProvider client={queryClient}><GenerateAgentModal {...props} /></QueryClientProvider>) as ReactElement);
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('GenerateAgentModal', () => {
  it('disables Generate until a description is typed', () => {
    renderModal();
    expect(screen.getByText('Generate').closest('button')).toBeDisabled();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    expect(screen.getByText('Generate').closest('button')).not.toBeDisabled();
  });

  it('generates a draft and transitions to the review step, filling in every served field', async () => {
    server.use(generateAgentDraftHandler({ instructions: 'Answer support questions.' }));
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));

    await waitFor(() => expect(screen.getByDisplayValue('Answer support questions.')).toBeInTheDocument());
    expect(screen.getByText('Create Agent')).toBeInTheDocument();
    // #254 P1: before the endpoint was served, name/description/welcome
    // message arrived blank because the route answered with a chat-completion
    // envelope and only `instructions` could be honestly recovered from it.
    expect(screen.getByDisplayValue('Support Bot')).toBeInTheDocument();
    expect(screen.getByDisplayValue('Answers support questions')).toBeInTheDocument();
    expect(screen.getByDisplayValue('How can I help?')).toBeInTheDocument();
  });

  it('shows an inline error and stays on the input step when generation fails', async () => {
    server.use(
      http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'model unavailable' }, { status: 500 }),
      ),
    );
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));

    // `applicationErrorMessage` (a landed sibling file) surfaces `EliteaApiError.message`
    // — a generic "eliteaFetch: STATUS from URL" string, never the JSON body's `error`
    // field (see that file's own doc comment) — so the inline alert shows that shape,
    // not the raw backend error text.
    await waitFor(() => expect(screen.getByText(/eliteaFetch: 500/)).toBeInTheDocument());
    expect(screen.getByPlaceholderText(/Describe your agent/)).toBeInTheDocument();
  });

  it('going back to the prompt clears the draft', async () => {
    server.use(generateAgentDraftHandler({ instructions: 'Draft text' }));
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText('Back to prompt')).toBeInTheDocument());

    fireEvent.click(screen.getByText('Back to prompt'));

    expect(screen.getByPlaceholderText(/Describe your agent/)).toBeInTheDocument();
  });

  it('creates the agent and calls onAgentCreated on approve', async () => {
    server.use(
      generateAgentDraftHandler({ instructions: 'Draft text' }),
      getCreateApplicationMockHandler({
        id: '42',
        name: 'New Agent',
        description: '',
        type: 'interface',
        icon: '',
        owner_id: 'u1',
        created_at: '2026-01-01T00:00:00Z',
      }),
    );
    const onAgentCreated = vi.fn();
    const onClose = vi.fn();
    renderModal({ onAgentCreated, onClose });

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText('Create Agent')).toBeInTheDocument());

    // `validateAgentDraft` requires both `name` and `description`. The served
    // draft now fills them in, and the user edits them — which is what the
    // review step is for.
    fireEvent.change(screen.getByTestId('agent-draft-name-input'), { target: { value: 'New Agent' } });
    fireEvent.change(screen.getByTestId('agent-draft-description-input'), { target: { value: 'A helpful agent' } });
    await waitFor(() => expect(screen.getByText('Create Agent').closest('button')).not.toBeDisabled());
    fireEvent.click(screen.getByText('Create Agent'));

    await waitFor(() => expect(onAgentCreated).toHaveBeenCalledWith(expect.objectContaining({ id: '42', name: 'New Agent' })));
    expect(onClose).toHaveBeenCalled();
  });

  it('reports a failed approve via onApproveError instead of throwing', async () => {
    server.use(
      generateAgentDraftHandler({ instructions: 'Draft text' }),
      http.post('*/elitea_core/applications/prompt_lib/:projectId', () =>
        HttpResponse.json({ error: 'create failed' }, { status: 500 }),
      ),
    );
    const onApproveError = vi.fn();
    renderModal({ onApproveError });

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText('Create Agent')).toBeInTheDocument());

    fireEvent.change(screen.getByTestId('agent-draft-name-input'), { target: { value: 'New Agent' } });
    fireEvent.change(screen.getByTestId('agent-draft-description-input'), { target: { value: 'A helpful agent' } });
    await waitFor(() => expect(screen.getByText('Create Agent').closest('button')).not.toBeDisabled());
    fireEvent.click(screen.getByText('Create Agent'));

    // `useAgentDraftApproval.approve` swallows `useCreateApplicationDraft`'s own caught
    // error (it sets `.error` state rather than rejecting) and throws its own literal
    // "Failed to create the agent." instead — see `useAgentDraftApproval.ts`'s `approve`.
    await waitFor(() => expect(onApproveError).toHaveBeenCalledWith('Failed to create the agent.'));
  });

  it('disables Create Agent when the user clears the name', async () => {
    // Before #254 P1 the served draft carried no name, so this case arrived
    // for free. It now takes an edit — which is the real path anyway: the
    // review step exists so the user can change what the model produced, and
    // emptying a required field must block the create.
    server.use(generateAgentDraftHandler({ instructions: 'Draft text' }));
    renderModal();

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText('Create Agent')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('Create Agent').closest('button')).not.toBeDisabled());

    fireEvent.change(screen.getByTestId('agent-draft-name-input'), { target: { value: '' } });

    // `isDraftValid` only flips once `GenerateAgentReviewForm`'s own validation
    // effect runs — wait for it rather than asserting synchronously.
    await waitFor(() => expect(screen.getByText('Create Agent').closest('button')).toBeDisabled());
  });

  it('autofocuses the description field when the modal opens', () => {
    renderModal();
    expect(screen.getByPlaceholderText(/Describe your agent/)).toHaveFocus();
  });

  it('pressing Enter in the description field triggers Generate', async () => {
    server.use(generateAgentDraftHandler({ instructions: 'From Enter' }));
    renderModal();

    const textarea = screen.getByPlaceholderText(/Describe your agent/);
    fireEvent.change(textarea, { target: { value: 'A support bot' } });
    fireEvent.keyDown(textarea, { key: 'Enter' });

    await waitFor(() => expect(screen.getByDisplayValue('From Enter')).toBeInTheDocument());
  });

  it('Shift+Enter in the description field inserts a newline instead of triggering Generate', () => {
    renderModal();

    const textarea = screen.getByPlaceholderText(/Describe your agent/);
    fireEvent.change(textarea, { target: { value: 'A support bot' } });
    fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });

    expect(screen.queryByText(/Generating agent draft/)).not.toBeInTheDocument();
    expect(screen.getByPlaceholderText(/Describe your agent/)).toBeInTheDocument();
  });

  it('closing the modal while a generate request is in flight discards the response instead of reopening into a stale review step', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    let resolveResponse: (() => void) | undefined;
    const responseGate = new Promise<void>((resolve) => {
      resolveResponse = resolve;
    });
    server.use(
      http.post('*/elitea_core/generate_application_draft/prompt_lib/:projectId', async () => {
        await responseGate;
        return HttpResponse.json({ ...DEFAULT_DRAFT, instructions: 'Stale draft text' });
      }),
    );

    const onClose = vi.fn();
    const props: GenerateAgentModalProps = { open: true, onClose, projectId: 'p1', onAgentCreated: vi.fn() };
    const { rerender } = renderWithTheme(
      (<QueryClientProvider client={queryClient}><GenerateAgentModal {...props} /></QueryClientProvider>) as ReactElement,
    );

    fireEvent.change(screen.getByPlaceholderText(/Describe your agent/), { target: { value: 'A support bot' } });
    fireEvent.click(screen.getByText('Generate'));
    await waitFor(() => expect(screen.getByText(/Generating agent draft/)).toBeInTheDocument());

    // Close mid-flight — the X button, Escape, and backdrop all wire to the same `handleClose`.
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);

    // Simulate the parent (`GenerateAgentButton`) reacting to `onClose` by hiding the dialog.
    rerender(
      (<QueryClientProvider client={queryClient}><GenerateAgentModal {...props} open={false} /></QueryClientProvider>) as ReactElement,
    );

    // Let the in-flight request resolve only now, after close.
    resolveResponse?.();
    await waitFor(() => expect(queryClient.isFetching()).toBe(0));

    // Reopen — must land on a fresh input step, not the stale review step with the
    // just-arrived (unrequested-by-then) draft content.
    rerender(
      (<QueryClientProvider client={queryClient}><GenerateAgentModal {...props} open /></QueryClientProvider>) as ReactElement,
    );

    expect(screen.getByPlaceholderText(/Describe your agent/)).toBeInTheDocument();
    expect(screen.queryByDisplayValue('Stale draft text')).not.toBeInTheDocument();
    expect(screen.queryByText('Create Agent')).not.toBeInTheDocument();
  });
});
