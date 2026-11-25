/**
 * The SCIM page (ADR-0059 decision 6; R3.6, issue 2537): the client
 * credentials and the group bindings on a page of their own, read-only at
 * `read`, and with the trust actions (create, rotate, delete a client)
 * read-only to a holder of the settings half.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminScim } from './Scim';
import { renderAdminRoute } from './__tests__/testRouter';
import { enabledWriteControls, findViewOnlyNotice, serveConsoleLevel } from './__tests__/viewOnly';

const GOLDEN = join(
  process.cwd(),
  '../../services/elitea-main/internal/api/v2/adminaccess/testdata/console/identity_settings.golden.json',
);

let writes = 0;

beforeEach(() => {
  writes = 0;
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/scim_clients/administration', () =>
      HttpResponse.json({
        clients: [{ id: 'c1', name: 'Entra production', auth_method: 'bearer', secret_hint: 'ab12', status: 'active' }],
        total: 1,
        scim_base_path: '/api/v2/scim/v2',
        token_endpoint_path: '/api/v2/scim/oauth/token',
        access_token_ttl_seconds: 3600,
      }),
    ),
    http.get('*/admin/scim_group_bindings/administration', () =>
      HttpResponse.json({
        bindings: [
          {
            id: '7',
            display_name: 'Platform Team',
            external_id: 'grp-1',
            project_id: 12,
            project_name: 'Platform',
            role_name: 'editor',
            members: [],
          },
        ],
        total: 1,
      }),
    ),
    http.all('*/admin/scim_*', ({ request }) => {
      if (request.method !== 'GET') writes += 1;
      return HttpResponse.json({});
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Admin › People and access › SCIM', () => {
  it('puts the clients and the group bindings on one page', async () => {
    renderAdminRoute(<AdminScim />);

    expect(await screen.findByTestId('admin-scim-page')).toBeInTheDocument();
    expect(await screen.findByText('Entra production')).toBeInTheDocument();
    expect(await screen.findByText('Platform Team')).toBeInTheDocument();
  });

  it('disables every write at read and keeps the listings', async () => {
    serveConsoleLevel('read', 'scim');
    renderAdminRoute(<AdminScim />);

    expect(await screen.findByText('Entra production')).toBeInTheDocument();
    expect(await screen.findByText('Platform Team')).toBeInTheDocument();
    await findViewOnlyNotice();
    await waitFor(() => expect(screen.getByTestId('admin-scim-clients-add')).toBeDisabled());
    expect(screen.getByTestId('admin-scim-group-bindings-add')).toBeDisabled();
    expect(enabledWriteControls()).toEqual([]);
    expect(writes).toBe(0);
  });

  // The server's own golden for a holder of identity.sso.settings.manage alone.
  it('locks create, rotate and delete for the settings half, and keeps revoke and the bindings', async () => {
    const document = JSON.parse(readFileSync(GOLDEN, 'utf8')) as Record<string, unknown>;
    server.use(http.get('*/admin/console', () => HttpResponse.json(document)));
    renderAdminRoute(<AdminScim />);

    expect(await screen.findByTestId('admin-identity-trust-notice')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-view-only')).toBeNull();
    await waitFor(() => expect(screen.getByTestId('admin-scim-clients-add')).toBeDisabled());
    expect(await screen.findByTestId('admin-scim-client-rotate-c1')).toBeDisabled();
    expect(screen.getByTestId('admin-scim-client-delete-c1')).toBeDisabled();
    expect(screen.getByTestId('admin-scim-client-revoke-c1')).toBeEnabled();
    expect(screen.getByTestId('admin-scim-group-bindings-add')).toBeEnabled();
  });

  it('opens the trust actions to a holder of the trust item', async () => {
    serveConsoleLevel('write', 'scim', 'identity-trust');
    renderAdminRoute(<AdminScim />);

    await waitFor(() => expect(screen.getByTestId('admin-scim-clients-add')).toBeEnabled());
    expect(await screen.findByTestId('admin-scim-client-rotate-c1')).toBeEnabled();
    expect(screen.queryByTestId('admin-identity-trust-notice')).toBeNull();
  });
});
