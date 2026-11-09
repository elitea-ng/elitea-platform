import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithQueryAndTheme as renderWithProviders } from '@/test/renderWithQueryAndTheme';
import { server } from '@/test/setup';

import { proposalFixture } from '../__tests__/fixtures';
import { CopyToProjectButton, copyableEntries } from './CopyToProjectButton';
import { ProposalCards } from './ProposalCards';
import { ProposalsInboxButton } from './ProposalsInboxButton';

const BASE = '/api/v2';
const LIST = `${BASE}/elitea_core/agent_proposals/prompt_lib/:projectId`;
const ONE = `${BASE}/elitea_core/agent_proposals/prompt_lib/:projectId/:proposalId`;
const MEMORY = `${BASE}/elitea_core/agent_memory/prompt_lib/:projectId/:applicationId`;
const EXPORT = `${BASE}/elitea_core/export_import/prompt_lib/:projectId/:id`;
const FORK = `${BASE}/elitea_core/fork/prompt_lib/:projectId`;
const COPY = `${BASE}/elitea_core/agent_copy_memory/prompt_lib/:projectId/:applicationId`;

const noBody = () => undefined;

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

function memoryRow(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id,
    application_id: 41,
    kind: 'fact',
    content: `Entry ${id}`,
    subject_user_id: null,
    pinned: false,
    status: 'active',
    version: 1,
    conversation_uuid: null,
    message_id: null,
    execution_id: null,
    written_by_kind: 'agent',
    written_by_user_id: null,
    created_at: '2026-11-01T10:00:00Z',
    updated_at: '2026-11-01T10:00:00Z',
    ...overrides,
  };
}

describe('ProposalsInboxButton', () => {
  it('counts the open proposals and opens one for review', async () => {
    server.use(http.get(LIST, () => HttpResponse.json({ rows: [proposalFixture()], total: 1 })));
    renderWithProviders(<ProposalsInboxButton projectId="1" />);
    await userEvent.click(await screen.findByTestId('agent-proposals-inbox'));
    expect(screen.getByTestId('agent-proposals-inbox')).toHaveTextContent('Proposals (1)');
    expect(screen.getByTestId('agent-proposal-row-5')).toHaveTextContent('From Mira');
    await userEvent.click(screen.getByTestId('agent-proposal-review-5'));
    expect(await screen.findByTestId('agent-proposal-review')).toBeInTheDocument();
  });

  it('takes no toolbar slot with an empty inbox', async () => {
    let asked = false;
    server.use(
      http.get(LIST, () => {
        asked = true;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );
    renderWithProviders(<ProposalsInboxButton projectId="1" />);
    await waitFor(() => expect(asked).toBe(true));
    expect(screen.queryByTestId('agent-proposals-inbox')).not.toBeInTheDocument();
  });
});

describe('ProposalCards', () => {
  const call = { id: 'call-1', name: 'propose_agent', status: 'complete', toolOutputs: JSON.stringify({ status: 'proposed', proposal_id: '5', message: '' }) };

  it('shows the proposal as the server holds it now', async () => {
    server.use(http.get(ONE, () => HttpResponse.json(proposalFixture({ status: 'created' }))));
    renderWithProviders(<ProposalCards toolActions={[call]} projectId="1" agentName="Mira" useStepBody={noBody} />);
    const card = await screen.findByTestId('agent-proposal-card-5');
    expect(card).toHaveAttribute('data-state', 'created');
    expect(card).toHaveTextContent('Mira proposes saving a sub-agent as a new agent: Ada (created)');
    expect(screen.queryByTestId('agent-proposal-card-5-review')).not.toBeInTheDocument();
  });

  it('has no card for a refused call', () => {
    const refused = { ...call, toolOutputs: { status: 'proposal_limit', message: 'three open' } };
    renderWithProviders(<ProposalCards toolActions={[refused]} projectId="1" agentName="Mira" useStepBody={noBody} />);
    expect(screen.queryByTestId('agent-proposal-card-5')).not.toBeInTheDocument();
  });
});

describe('CopyToProjectButton', () => {
  it('keeps memory about a person, a pending note and a cursor out of the picker', () => {
    const rows = [memoryRow(1), memoryRow(2, { subject_user_id: 7 }), memoryRow(3, { status: 'pending' }), memoryRow(4, { kind: 'cursor' })];
    expect(copyableEntries(rows as never).map((row) => row.id)).toEqual([1]);
  });

  it('forks the agent, then copies only the picked memory into the copy', async () => {
    let forkedInto: string | undefined;
    let copyBody: unknown;
    let copyPath: string | undefined;
    server.use(
      http.get(MEMORY, () => HttpResponse.json({ rows: [memoryRow(1), memoryRow(2)], total: 2, people: {}, can_manage: true, can_delete: true, can_approve: true, opted_out: false })),
      http.get(EXPORT, () => HttpResponse.json({ ok: true, applications: [{ name: 'Mira' }], toolkits: [] })),
      http.post(FORK, ({ params }) => {
        forkedInto = String(params['projectId']);
        return HttpResponse.json({ result: { agents: [{ id: '90', name: 'Mira' }], toolkits: [], skills: [] }, errors: { agents: [], toolkits: [], skills: [] } }, { status: 201 });
      }),
      http.post(COPY, async ({ request, params }) => {
        copyPath = `${String(params['projectId'])}/${String(params['applicationId'])}`;
        copyBody = await request.json();
        return HttpResponse.json({ copied: 1 });
      }),
    );
    const onCopied = vi.fn();
    renderWithProviders(<CopyToProjectButton projectId="3" applicationId={41} agentName="Mira" projects={[{ id: '9', name: 'Team' }]} onCopied={onCopied} />);
    await userEvent.click(screen.getByTestId('agent-copy-to-project'));
    await userEvent.click(await screen.findByTestId('agent-copy-entry-2'));
    await userEvent.click(screen.getByRole('button', { name: 'Copy' }));
    await waitFor(() => expect(onCopied).toHaveBeenCalledWith('9', 90, 1));
    expect(forkedInto).toBe('9');
    expect(copyPath).toBe('9/90');
    expect(copyBody).toEqual({ source_project_id: 3, source_application_id: 41, entry_ids: [2] });
  });

  it('is not offered without a team project to copy to', () => {
    renderWithProviders(<CopyToProjectButton projectId="3" applicationId={41} agentName="Mira" projects={[]} />);
    expect(screen.queryByTestId('agent-copy-to-project')).not.toBeInTheDocument();
  });
});
