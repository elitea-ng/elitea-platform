/**
 * `/marketplace` is the page; `/elitea-catalog` and `/agents-hub` are redirect
 * sources (ADR-0051).
 *
 * Mounted through the REAL generated route tree (same harness as
 * `allRoutesSmoke.test.tsx`), because the thing under test is the route
 * wiring itself: a redirect that drops the query string still "settles to
 * idle", and the smoke test would call that a pass. The shared `?agentId=`
 * deep link is the one link shape that reaches the old paths in the wild, so
 * losing it is the whole defect.
 */
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { cleanup, render, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { resetConfigForTests } from '@/shared/config/get-config';

import { routeTree } from '../../routeTree.gen';

const permissiveAuth: AuthContext = {
  getUser: () => ({
    id: 'u1',
    personal_project_id: 'p1',
    permissions: [],
    publicPermissions: [],
  }),
  getSelectedProjectId: () => '999',
};

async function loadAt(path: string) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
    context: { auth: permissiveAuth } satisfies RouterContext,
  });
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  await waitFor(() => {
    expect(router.state.status).toBe('idle');
  });
  return router;
}

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
});

afterEach(() => {
  cleanup();
  vi.unstubAllEnvs();
  resetConfigForTests();
});

describe('/marketplace', () => {
  it('mounts the Marketplace page', async () => {
    const router = await loadAt('/marketplace');
    expect(router.state.location.pathname).toBe('/marketplace');
    const errored = (router.state.matches as { status?: string }[]).find((match) => match.status === 'error');
    expect(errored).toBeUndefined();
  });

  it('keeps ?kind=skills through validation', async () => {
    const router = await loadAt('/marketplace?kind=skills');
    expect((router.state.location.search as { kind?: string }).kind).toBe('skills');
  });

  it('mounts a listing page at /marketplace/:kind/:listingId', async () => {
    const router = await loadAt('/marketplace/skills/42');
    expect(router.state.location.pathname).toBe('/marketplace/skills/42');
    expect(router.state.matches.at(-1)?.params).toEqual({ kind: 'skills', listingId: '42' });
  });
});

describe.each(['/elitea-catalog', '/agents-hub'])('%s redirects to the Marketplace', (legacy) => {
  it('lands on /marketplace with no params', async () => {
    const router = await loadAt(legacy);
    expect(router.state.location.pathname).toBe('/marketplace');
  });

  it('turns ?tab=skills into ?kind=skills', async () => {
    const router = await loadAt(`${legacy}?tab=skills`);
    // Both halves asserted: a redirect that kept the old path with the new key
    // would satisfy the search assertion on its own.
    expect(router.state.location.pathname).toBe('/marketplace');
    expect((router.state.location.search as { kind?: string }).kind).toBe('skills');
  });

  it('degrades an unknown ?tab= to the agents', async () => {
    const router = await loadAt(`${legacy}?tab=nonsense`);
    expect(router.state.location.pathname).toBe('/marketplace');
    expect((router.state.location.search as { kind?: string }).kind).toBeUndefined();
  });

  it('turns the shared ?agentId= deep link into the agent listing page', async () => {
    const router = await loadAt(`${legacy}?agentId=app-1`);
    expect(router.state.location.pathname).toBe('/marketplace/agents/app-1');
  });

  it('keeps the kind and the id together: ?tab=skills&agentId= opens the skill listing', async () => {
    const router = await loadAt(`${legacy}?tab=skills&agentId=7`);
    expect(router.state.location.pathname).toBe('/marketplace/skills/7');
  });

  it('keeps the legacy tab=agents deep link shape (?tab=agents&agentId=)', async () => {
    const router = await loadAt(`${legacy}?tab=agents&agentId=42`);
    expect(router.state.location.pathname).toBe('/marketplace/agents/42');
  });
});
