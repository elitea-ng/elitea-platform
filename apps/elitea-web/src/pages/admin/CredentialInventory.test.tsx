/**
 * Admin › Credential inventory (ADR-0044 decision 5, #1501): the table with
 * its stale flag and agents, the filters as they reached the server, Revoke
 * only on the kinds the platform issues (with the typed reason in the body),
 * and the flag-only kinds.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import type { CredentialInventoryCredential } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminCredentialInventory } from './CredentialInventory';
import { confirmDangerousAction } from './__tests__/dangerousDialog';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const PAT: CredentialInventoryCredential = {
  kind: 'pat',
  credential_id: '501',
  name: 'ci token',
  owner_id: 4,
  project_id: 12,
  scope: 'project 12',
  state: 'active',
  created_at: '2026-01-01T00:00:00Z',
  expires_at: null,
  last_used_at: '2026-05-01T00:00:00Z',
  first_seen_at: '2026-06-01T00:00:00Z',
  flagged_at: '2026-10-01T00:00:00Z',
  agents: [],
  stale: true,
  revocable: true,
};

const SECRET: CredentialInventoryCredential = {
  kind: 'toolkit_secret',
  credential_id: '12:aha_token',
  name: 'aha_token',
  owner_id: null,
  project_id: 12,
  scope: 'project secret',
  state: 'active',
  created_at: null,
  expires_at: null,
  last_used_at: null,
  first_seen_at: null,
  flagged_at: null,
  agents: [{ project_id: 12, application_id: 31, name: 'Refund assistant' }],
  stale: false,
  revocable: false,
};

let queries: URLSearchParams[];
let revokes: unknown[];

beforeEach(() => {
  queries = [];
  revokes = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/credential_inventory', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      return HttpResponse.json({
        total: 2,
        rows: [PAT, SECRET],
        stale_after_days: 90,
        stale_action: 'flag',
      });
    }),
    http.post('*/admin/credential_inventory/revoke', async ({ request }) => {
      revokes.push(await request.json());
      return HttpResponse.json({
        kind: 'pat',
        credential_id: '501',
        project_id: 12,
        owner_id: 4,
        revoked_at: '2026-10-29T09:00:00Z',
      });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminCredentialInventory', () => {
  it('lists credentials with the stale flag, the agents and the policy', async () => {
    renderAdminRoute(<AdminCredentialInventory />);
    const pat = await screen.findByTestId('credential-inventory-row-pat-501');
    expect(within(pat).getByTestId('credential-inventory-stale')).toBeInTheDocument();
    const secret = screen.getByTestId('credential-inventory-row-toolkit_secret-12:aha_token');
    expect(within(secret).getByRole('link', { name: 'Refund assistant' })).toHaveAttribute(
      'href',
      '/12/agents/all/31',
    );
    expect(secret).toHaveTextContent('Flag only');
    expect(screen.queryByTestId('credential-inventory-revoke-toolkit_secret-12:aha_token')).not.toBeInTheDocument();
    expect(await screen.findByText(/unused for 90 days/)).toBeInTheDocument();
  });

  it('sends the filters, and a project id that is not a positive integer is left out', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminCredentialInventory />);
    await screen.findByTestId('credential-inventory-row-pat-501');
    await user.type(screen.getByTestId('credential-inventory-filter-project'), 'x');
    await waitFor(() => expect(queries.length).toBeGreaterThan(1));
    expect(queries.at(-1)?.get('project_id')).toBeNull();
    await user.clear(screen.getByTestId('credential-inventory-filter-project'));
    await user.type(screen.getByTestId('credential-inventory-filter-project'), '12');
    await waitFor(() => expect(queries.at(-1)?.get('project_id')).toBe('12'));
    expect(queries.at(-1)?.get('limit')).toBe('100');
  });

  it('revokes a personal access token with the typed reason', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminCredentialInventory />);
    await user.click(await screen.findByTestId('credential-inventory-revoke-pat-501'));
    await confirmDangerousAction(user, 'ci token', { code: 'offboarding', text: 'left the team' });
    await waitFor(() =>
      expect(revokes).toEqual([
        { kind: 'pat', credential_id: '501', reason_code: 'offboarding', reason: 'left the team' },
      ]),
    );
    await waitFor(() => expect(screen.queryByTestId('admin-dangerous-dialog')).not.toBeInTheDocument());
  });
});

// Read-only mode (R3.4b, issue 2613): at the read level of `credential-inventory` the
// notice shows and every write is disabled, not hidden.
describe('view only', () => {
  it('disables the writes at the read level and keeps the listing', async () => {
    serveConsoleLevel('read', 'credential-inventory');
    renderAdminRoute(<AdminCredentialInventory />);
    const revoke = await screen.findByTestId('credential-inventory-revoke-pat-501');
    await findViewOnlyNotice();
    await waitFor(() => expect(revoke).toBeDisabled());
    expect(enabledWriteControls()).toEqual([]);
  });
});
