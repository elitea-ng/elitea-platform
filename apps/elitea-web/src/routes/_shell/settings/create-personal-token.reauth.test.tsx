/**
 * create-personal-token.reauth.test.tsx — administration scopes need a recent
 * sign-in (ADR-0059 R1.3b, issue 2593).
 *
 * A create with scopes that answers 401 `reauthentication_required` must:
 *  1. NOT open the silent re-auth popup (the session is valid, only too old
 *     for this action; the popup would restore the same session);
 *  2. start the sign-in the server named, with its limit, returning to this
 *     page: `/auth/oidc/login?max_age=…`, `/auth/saml/login?max_age=…` or
 *     `/auth/form/reauthenticate`;
 *  3. keep what the person typed: back on the page, the name, the expiry and
 *     the chosen scopes are restored, and the next Generate sends them;
 *  4. say so, rather than loop, when the session has no browser sign-in.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { server } from '../../../test/setup';
import { CreatePersonalTokenPage } from '@/routes/-pages/CreatePersonalToken';

const BASE = '/api/v2';
const PAGE = '/settings/create-personal-token';
const TOKEN_PATH = `${BASE}/auth/token/`;
const OFFER_PATH = `${BASE}/auth/token/admin_scopes`;

const globals = globalThis as unknown as Record<string, unknown>;
const restorers: (() => void)[] = [];

const auth: AuthContext = {
  getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions: ['admin.auth.users'], publicPermissions: [] }),
  getSelectedProjectId: () => '',
};

function mount(): void {
  const rootRoute = createRootRoute();
  const route = createRoute({ getParentRoute: () => rootRoute, path: PAGE, component: CreatePersonalTokenPage });
  const router = createRouter({
    routeTree: rootRoute.addChildren([route]),
    history: createMemoryHistory({ initialEntries: [PAGE] }),
    context: { auth } satisfies RouterContext,
  });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

/** jsdom's `location.assign` is not configurable: replace `location` itself, as `App.redirect.test.tsx` does. */
function stubLocation(): string[] {
  const sink: string[] = [];
  const real = window.location;
  Object.defineProperty(window, 'location', {
    configurable: true,
    writable: true,
    value: Object.assign(Object.create(Object.getPrototypeOf(real) as object), real, {
      pathname: PAGE,
      search: '',
      assign: (url: string | URL) => void sink.push(String(url)),
    }),
  });
  restorers.push(() => Object.defineProperty(window, 'location', { configurable: true, writable: true, value: real }));
  return sink;
}

function offerScopes(): void {
  server.use(
    http.get(OFFER_PATH, () =>
      HttpResponse.json({
        settable: true,
        scopes: [
          { name: 'access.roles.view', area: 'people_access', class: 'read' },
          { name: 'admin.auth.users', area: 'people_access', class: 'write' },
        ],
      }),
    ),
  );
}

const STALE = {
  error: { type: 'authentication_error', code: 'reauthentication_required', message: 'this action needs a recent sign-in' },
};

function staleCreate(method: string | null): { bodies: Record<string, unknown>[] } {
  const bodies: Record<string, unknown>[] = [];
  server.use(
    http.post(TOKEN_PATH, async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json(STALE, {
        status: 401,
        headers: { 'X-Elitea-Reauthentication-Max-Age': '900', ...(method === null ? {} : { 'X-Elitea-Reauthentication-Method': method }) },
      });
    }),
  );
  return { bodies };
}

async function fillAndGenerate(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.type(await screen.findByLabelText('Name'), 'nightly-sync');
  await user.click(await screen.findByLabelText('access.roles.view'));
  const generate = screen.getByRole('button', { name: 'Generate' });
  await waitFor(() => expect(generate).toBeEnabled());
  await user.click(generate);
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: null });
  window.sessionStorage.clear();
});

afterEach(() => {
  while (restorers.length > 0) restorers.pop()?.();
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  window.sessionStorage.clear();
});

describe('a create that needs a recent sign-in', () => {
  it('starts the OIDC sign-in with the limit, and restores the form on the way back', async () => {
    offerScopes();
    const { bodies } = staleCreate('oidc');
    const reauthenticate = vi.fn().mockResolvedValue(undefined);
    resetGeneratedClient();
    configureGeneratedClient({ baseUrl: BASE, reauthenticate });
    const visits = stubLocation();
    mount();

    await fillAndGenerate(userEvent.setup());

    await waitFor(() => expect(visits).toEqual([`/auth/oidc/login?max_age=900&target_to=${encodeURIComponent(PAGE)}`]));
    expect(bodies).toEqual([expect.objectContaining({ admin_access: 'scoped', admin_scopes: ['access.roles.view'] })]);
    expect(reauthenticate).not.toHaveBeenCalled();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();

    // Back from the sign-in: a new document mounts the page again.
    cleanup();
    const created: Record<string, unknown>[] = [];
    server.use(
      http.post(TOKEN_PATH, async ({ request }) => {
        created.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ uuid: 'tok', name: 'nightly-sync', token: 'secret', expires: null, project_id: null });
      }),
    );
    mount();

    expect(await screen.findByLabelText('Name')).toHaveValue('nightly-sync');
    expect(await screen.findByLabelText('access.roles.view')).toBeChecked();
    expect(screen.getByLabelText('admin.auth.users')).not.toBeChecked();
    const user = userEvent.setup();
    const generate = screen.getByRole('button', { name: 'Generate' });
    await waitFor(() => expect(generate).toBeEnabled());
    await user.click(generate);
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0]).toMatchObject({ name: 'nightly-sync', admin_access: 'scoped', admin_scopes: ['access.roles.view'] });

    // The draft is read once: a later visit starts empty.
    cleanup();
    mount();
    expect(await screen.findByLabelText('Name')).toHaveValue('');
  });

  it.each([
    ['saml', `/auth/saml/login?max_age=900&target_to=${encodeURIComponent(PAGE)}`],
    ['form', `/auth/form/reauthenticate?target_to=${encodeURIComponent(PAGE)}`],
  ])('starts the %s sign-in the server named', async (method, url) => {
    offerScopes();
    staleCreate(method);
    const visits = stubLocation();
    mount();

    await fillAndGenerate(userEvent.setup());

    await waitFor(() => expect(visits).toEqual([url]));
  });

  it('says so, and navigates nowhere, when the session has no browser sign-in to renew', async () => {
    offerScopes();
    staleCreate(null);
    const visits = stubLocation();
    mount();

    await fillAndGenerate(userEvent.setup());

    expect(await screen.findByRole('alert')).toHaveTextContent('Administration permissions need a recent sign-in.');
    expect(visits).toEqual([]);
  });

  it('names the refused scopes when the server refuses them', async () => {
    offerScopes();
    server.use(
      http.post(TOKEN_PATH, () =>
        HttpResponse.json(
          { error: { type: 'permission_error', code: 'token_scope_not_held', message: 'no', scopes: ['access.roles.view'] } },
          { status: 403 },
        ),
      ),
    );
    mount();

    await fillAndGenerate(userEvent.setup());

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'This token cannot have these administration permissions: access.roles.view.',
    );
  });
});
