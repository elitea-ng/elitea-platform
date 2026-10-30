/**
 * Admin › Connector requests (ADR-0044 decision 1, #1501): the queue shows
 * what an approver judges, approve sends an optional reason, reject needs
 * one, and the server's refusal (self-approval) is shown in its words.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { ConnectorRequest } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConnectorRequests } from './ConnectorRequests';
import { renderAdminRoute } from './__tests__/testRouter';

const ROW: ConnectorRequest = {
  id: 5,
  project_id: 12,
  target_kind: 'toolkit_type',
  target: 'jira',
  toolkit_type: 'jira',
  platform_wide: false,
  project_ids: [12],
  tools: [{ name: 'search_issues', category: 'read' }],
  egress_hosts: ['jira.example.com'],
  data_classes: ['internal'],
  justification: 'Support triage',
  state: 'submitted',
  requested_by: 11,
  requester_name: 'Alice',
  requester_email: 'alice@example.com',
  decided_by: null,
  decision_reason: '',
  attestation_id: null,
  created_at: '2026-10-29T09:00:00Z',
  updated_at: '2026-10-29T09:00:00Z',
  submitted_at: '2026-10-29T09:00:00Z',
  decided_at: null,
};

let decisions: { path: string; body: unknown }[];
let queries: string[];

beforeEach(() => {
  decisions = [];
  queries = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/connector_requests', ({ request }) => {
      queries.push(new URL(request.url).searchParams.get('state') ?? '');
      return HttpResponse.json({ total: 1, rows: [ROW] });
    }),
    http.post('*/admin/connector_requests/:id/approve', async ({ request }) => {
      decisions.push({ path: new URL(request.url).pathname, body: await request.json() });
      return HttpResponse.json({ request: { ...ROW, state: 'approved' }, granted_projects: [12], egress_hosts: ['jira.example.com'] });
    }),
    http.post('*/admin/connector_requests/:id/reject', async ({ request }) => {
      decisions.push({ path: new URL(request.url).pathname, body: await request.json() });
      return HttpResponse.json(
        { error: 'self_approval_refused', code: 'self_approval_refused', message: 'You made this request, so another approver must decide it.' },
        { status: 403 },
      );
    }),
  );
});

afterEach(() => resetGeneratedClient());

describe('AdminConnectorRequests', () => {
  it('lists the submitted requests with what an approver judges', async () => {
    renderAdminRoute(<AdminConnectorRequests />);
    const row = await screen.findByTestId('connector-request-row-5');
    expect(row).toHaveTextContent('jira');
    expect(row).toHaveTextContent('Alice');
    expect(row).toHaveTextContent('search_issues · read');
    expect(row).toHaveTextContent('jira.example.com');
    expect(row).toHaveTextContent('internal');
    expect(queries[0]).toBe('submitted');
  });

  it('approves with an optional reason', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConnectorRequests />);
    await user.click(await screen.findByTestId('connector-request-approve-5'));
    await user.click(screen.getByTestId('connector-request-confirm'));
    await waitFor(() => expect(decisions).toHaveLength(1));
    expect(decisions[0]).toEqual({ path: '/api/v2/admin/connector_requests/5/approve', body: {} });
  });

  it('needs a reason to reject and shows the server’s refusal', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminConnectorRequests />);
    await user.click(await screen.findByTestId('connector-request-reject-5'));
    const confirm = screen.getByTestId('connector-request-confirm');
    expect(confirm).toBeDisabled();
    await user.type(screen.getByTestId('connector-request-reason'), 'Not needed');
    await user.click(confirm);
    expect(await screen.findByText('You made this request, so another approver must decide it.')).toBeInTheDocument();
    expect(decisions[0]?.body).toEqual({ reason: 'Not needed' });
  });
});
