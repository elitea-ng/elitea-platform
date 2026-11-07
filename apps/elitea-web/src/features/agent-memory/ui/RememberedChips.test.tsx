import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithMemoryProviders as renderWithProviders } from '../__tests__/testUtils';
import { memoryEntry } from '../__tests__/fixtures';
import { RememberedChips } from './RememberedChips';

const BASE = '/api/v2';
const ENTRY = `${BASE}/elitea_core/agent_memory/prompt_lib/:projectId/:applicationId/:entryId`;
const LIST = `${BASE}/elitea_core/agent_memory/prompt_lib/:projectId/:applicationId`;

const noBody = () => undefined;

function rememberCall(status: 'active' | 'pending', entryId = 12) {
  return {
    id: 'call-1',
    name: 'remember',
    status: 'complete',
    toolInputs: { kind: 'preference', content: 'Prefers short answers' },
    toolOutputs: JSON.stringify({ status, entry_id: String(entryId), note: '' }),
  };
}

function renderChips(status: 'active' | 'pending') {
  return renderWithProviders(
    <RememberedChips toolActions={[rememberCall(status)]} projectId="1" applicationId={41} agentName="Mira" useStepBody={noBody} />,
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('RememberedChips', () => {
  it('says what the agent remembered', () => {
    renderChips('active');
    expect(screen.getByTestId('agent-memory-chip-12')).toHaveTextContent('Mira remembered: Prefers short answers');
  });

  it('renders nothing without the agent behind the answer', () => {
    renderWithProviders(<RememberedChips toolActions={[rememberCall('active')]} projectId="1" applicationId={undefined} agentName="Mira" useStepBody={noBody} />);
    expect(screen.queryByTestId('agent-memory-chip-12')).not.toBeInTheDocument();
  });

  it('approves a pending note when the reader may', async () => {
    let patched: unknown;
    server.use(
      http.get(LIST, () => HttpResponse.json({ rows: [], total: 0, people: [], can_manage: true, can_delete: true, can_approve: true, opted_out: false })),
      http.patch(ENTRY, async ({ request }) => {
        patched = await request.json();
        return HttpResponse.json(memoryEntry({ id: 12, status: 'active' }));
      }),
    );
    renderChips('pending');
    expect(screen.getByTestId('agent-memory-chip-12')).toHaveTextContent('waiting for approval');
    await userEvent.click(await screen.findByTestId('agent-memory-chip-12-approve'));
    await waitFor(() => expect(screen.getByTestId('agent-memory-chip-12')).toHaveAttribute('data-state', 'active'));
    expect(patched).toEqual({ status: 'active' });
  });

  it('does not offer Approve when the reader may not approve', async () => {
    server.use(http.get(LIST, () => HttpResponse.json({ rows: [], total: 0, people: [], can_manage: false, can_delete: false, can_approve: false, opted_out: false })));
    renderChips('pending');
    await waitFor(() => expect(screen.getByTestId('agent-memory-chip-12-edit')).toBeInTheDocument());
    expect(screen.queryByTestId('agent-memory-chip-12-approve')).not.toBeInTheDocument();
  });

  it('edits the note and shows the new text', async () => {
    server.use(http.patch(ENTRY, () => HttpResponse.json(memoryEntry({ id: 12, content: 'Prefers bullet points' }))));
    renderChips('active');
    await userEvent.click(screen.getByTestId('agent-memory-chip-12-edit'));
    const input = await screen.findByTestId('agent-memory-edit-content');
    await userEvent.clear(input);
    await userEvent.type(input, 'Prefers bullet points');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.getByTestId('agent-memory-chip-12-text')).toHaveTextContent('Mira remembered: Prefers bullet points'));
  });

  it('forgets the note', async () => {
    server.use(http.delete(ENTRY, () => new HttpResponse(null, { status: 204 })));
    renderChips('active');
    await userEvent.click(screen.getByTestId('agent-memory-chip-12-forget'));
    await waitFor(() => expect(screen.getByTestId('agent-memory-chip-12')).toHaveTextContent('Mira forgot this.'));
  });

  it("shows the server's refusal on the chip", async () => {
    server.use(http.delete(ENTRY, () => HttpResponse.json({ error: 'This note is about someone else.', code: 'memory_change_forbidden' }, { status: 403 })));
    renderChips('active');
    await userEvent.click(screen.getByTestId('agent-memory-chip-12-forget'));
    expect(await screen.findByTestId('agent-memory-chip-12-error')).toHaveTextContent('This note is about someone else.');
  });
});
