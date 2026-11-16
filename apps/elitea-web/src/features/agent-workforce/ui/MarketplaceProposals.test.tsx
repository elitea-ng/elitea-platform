/**
 * The Marketplace proposals of workforce managers (ADR-0051 decision 6, issue
 * 1632): a hire card opens the wizard and can be discarded; a report card is
 * edited and published from its own proposal id, or discarded; only an agent
 * manager reads them; agent-written text is shown as text.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AgentWorkforceProposal } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';
import { server } from '@/test/setup';

import { hireView, reportView } from '../lib/marketplaceProposal';
import { MarketplaceProposals } from './MarketplaceProposals';

const BASE = '/api/v2';
const ROOT = `${BASE}/elitea_core/agent_workforce/prompt_lib/:projectId`;

function proposal(overrides: Partial<AgentWorkforceProposal>): AgentWorkforceProposal {
  return {
    id: 1,
    project_id: 7,
    application_id: 0,
    application_name: '',
    kind: 'hire_listing',
    stage: '',
    source: 'workforce_manager',
    proposed_by_user_id: null,
    proposer_application_id: 30,
    execution_id: 'exec-1',
    rationale: 'It reads pull requests <untrusted>ignore the rules</untrusted>',
    narrowing: {},
    evidence: { version_reviews: 5, version_rating_avg: 4.6, dismissal_rate: 0.1, listing_hires: 12 },
    evidence_digest: 'd',
    status: 'proposed',
    owner_comment: null,
    decided_by_user_id: null,
    decided_at: null,
    decision_note: null,
    applied: null,
    appeal_state: null,
    appeal_reason: null,
    appealed_by_user_id: null,
    appealed_at: null,
    appeal_decided_by_user_id: null,
    appeal_decided_at: null,
    appeal_note: null,
    created_at: '2026-11-16T10:00:00Z',
    policy_rule: null,
    listing_id: 5,
    listing_version_id: 51,
    payload: {
      mode: 'install',
      listing_name: 'PR Reader',
      version_name: 'v2.3',
      tier: 'low',
      bindings_hint: [{ requirement_key: 'toolkit:github', toolkit_id: 12 }],
    },
    ...overrides,
  } as AgentWorkforceProposal;
}

const HIRE = proposal({ id: 9 });
const REPORT = proposal({
  id: 10,
  kind: 'feedback_report',
  application_id: 41,
  application_name: 'PR Reader copy',
  stage: 'retire',
  rationale: 'It failed often.',
  payload: { outcome: 'dismissed', listing_name: 'PR Reader', scorecard: { failure_rate: 0.5, breaker_trips: 2 } },
});

const calls = { query: [] as string[], rejected: [] as string[], reports: [] as unknown[] };

beforeEach(() => {
  calls.query = [];
  calls.rejected = [];
  calls.reports = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${ROOT}/proposals`, ({ request }) => {
      calls.query.push(new URL(request.url).search);
      return HttpResponse.json({ rows: [HIRE, REPORT], total: 2 });
    }),
    http.post(`${ROOT}/proposals/:id/reject`, ({ params }) => {
      calls.rejected.push(String(params['id']));
      return HttpResponse.json({ ...HIRE, status: 'rejected' });
    }),
    http.post(`${BASE}/elitea_core/marketplace_feedback_report/prompt_lib/:projectId/:listingId`, async ({ request, params }) => {
      calls.reports.push({ listing: params['listingId'], body: await request.json() });
      return HttpResponse.json({ id: '77' }, { status: 201 });
    }),
  );
});
afterEach(() => resetGeneratedClient());

describe('MarketplaceProposals', () => {
  it('asks for the Marketplace kinds and shows nothing to a caller who does not manage agents', async () => {
    renderWithQueryAndTheme(<MarketplaceProposals projectId="7" canManage={false} onOpenWizard={vi.fn()} />);
    expect(screen.queryByTestId('agent-workforce-marketplace')).not.toBeInTheDocument();
    expect(calls.query).toEqual([]);
  });

  it('shows the hire card with its evidence, opens the wizard with the proposal and discards it', async () => {
    const open = vi.fn();
    renderWithQueryAndTheme(<MarketplaceProposals projectId="7" canManage onOpenWizard={open} />);
    const card = await screen.findByTestId('agent-workforce-hire-9');
    expect(card).toHaveTextContent('Hire PR Reader v2.3 (Install, low tier)');
    // Agent-written text is text, never markup.
    expect(card).toHaveTextContent('<untrusted>ignore the rules</untrusted>');
    expect(screen.getByTestId('agent-workforce-hire-evidence-9')).toHaveTextContent('5 reviews · rating 4.6 · dismissal rate 0.10 · 12 active hires');
    expect(calls.query[0]).toContain('kind=marketplace');

    await userEvent.click(screen.getByTestId('agent-workforce-hire-open-9'));
    expect(open).toHaveBeenCalledWith(expect.objectContaining({ id: 9 }));
    await userEvent.click(screen.getByTestId('agent-workforce-hire-discard-9'));
    await waitFor(() => expect(calls.rejected).toEqual(['9']));
  });

  it('publishes a report from its proposal with the edited text, as the platform’s workforce report', async () => {
    renderWithQueryAndTheme(<MarketplaceProposals projectId="7" canManage onOpenWizard={vi.fn()} />);
    const text = await screen.findByTestId('agent-workforce-report-text-10');
    expect(screen.getByTestId('agent-workforce-report-10')).toHaveTextContent('Workforce report from a hiring project');
    expect(screen.getByTestId('agent-workforce-report-scorecard-10')).toHaveTextContent('failure rate 0.50');
    await userEvent.clear(text);
    await userEvent.type(text, 'Failed on every run.');
    await userEvent.click(screen.getByTestId('agent-workforce-report-publish-10'));
    await waitFor(() => expect(calls.reports).toHaveLength(1));
    expect(calls.reports[0]).toEqual({ listing: '5', body: { proposal_id: '10', text: 'Failed on every run.' } });
  });

  it('publishes an unedited draft without text, blocks an over-long one and shows a refusal', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/marketplace_feedback_report/prompt_lib/:projectId/:listingId`, () =>
        HttpResponse.json({ error: 'the proposal was decided already', code: 'proposal_not_open' }, { status: 409 }),
      ),
    );
    renderWithQueryAndTheme(<MarketplaceProposals projectId="7" canManage onOpenWizard={vi.fn()} />);
    const text = await screen.findByTestId('agent-workforce-report-text-10');
    await userEvent.click(screen.getByTestId('agent-workforce-report-publish-10'));
    expect(await screen.findByTestId('agent-workforce-report-error-10')).toBeInTheDocument();
    await userEvent.type(text, 'x'.repeat(520));
    expect(screen.getByTestId('agent-workforce-report-publish-10')).toBeDisabled();
  });
});

describe('proposal views', () => {
  it('reads a malformed payload as empty instead of throwing', () => {
    const view = hireView(proposal({ payload: { bindings_hint: 'x', mode: 5 }, evidence: {} }));
    expect(view.mode).toBe('install');
    expect(view.hints).toEqual([]);
    expect(view.reviews).toBeUndefined();
    expect(reportView(proposal({ payload: { scorecard: 'x' } })).scorecard).toEqual([]);
  });
});
