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
import { renderAdminRoute } from './__tests__/testRouter';

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
      '/agents/all/31?project_id=12',
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
    await user.type(screen.getByTestId('credential-revoke-reason'), 'left the team');
    await user.click(screen.getByTestId('credential-revoke-confirm'));
    await waitFor(() => expect(revokes).toEqual([{ kind: 'pat', credential_id: '501', reason: 'left the team' }]));
    await waitFor(() => expect(screen.queryByTestId('credential-revoke-dialog')).not.toBeInTheDocument());
  });
});
