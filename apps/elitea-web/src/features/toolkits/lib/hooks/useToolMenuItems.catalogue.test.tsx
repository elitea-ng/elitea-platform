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

    // #865/#866: this hook now opts INTO includeHidden (the type picker
    // grid greys a hidden tile with a reason instead of omitting it), so the
    // heading set includes categories whose only member the configured
    // worker cannot build — Authentication (keycloak) and Cloud (aws) in
    // this fixture — where the pre-#865 chooser rendered neither heading at
    // all. `Communication` (slack) was ALREADY not hidden in this fixture
    // (#869 made it importable) and belongs here independent of this fix;
    // the previous version of this assertion asserting its absence was
    // itself stale by the time this test was touched for #865/#866.
    expect([...byCategory.keys()].sort()).toEqual([
      'Authentication',
      'Cloud',
      'Code Repositories',
      'Communication',
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
  });

  /*
   * keycloak (Authentication) and aws (Cloud) are each their category's only
   * member, and each is hidden — the configured worker cannot build either.
   * Confirms they are TILES (disabled, with a reason), not just headings
   * that happen to exist with nothing under them (split out of the test
   * above to stay under the file's own complexity budget).
   */
  it('renders each worker-hidden type as a disabled tile with a reason', async () => {
    serveTheRealCatalogue();

    const { box } = renderToolMenuItems({});
    await waitFor(() => expect(box.current?.isFetchingToolkitTypes).toBe(false));

    const items = box.current?.toolMenuItems ?? [];
    const keycloak = items.find((item) => item.key === 'keycloak');
    expect(keycloak?.disabled).toBe(true);
    expect(keycloak?.disabledReason).toBeTruthy();
    const aws = items.find((item) => item.key === 'aws');
    expect(aws?.disabled).toBe(true);
    expect(aws?.disabledReason).toBeTruthy();
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
