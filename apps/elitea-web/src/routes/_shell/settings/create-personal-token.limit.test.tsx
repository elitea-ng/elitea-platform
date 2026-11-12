/**
 * create-personal-token.limit.test.tsx — the name limit of issue 1968 on the
 * new-token form: at most 128 characters, no silent cut, a counter, and
 * Generate stays disabled past the limit. Same minimal router harness as
 * `create-personal-token.test.tsx`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { CreatePersonalTokenPage } from '@/routes/-pages/CreatePersonalToken';

const globals = globalThis as unknown as Record<string, unknown>;

const auth: AuthContext = {
  getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions: [], publicPermissions: [] }),
  getSelectedProjectId: () => '42',
};

const rootRoute = createRootRoute();
const createTokenRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings/create-personal-token',
  component: CreatePersonalTokenPage,
});
const testRouteTree = rootRoute.addChildren([createTokenRoute]);

function mount(): void {
  const history = createMemoryHistory({ initialEntries: ['/settings/create-personal-token'] });
  const router = createRouter({ routeTree: testRouteTree, history, context: { auth } satisfies RouterContext });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useSelectedProjectStore.setState({ project: null });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  useSelectedProjectStore.setState({ project: null });
  delete globals['elitea_ui_config'];
});

describe('token name limit (issue 1968)', () => {
  it('accepts 128 characters', async () => {
    mount();
    const input = await screen.findByLabelText('Name');
    fireEvent.change(input, { target: { value: 'a'.repeat(128) } });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Generate' })).toBeEnabled());
    expect(screen.getByTestId('token-name-counter')).toHaveTextContent('0 characters left');
  });

  it('does not cut a longer name, says by how much it is over, and blocks Generate', async () => {
    mount();
    const input = (await screen.findByLabelText('Name')) as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'a'.repeat(129) } });
    // Nothing was truncated.
    expect(input.value).toHaveLength(129);
    expect(await screen.findByText('The name must be 128 characters or fewer.')).toBeInTheDocument();
    expect(screen.getByTestId('token-name-counter')).toHaveTextContent('1 characters over the limit');
    expect(screen.getByRole('button', { name: 'Generate' })).toBeDisabled();
  });
});
