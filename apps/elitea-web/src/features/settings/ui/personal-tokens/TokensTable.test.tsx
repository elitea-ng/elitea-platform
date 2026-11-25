/**
 * TokensTable.test.tsx — the end date of an `inherit` token (ADR-0059
 * R3.15b, issue 2619).
 *
 * An `inherit` token ends at the later of the sunset date and 30 days after
 * its owner's notice was delivered. The server computes that date and sends
 * it as `inherit_ends_on`; the list shows it as sent and does no date
 * arithmetic of its own: two tokens with two different dates (one still at
 * the sunset date, one moved later by its delivery) show their own dates.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../../../test/setup';
import { TokensTable } from './TokensTable';

const BASE = '/api/v2';
const TOKEN_PATH = `${BASE}/auth/token/`;

const globals = globalThis as unknown as Record<string, unknown>;

function mountTable(): void {
  const auth: AuthContext = {
    getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions: [], publicPermissions: [] }),
    getSelectedProjectId: () => '',
  };
  const rootRoute = createRootRoute();
  const route = createRoute({
    getParentRoute: () => rootRoute,
    path: '/settings/tokens',
    component: () => <TokensTable search="" />,
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([route]),
    history: createMemoryHistory({ initialEntries: ['/settings/tokens'] }),
    context: { auth } satisfies RouterContext,
  });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('the end date of an inherit token', () => {
  const token = { token: '...abc1234', expires: null, project_id: null, admin_scopes: [] };

  it('shows the date the server sends for each token', async () => {
    server.use(
      http.get(TOKEN_PATH, () =>
        HttpResponse.json([
          { ...token, uuid: 'u-1', name: 'at-sunset', admin_access: 'inherit', inherit_ends_on: '2027-01-31' },
          { ...token, uuid: 'u-2', name: 'after-notice', admin_access: 'inherit', inherit_ends_on: '2027-02-09' },
          { ...token, uuid: 'u-3', name: 'plain', admin_access: 'none' },
        ]),
      ),
    );
    mountTable();

    expect(await screen.findByRole('columnheader', { name: 'Administration' })).toBeInTheDocument();
    const marks = screen.getAllByTestId('token-admin-inherit').map((mark) => mark.textContent);
    expect(marks).toEqual([
      'All your administration permissions, until Feb 9, 2027',
      'All your administration permissions, until Jan 31, 2027',
    ]);
  });
});
