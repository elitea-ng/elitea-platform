import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';

import { scorecard, workforceAgent, workforceProposal } from '../__tests__/fixtures';
import { WorkforcePanel } from './WorkforcePanel';

const BASE = '/api/v2';
const ROOT = `${BASE}/elitea_core/agent_workforce/prompt_lib/:projectId`;

function mock(agent = workforceAgent({ scorecard: scorecard() }), proposals = [workforceProposal()]): void {
  server.use(
    http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 4, name: 'Ann' })),
    http.get(`${ROOT}/agents/:applicationId`, () => HttpResponse.json(agent)),
    http.get(`${ROOT}/agents/:applicationId/scorecards`, () => HttpResponse.json({ rows: [scorecard()], total: 1 })),
    http.get(`${ROOT}/agents/:applicationId/impact`, () =>
      HttpResponse.json({
        conversations: 3,
        conversations_led: 1,
        teams: [],
        nested_by: [{ application_id: 30, name: 'Router' }],
        schedules: 1,
        triggers: 0,
        published_versions: 2,
        suspension: null,
        legal_hold: false,
        lifecycle_state: 'active',
      }),
    ),
    http.get(`${ROOT}/proposals`, () => HttpResponse.json({ rows: proposals, total: proposals.length })),
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: BASE }));
afterEach(() => resetGeneratedClient());

describe('WorkforcePanel', () => {
  it('asks to save the agent first', () => {
    renderWithQueryAndTheme(<WorkforcePanel projectId="1" applicationId={undefined} />);
    expect(screen.getByTestId('agent-workforce-unsaved')).toBeInTheDocument();
  });

  it('shows the state, the scorecard and the proposals', async () => {
    mock();
    renderWithQueryAndTheme(<WorkforcePanel projectId="1" applicationId={41} />);
    expect(await screen.findByTestId('agent-workforce-state')).toHaveTextContent('Active');
    expect(screen.getByTestId('agent-workforce-scorecard')).toHaveTextContent('25%');
    expect(await screen.findByTestId('agent-workforce-proposal-7')).toHaveTextContent('reviewer agent');
  });

  it('approving a retirement previews the impact and confirms the nesting', async () => {
    let body: unknown;
    mock();
    server.use(
      http.post(`${ROOT}/proposals/:proposalId/approve`, async ({ request }) => {
        body = await request.json();
        return HttpResponse.json(workforceProposal({ status: 'approved' }));
      }),
    );
    renderWithQueryAndTheme(<WorkforcePanel projectId="1" applicationId={41} />);
    await userEvent.click(await screen.findByTestId('agent-workforce-approve-7'));
    expect(await screen.findByTestId('agent-workforce-impact-nested')).toHaveTextContent('Router');
    await userEvent.click(screen.getByTestId('agent-workforce-confirm-nested'));
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(body).toEqual({ confirm_nested: true }));
  });

  it('a manager reinstates a retired agent', async () => {
    let called = false;
    mock(workforceAgent({ lifecycle_state: 'retired', reinstate_until: '2099-01-01T00:00:00Z' }), []);
    server.use(
      http.post(`${ROOT}/agents/:applicationId/reinstate`, () => {
        called = true;
        return HttpResponse.json(workforceAgent());
      }),
    );
    renderWithQueryAndTheme(<WorkforcePanel projectId="1" applicationId={41} />);
    expect(await screen.findByTestId('agent-workforce-state')).toHaveTextContent('Retired');
    await userEvent.click(screen.getByTestId('agent-workforce-reinstate'));
    await waitFor(() => expect(called).toBe(true));
  });

  it('shows the server refusal of a proposal', async () => {
    mock(workforceAgent(), []);
    server.use(
      http.post(`${ROOT}/proposals`, () =>
        HttpResponse.json({ error: 'workforce_guard_refused', code: 'workforce_guard_refused', message: 'A critical-tier agent is exempt.' }, { status: 403 }),
      ),
    );
    renderWithQueryAndTheme(<WorkforcePanel projectId="1" applicationId={41} />);
    await userEvent.click(await screen.findByTestId('agent-workforce-propose'));
    await userEvent.type(screen.getByTestId('agent-workforce-propose-rationale'), 'Unused for months');
    await userEvent.click(screen.getByRole('button', { name: 'Propose' }));
    expect(await screen.findByTestId('agent-workforce-propose-error')).toHaveTextContent('critical-tier');
  });
});
