import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import servedCatalogue from '@/entities/toolkit/model/__fixtures__/servedToolkitTypeCatalogue.json';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../../__tests__/testUtils';
import { useToolMenuItems } from './useToolMenuItems';
import type { UseToolMenuItemsParams, UseToolMenuItemsResult } from './useToolMenuItems';

/*
 * The chooser's GROUPING, against the catalogue the server actually serves.
 *
 * `useToolMenuItems` derives a tile's heading from `metadata.categories[0]`.
 * Until the server served metadata, every type fell through to "Other" and the
 * whole category chrome — the chips, the section headings — was decoration over
 * one group. These assertions are the twelve headings the reference deployment
 * renders, produced from the real category strings.
 *
 * The fixture is compared with the served catalogue by a Go test; see
 * entities/toolkit/model/toolMenu.catalogue.test.ts for the note.
 */

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderToolMenuItems(params: UseToolMenuItemsParams): { readonly box: { current: UseToolMenuItemsResult | undefined } } {
  const box: { current: UseToolMenuItemsResult | undefined } = { current: undefined };

  function ProbeComponent() {
    box.current = useToolMenuItems(params);
    return null;
  }

  function RootComponent() {
    return (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ProbeComponent />
      </SocketClientContext.Provider>
    );
  }

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({ component: RootComponent });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });

  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );

  return { box };
}

function serveTheRealCatalogue(): void {
  server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json(servedCatalogue)));
}

describe('useToolMenuItems over the served catalogue', () => {
  it('groups the tiles under the headings the reference deployment renders', async () => {
    serveTheRealCatalogue();

    const { box } = renderToolMenuItems({});
    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));

    const byCategory = new Map<string, string[]>();
    for (const item of box.current?.toolMenuItems ?? []) {
      byCategory.set(item.category, [...(byCategory.get(item.category) ?? []), item.key]);
    }

    expect([...byCategory.keys()].sort()).toEqual([
      'Code Repositories',
      'Development',
      'Documentation',
      'Integrations',
      'Office',
      'Other',
      'Project Management',
      'Storage',
      'Test Management',
      'Testing',
    ]);
    expect(byCategory.get('Code Repositories')?.sort()).toEqual(['ado_repos', 'bitbucket', 'github', 'gitlab', 'gitlab_org']);
    expect(byCategory.get('Documentation')?.sort()).toEqual(['ado_wiki', 'confluence']);
    expect(byCategory.get('Storage')).toEqual(['artifact']);
    expect(byCategory.get('Integrations')).toEqual(['openapi']);
    expect(byCategory.get('Test Management')?.length).toBeGreaterThanOrEqual(6);

    /*
     * "Communication" is the one reference heading this deployment does not
     * render, and its absence is deliberate rather than missing data: `slack`
     * is the only type in it, and the admitted Python worker image cannot
     * import the Slack SDK. The server serves the type hidden with that
     * reason instead of offering a tile that fails at the first tool call.
     */
    expect([...byCategory.keys()]).not.toContain('Communication');
  });

  it('carries an icon kind for every tile', async () => {
    serveTheRealCatalogue();

    const { box } = renderToolMenuItems({});
    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));

    for (const item of box.current?.toolMenuItems ?? []) {
      expect(item.iconKind).toBeTruthy();
      expect(item.label.trim()).not.toBe('');
    }
  });

  it('shows Remote MCP in the MCP chooser and no local server', async () => {
    serveTheRealCatalogue();

    const { box } = renderToolMenuItems({ isMCP: true });
    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));

    const items = box.current?.toolMenuItems ?? [];
    expect(items.map((item) => item.label)).toEqual(['Remote MCP']);
    // mcp_config carries no label, so the server withholds it. Without that
    // rule this chooser would invent an "Mcp Config" tile the reference
    // deployment has never shown, and the empty local-server state would stop
    // rendering.
    expect(items.map((item) => item.key)).not.toContain('mcp_config');
  });

  it('adds no synthetic Custom tile, because the server now serves one', async () => {
    serveTheRealCatalogue();

    const { box } = renderToolMenuItems({});
    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));

    const custom = (box.current?.toolMenuItems ?? []).filter((item) => item.key === 'custom');
    expect(custom).toHaveLength(1);
    expect(custom[0]?.label).toBe('Custom');
  });
});
