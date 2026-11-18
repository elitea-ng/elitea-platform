import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetApplicationMockHandler, getPreviewDelegationGrantMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import type { DelegationGrant, DelegationItem, DelegationPreview } from '@/shared/api/generated/model';
import { server } from '@/test/setup';

import { renderWithProviders } from '../__tests__/testUtils';

import { DelegationGrantStatus } from './DelegationGrantStatus';

const GIT: DelegationItem = { kind: 'toolkit', name: 'toolkit:12' };
const GIT_PUSH: DelegationItem = { kind: 'tool', name: 'push', toolkit: 'toolkit:12' };
const DB: DelegationItem = { kind: 'toolkit', name: 'toolkit:13' };

function grant(overrides: Partial<DelegationGrant> = {}): DelegationGrant {
  return {
    id: 5,
    caller_application_id: 42,
    callee_application_id: 7,
    edge_kind: 'agent_tool',
    callee_version_id: 100,
    state: 'pending',
    legacy: false,
    governed: false,
    origin: 'attach',
    digest: '',
    requested_digest: 'abc',
    items: [],
    requested_items: [GIT, GIT_PUSH, DB],
    omitted: [],
    created_at: '2026-11-17T10:00:00Z',
    updated_at: '2026-11-17T10:00:00Z',
    ...overrides,
  };
}

function preview(overrides: Partial<DelegationPreview> = {}): DelegationPreview {
  return {
    callee_application_id: 7,
    callee_version_id: 100,
    callee_name: 'Git specialist',
    digest: 'abc',
    items: [GIT, GIT_PUSH, DB],
    approvable: [GIT, GIT_PUSH, DB],
    withheld: [],
    closure: [],
    governed: false,
    ...overrides,
  };
}

function serve(body: DelegationPreview): { approvals: unknown[] } {
  const seen = { approvals: [] as unknown[] };
  server.use(getPreviewDelegationGrantMockHandler(body));
  server.use(
    getGetApplicationMockHandler({
      id: '7',
      name: 'Git specialist',
      description: '',
      icon: '',
      owner_id: 'user-1',
      created_at: '2026-01-01T00:00:00Z',
      versions: [],
      version_details: { id: '100', application_id: '7', name: 'base', status: 'draft', tools: [{ id: 1, tool_id: 12, name: 'Git', type: 'github' }], meta: {} },
    }),
  );
  server.use(
    http.post('*/elitea_core/delegation_grant/prompt_lib/:projectId/:applicationId/:grantId/approve', async ({ request }) => {
      const text = await request.text();
      seen.approvals.push(text === '' ? undefined : JSON.parse(text));
      return HttpResponse.json(grant({ state: 'approved' }));
    }),
  );
  server.use(http.get('*/elitea_core/delegation_grants/prompt_lib/:projectId/:applicationId', () => HttpResponse.json({ grants: [grant()] })));
  return seen;
}

function renderStatus(value: DelegationGrant, readOnly = false) {
  return renderWithProviders(
    <DelegationGrantStatus
      projectId="proj-1"
      applicationId={42}
      grant={value}
      readOnly={readOnly}
    />,
  );
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

describe('DelegationGrantStatus', () => {
  it.each([
    ['pending', grant(), 'Delegation not approved'],
    ['awaiting', grant({ governed: true, change_request_id: 9 }), 'Awaiting approval'],
    ['stale', grant({ state: 'stale' }), 'Needs re-approval'],
    ['approved', grant({ state: 'approved', items: [GIT, GIT_PUSH] }), 'Delegation approved'],
  ])('says %s on the row, with the Review delegation action', (view, value, label) => {
    renderStatus(value);

    expect(screen.getByTestId('delegation-grant-status')).toHaveAttribute('data-grant-state', view);
    expect(screen.getByText(label)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Review delegation' })).toBeEnabled();
  });

  it('approves a pending grant from the review, with what the person kept, and closes', async () => {
    const seen = serve(preview());
    renderStatus(grant());

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));
    const dialog = await screen.findByRole('dialog', { name: 'What Git specialist will be able to use' });
    expect(await within(dialog).findByRole('checkbox', { name: 'Git' })).toBeChecked();
    fireEvent.click(within(dialog).getByRole('checkbox', { name: 'Toolkit 13' }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Approve' }));

    await waitFor(() => expect(seen.approvals).toHaveLength(1));
    expect(seen.approvals[0]).toEqual({ items: [GIT, GIT_PUSH] });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('opens a governed grant read-only: it says a different approver decides and offers no Approve', async () => {
    serve(preview({ governed: true }));
    renderStatus(grant({ governed: true, change_request_id: 9 }));

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/A different approver decides this delegation/)).toBeVisible();
    expect(within(dialog).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument();
  });

  it('re-approves a stale grant against what the callee holds now', async () => {
    const seen = serve(preview());
    renderStatus(grant({ state: 'stale', items: [GIT, GIT_PUSH], requested_items: [GIT, GIT_PUSH], omitted: [], drift: { callee_version_id: 101, digest: 'd', added: [DB] } }));

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/anything new is left out until it is approved again/)).toBeVisible();
    expect(await within(dialog).findByRole('checkbox', { name: 'Toolkit 13' })).toBeChecked();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Re-approve' }));

    await waitFor(() => expect(seen.approvals).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('shows a stale grant to a read-only viewer with what was approved and no action', async () => {
    serve(preview());
    renderStatus(grant({ state: 'stale', items: [GIT, GIT_PUSH], requested_items: [GIT, GIT_PUSH], omitted: [] }), true);

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).queryByRole('checkbox', { name: 'Toolkit 13' })).not.toBeInTheDocument();
    expect(within(dialog).queryByRole('button', { name: 'Re-approve' })).not.toBeInTheDocument();
  });

  it('lets a read-only viewer look but not approve', async () => {
    serve(preview());
    renderStatus(grant(), true);

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));

    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument();
  });

  it('says so when the review cannot be loaded', async () => {
    server.use(http.get('*/elitea_core/delegation_preview/prompt_lib/:projectId/:applicationId', () => HttpResponse.json({ error: 'boom' }, { status: 500 })));
    renderStatus(grant());

    fireEvent.click(screen.getByRole('button', { name: 'Review delegation' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('The delegation could not be loaded.');
  });
});
