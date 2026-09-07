/*
 * `useIndexesTabState` — the hook `EditToolkit` reads to decide whether the
 * Indexes tab exists, which tab is active, and what the index runner is handed.
 *
 * It had no test. Its three decisions are each the kind that fails silently:
 *
 *  - `activeTab` collapses to Configuration when the tab is hidden. Without
 *    that, deselecting the last index tool while standing ON the Indexes tab
 *    leaves the page rendering tab index 1 of a one-tab strip — a blank panel.
 *  - `toolkitValues` reads the LIVE editor state with the saved row as the
 *    fallback. Reading only the saved row would hand the index runner the tool
 *    selection the user had before this edit, which is the failure the
 *    baseline's shared Formik context did not have.
 *  - `hidden` is delegated to `useIndexesTabVisibility`, and an MCP screen must
 *    never offer the tab whatever the type's schema says.
 */
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { createTestQueryClient } from '@/features/toolkits/__tests__/testUtils';
import { server } from '@/test/setup';

import { useIndexesTabState } from './useIndexesTabState';
import type { IndexesTabState, UseIndexesTabStateParams } from './useIndexesTabState';

/** A catalogue with one indexing type and one that offers no index tool. */
const CATALOGUE = {
  artifact: {
    type: 'object',
    properties: { selected_tools: { type: 'object', args_schemas: { index_data: {}, search_data: {} } } },
  },
  github: {
    type: 'object',
    properties: { selected_tools: { type: 'object', args_schemas: { get_issue: {} } } },
  },
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json(CATALOGUE)),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function renderIndexesTabState(params: UseIndexesTabStateParams): { current: IndexesTabState | undefined } {
  const box: { current: IndexesTabState | undefined } = { current: undefined };

  function Probe() {
    box.current = useIndexesTabState(params);
    return null;
  }

  const rootRoute = createRootRoute({
    component: () => (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <Probe />
      </SocketClientContext.Provider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });

  render(
    <QueryClientProvider client={createTestQueryClient()}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return box;
}

describe('useIndexesTabState', () => {
  it('offers the tab for a type whose schema carries an index tool', async () => {
    const state = renderIndexesTabState({
      isMCP: false,
      detail: { type: 'artifact', settings: { selected_tools: ['index_data'] } },
      editToolDetail: { name: 'a', description: '', settings: { selected_tools: ['index_data'] }, type: 'artifact' },
      tab: 1,
    });

    await waitFor(() => expect(state.current?.hidden).toBe(false));
    expect(state.current?.selectedIndexTools).toEqual(['index_data']);
    // The caller asked for tab 1 and the tab exists, so it stays on 1.
    expect(state.current?.activeTab).toBe(1);
  });

  it('collapses the active tab to Configuration when the type offers no index tool', async () => {
    const state = renderIndexesTabState({
      isMCP: false,
      detail: { type: 'github', settings: {} },
      editToolDetail: { name: 'g', description: '', settings: { selected_tools: ['get_issue'] }, type: 'github' },
      tab: 1,
    });

    await waitFor(() => expect(state.current?.hidden).toBe(true));
    // The regression this guards: tab 1 of a one-tab strip renders nothing.
    expect(state.current?.activeTab).toBe(0);
    expect(state.current?.selectedIndexTools).toEqual([]);
  });

  it('hides the tab on an MCP screen even for an index-capable type', async () => {
    const state = renderIndexesTabState({
      isMCP: true,
      detail: { type: 'artifact', settings: { selected_tools: ['index_data'] } },
      editToolDetail: { name: 'a', description: '', settings: { selected_tools: ['index_data'] }, type: 'artifact' },
      tab: 1,
    });

    await waitFor(() => expect(state.current).toBeDefined());
    expect(state.current?.hidden).toBe(true);
    expect(state.current?.activeTab).toBe(0);
    // The SELECTION is still reported: the tab is hidden by the route, not by
    // the toolkit having nothing to index.
    expect(state.current?.selectedIndexTools).toEqual(['index_data']);
  });

  it('hands the runner the LIVE editor state, not the saved row', async () => {
    const state = renderIndexesTabState({
      isMCP: false,
      detail: { type: 'artifact', settings: { selected_tools: [] } },
      editToolDetail: {
        name: 'a',
        description: '',
        settings: { selected_tools: ['index_data'], bucket: 'edited' },
        type: 'artifact',
      },
      tab: 0,
    });

    await waitFor(() => expect(state.current?.toolkitValues).toBeDefined());
    expect(state.current?.toolkitValues).toEqual({
      type: 'artifact',
      settings: { selected_tools: ['index_data'], bucket: 'edited' },
    });
  });

  it('falls back to the saved row while the editor state is still null', async () => {
    const state = renderIndexesTabState({
      isMCP: false,
      detail: { type: 'artifact', settings: { selected_tools: ['index_data'] } },
      editToolDetail: null,
      tab: 0,
    });

    await waitFor(() => expect(state.current?.toolkitValues).toBeDefined());
    expect(state.current?.toolkitValues).toEqual({
      type: 'artifact',
      settings: { selected_tools: ['index_data'] },
    });
  });
});
