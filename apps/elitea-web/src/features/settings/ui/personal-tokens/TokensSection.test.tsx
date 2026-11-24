/**
 * TokensSection.test.tsx — administration scopes on the token screen
 * (ADR-0059 R1.3b, issue 2593).
 *
 * Pinned here:
 *  1. A person without administration permissions sees no scope control
 *     (the server offers an empty list).
 *  2. A person with administration permissions sees exactly the scopes the
 *     SERVER offers, never the browser's own copy of the permission list: a
 *     `token: never` permission the person holds is still not offered.
 *  3. The desktop build (a native device session) renders the page with NO
 *     scope control: the server answers `settable: false` to a native
 *     session (ADR-0059 decision 7: scopes only from a browser sign-in).
 *  4. The token list marks an `inherit` token with its end date when the
 *     server sends one, and with no date when it does not; a `scoped` token
 *     shows its scopes.
 *
 * The create's re-sign-in round trip is pinned at the page:
 * `routes/_shell/settings/create-personal-token.reauth.test.tsx`.
 */
import { useState } from 'react';

import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { setNativeTransport } from '@/shared/api/nativeTransport';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../../../test/setup';
import { TokenAdminScopesField } from './TokenAdminScopesField';
import { TokensSection } from './TokensSection';

const BASE = '/api/v2';
const OFFER_PATH = `${BASE}/auth/token/admin_scopes`;
const TOKEN_PATH = `${BASE}/auth/token/`;
const NEVER_SCOPE = 'admin.auth.users.super_admin';

const globals = globalThis as unknown as Record<string, unknown>;

/** The caller's own permission list, as the browser holds it: it includes a `token: never` permission. */
const ADMIN_PERMISSIONS = [NEVER_SCOPE, 'admin.auth.users', 'access.roles.view'];

function ScopesHarness() {
  const [value, setValue] = useState<readonly string[]>([]);
  return (
    <TokenAdminScopesField
      value={value}
      onChange={setValue}
    />
  );
}

function mount(component: () => React.JSX.Element, permissions: string[] = []): void {
  const auth: AuthContext = {
    getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions, publicPermissions: [] }),
    getSelectedProjectId: () => '',
  };
  const rootRoute = createRootRoute();
  const route = createRoute({ getParentRoute: () => rootRoute, path: '/settings/tokens', component });
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

function offer(body: Record<string, unknown>): { authorization: (string | null)[] } {
  const seen: (string | null)[] = [];
  server.use(
    http.get(OFFER_PATH, ({ request }) => {
      seen.push(request.headers.get('authorization'));
      return HttpResponse.json(body);
    }),
  );
  return { authorization: seen };
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  setNativeTransport(undefined);
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
});

describe('the scope control', () => {
  it('is absent for a person without administration permissions', async () => {
    const { authorization } = offer({ settable: true, scopes: [] });
    mount(ScopesHarness);

    await waitFor(() => expect(authorization).toHaveLength(1));
    expect(screen.queryByTestId('token-admin-scopes')).not.toBeInTheDocument();
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument();
  });

  it('offers exactly the scopes the server offers, never a token: never permission', async () => {
    offer({
      settable: true,
      scopes: [
        { name: 'access.roles.view', area: 'people_access', class: 'read' },
        { name: 'admin.auth.users', area: 'people_access', class: 'write' },
      ],
    });
    mount(ScopesHarness, ADMIN_PERMISSIONS);

    const control = await screen.findByTestId('token-admin-scopes');
    const boxes = within(control).getAllByRole('checkbox');
    expect(boxes).toHaveLength(2);
    expect(within(control).getByLabelText('access.roles.view')).not.toBeChecked();
    expect(within(control).getByLabelText('admin.auth.users')).toBeInTheDocument();
    // Held by the person, marked token: never, and so not offered.
    expect(within(control).queryByLabelText(NEVER_SCOPE)).not.toBeInTheDocument();
    // `inherit` is never a choice.
    expect(within(control).queryByText(/inherit/i)).not.toBeInTheDocument();
  });

  it('is absent when the sign-in may set no scope', async () => {
    const { authorization } = offer({
      settable: false,
      code: 'token_scopes_sealed_session',
      scopes: [{ name: 'access.roles.view', area: 'people_access', class: 'read' }],
    });
    mount(ScopesHarness, ADMIN_PERMISSIONS);

    await waitFor(() => expect(authorization).toHaveLength(1));
    expect(screen.queryByTestId('token-admin-scopes')).not.toBeInTheDocument();
  });
});

/*
 * The desktop build is this web app inside the Tauri host, with the native
 * transport registered (`entries/desktop/launchApp.tsx`): every request
 * carries the device session's bearer. The server refuses scopes to a native
 * session, so the desktop page renders without the control. This test is the
 * desktop parity evidence for R3.14: the cell is `n-a` (decision 7), not
 * `shipped`.
 */
describe('the desktop build', () => {
  it('renders the token page with no scope control for a native device session', async () => {
    setNativeTransport({
      accessToken: vi.fn().mockResolvedValue('device-access-token'),
      refresh: vi.fn().mockResolvedValue('unavailable'),
      signOut: vi.fn(),
      origin: window.location.origin,
      fetch: (input, init) => fetch(input, init),
    });
    server.use(http.get(TOKEN_PATH, () => HttpResponse.json([])));
    const { authorization } = offer({ settable: false, code: 'token_scopes_browser_session_required', scopes: [] });
    mount(() => (
      <>
        <TokensSection search="" />
        <ScopesHarness />
      </>
    ), ADMIN_PERMISSIONS);

    expect(await screen.findByText('No tokens')).toBeInTheDocument();
    await waitFor(() => expect(authorization).toEqual(['Bearer device-access-token']));
    expect(screen.queryByTestId('token-admin-scopes')).not.toBeInTheDocument();
  });
});

describe('the token list', () => {
  const token = { uuid: 'u-1', name: 'nightly', token: '...abc1234', expires: null, project_id: null };

  it('marks an inherit token with its end date, and with no date before one is set', async () => {
    server.use(
      http.get(TOKEN_PATH, () =>
        HttpResponse.json([
          { ...token, uuid: 'u-1', name: 'dated', admin_access: 'inherit', admin_scopes: [], inherit_ends_on: '2027-03-01' },
          { ...token, uuid: 'u-2', name: 'undated', admin_access: 'inherit', admin_scopes: [] },
          { ...token, uuid: 'u-3', name: 'scoped', admin_access: 'scoped', admin_scopes: ['access.roles.view', 'llm.usage.view'] },
          { ...token, uuid: 'u-4', name: 'plain', admin_access: 'none', admin_scopes: [] },
        ]),
      ),
    );
    mount(() => <TokensSection search="" />);

    expect(await screen.findByRole('columnheader', { name: 'Administration' })).toBeInTheDocument();
    const marks = screen.getAllByTestId('token-admin-inherit').map((mark) => mark.textContent);
    expect(marks).toEqual(['All your administration permissions, until 2027-03-01', 'All your administration permissions']);
    const scoped = screen.getByTestId('token-admin-scopes-cell');
    expect(within(scoped).getByText('access.roles.view')).toBeInTheDocument();
    expect(within(scoped).getByText('llm.usage.view')).toBeInTheDocument();
    expect(screen.getByText('None')).toBeInTheDocument();
  });

  it('shows no administration column when no token has administration access', async () => {
    server.use(http.get(TOKEN_PATH, () => HttpResponse.json([{ ...token, admin_access: 'none', admin_scopes: [] }])));
    mount(() => <TokensSection search="" />);

    expect(await screen.findByText('nightly')).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: 'Administration' })).not.toBeInTheDocument();
  });
});
