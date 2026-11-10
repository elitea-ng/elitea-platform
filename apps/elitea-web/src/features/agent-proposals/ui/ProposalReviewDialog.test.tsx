import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithQueryAndTheme as renderWithProviders } from '@/test/renderWithQueryAndTheme';
import { server } from '@/test/setup';

import { proposalFixture } from '../__tests__/fixtures';
import { ProposalReviewDialog } from './ProposalReviewDialog';

const BASE = '/api/v2';
const CREATE = `${BASE}/elitea_core/applications/prompt_lib/:projectId`;
const CREATED = `${BASE}/elitea_core/agent_proposals/prompt_lib/:projectId/:proposalId/created`;
const DISMISS = `${BASE}/elitea_core/agent_proposals/prompt_lib/:projectId/:proposalId/dismiss`;

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('ProposalReviewDialog', () => {
  it('opens prefilled, with the soul, the tool categories and the low tier', () => {
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture()} onClose={() => undefined} />);
    expect(screen.getByTestId('agent-proposal-name')).toHaveValue('Ada');
    expect(screen.getByTestId('agent-proposal-instructions')).toHaveValue('Review pull requests.');
    expect(screen.getByTestId('agent-proposal-soul')).toHaveValue('Precise and brief.');
    expect(screen.getByTestId('agent-proposal-tool-github/read_file')).toHaveTextContent('Read');
    expect(screen.getByTestId('agent-proposal-tier')).toHaveTextContent('Low risk tier');
  });

  it('creates the agent first, then records it with the kept memory only', async () => {
    const calls: string[] = [];
    let createBody: Record<string, unknown> | undefined;
    let createdBody: unknown;
    server.use(
      http.post(CREATE, async ({ request }) => {
        calls.push('create');
        createBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ id: '77', name: 'Ada', description: '', type: 'interface', icon: '', owner_id: '1', created_at: '2026-11-09T10:00:00Z' }, { status: 201 });
      }),
      http.post(CREATED, async ({ request }) => {
        calls.push('created');
        createdBody = await request.json();
        return HttpResponse.json({ proposal: proposalFixture({ status: 'created', created_application_id: 77 }), memory_seeded: 1 });
      }),
    );
    const onCreated = vi.fn();
    const onClose = vi.fn();
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture()} onClose={onClose} onCreated={onCreated} />);
    await userEvent.click(screen.getByTestId('agent-proposal-seed-0'));
    await userEvent.click(screen.getByTestId('agent-proposal-create'));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith({ applicationId: 77, memorySeeded: 1 }));
    expect(calls).toEqual(['create', 'created']);
    expect(createBody?.['name']).toBe('Ada');
    expect(createdBody).toEqual({ application_id: 77, memory_seeds: [1] });
    expect(onClose).toHaveBeenCalled();
  });

  it('shows the server refusal when the proposal was already decided', async () => {
    server.use(http.post(DISMISS, () => HttpResponse.json({ error: 'The proposal was already decided.', code: 'proposal_decided' }, { status: 409 })));
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture()} onClose={() => undefined} />);
    await userEvent.click(screen.getByTestId('agent-proposal-dismiss'));
    expect(await screen.findByTestId('agent-proposal-error')).toHaveTextContent('already decided');
  });

  it('offers no decision on a decided proposal', () => {
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture({ status: 'dismissed' })} onClose={() => undefined} />);
    expect(screen.getByTestId('agent-proposal-create')).toBeDisabled();
    expect(screen.getByTestId('agent-proposal-dismiss')).toBeDisabled();
  });

  it('a failed record after the agent exists says so, and the retry records the same agent', async () => {
    const calls: string[] = [];
    let recordAttempts = 0;
    server.use(
      http.post(CREATE, () => {
        calls.push('create');
        return HttpResponse.json({ id: '77', name: 'Ada', description: '', type: 'interface', icon: '', owner_id: '1', created_at: '2026-11-09T10:00:00Z' }, { status: 201 });
      }),
      http.post(CREATED, async ({ request }) => {
        calls.push('created');
        recordAttempts += 1;
        if (recordAttempts === 1) return HttpResponse.json({ error: 'boom' }, { status: 500 });
        expect(await request.json()).toMatchObject({ application_id: 77 });
        return HttpResponse.json({ proposal: proposalFixture({ status: 'created', created_application_id: 77 }), memory_seeded: 0 });
      }),
    );
    const onCreated = vi.fn();
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture()} onClose={() => undefined} onCreated={onCreated} />);
    await userEvent.click(screen.getByTestId('agent-proposal-create'));
    expect(await screen.findByTestId('agent-proposal-error')).toHaveTextContent(/created|boom/);
    await userEvent.click(screen.getByTestId('agent-proposal-create'));
    await waitFor(() => expect(onCreated).toHaveBeenCalledWith({ applicationId: 77, memorySeeded: 0 }));
    expect(calls).toEqual(['create', 'created', 'created']);
  });

  it('cuts a long prefilled name and description to the create bounds', () => {
    const proposal = proposalFixture();
    const long = { ...proposal, draft: { ...proposal.draft, name: 'n'.repeat(80), description: 'd'.repeat(3000) } };
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={long} onClose={() => undefined} />);
    expect(screen.getByTestId('agent-proposal-name')).toHaveValue('n'.repeat(32));
    expect(screen.getByTestId('agent-proposal-description')).toHaveValue('d'.repeat(2304));
  });

  it('refuses Create without a name', async () => {
    renderWithProviders(<ProposalReviewDialog projectId="1" proposal={proposalFixture()} onClose={() => undefined} />);
    await userEvent.clear(screen.getByTestId('agent-proposal-name'));
    expect(screen.getByTestId('agent-proposal-create')).toBeDisabled();
  });
});
