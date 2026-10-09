/**
 * Composition-root coverage for the desktop Workspace view: the REAL generated
 * route tree, the REAL app shell, the REAL sidebar. The page, the nav row and
 * the route are each unit-tested elsewhere; whether the route is reachable
 * from the shell at all is only ever true or false here (the dead-code-with-
 * no-caller class: every part correct, nothing joined to anything).
 */
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import type { AuthContext, RouterContext } from '@/app/router-context';
import { WorkspaceIpcProvider } from '@/features/workspace';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createFakeWorkspaceIpc } from '@/shared/desktop/workspaceIpc.fake';

import { routeTree } from '../../routeTree.gen';

const auth: AuthContext = {
  getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions: [], publicPermissions: [] }),
  getSelectedProjectId: () => '999',
};

const globals = globalThis as unknown as Record<string, unknown>;

function mountAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
    context: { auth } satisfies RouterContext,
  });
  const ipc = createFakeWorkspaceIpc({ workspaces: [{ id: 'w1', path: '/tmp/proj', name: 'proj', project_id: null, is_git: true }] });
  render(
    <AppProviders>
      <WorkspaceIpcProvider ipc={ipc}>
        <RouterProvider router={router} />
      </WorkspaceIpcProvider>
    </AppProviders>,
  );
  return router;
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '11' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  cleanup();
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.unstubAllEnvs();
});

describe('the Workspaces route in the real app shell', () => {
  it('desktop build: the nav shows a Workspaces row, /workspaces renders the page, and a folder opens its session route', async () => {
    vi.stubEnv('MODE', 'desktop');
    const router = mountAt('/workspaces');

    expect(await screen.findByTestId('workspaces-page')).toBeInTheDocument();
    expect(router.state.location.pathname).toBe('/workspaces');
    expect(screen.getByRole('heading', { name: 'Workspaces' })).toBeInTheDocument();
    expect(await screen.findByText('/tmp/proj')).toBeInTheDocument();
    // The sidebar row: reachable by navigation, not only by typing the URL.
    const navLink = screen.getAllByRole('link', { name: /Workspaces/ }).find((link) => link.getAttribute('href') === '/workspaces');
    expect(navLink).toBeDefined();
  });

  it('desktop build: /workspaces/$id reaches the session page', async () => {
    vi.stubEnv('MODE', 'desktop');
    mountAt('/workspaces/w1');
    // The folder has no project bound, so the session says so — the page itself rendered.
    expect(await screen.findByText('Bind a project to this folder first.')).toBeInTheDocument();
  });

  it('any other build: the route redirects to /chat and the nav has no Workspaces row', async () => {
    const router = mountAt('/workspaces');
    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
    expect(screen.queryByTestId('workspaces-page')).toBeNull();
    expect(screen.queryAllByRole('link', { name: /Workspaces/ })).toHaveLength(0);
  });
});
