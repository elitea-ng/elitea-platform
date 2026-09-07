/**
 * Saving an Inventory toolkit's source list.
 *
 * TWO THINGS CAN GO SILENTLY WRONG HERE, and both are what these tests hold.
 *
 * The route is a PUT and REPLACES the resource, so a request that carried only
 * `settings` would clear every other field the toolkit row has — its name, its
 * description, its owner. The screen would look right and the toolkit would be
 * damaged, which is why the body is asserted field by field rather than by its
 * settings alone.
 *
 * And a removed source leaves its per-source overrides behind. The facade reads
 * `source_configs` BY ID (`mergeSourceConfig`,
 * internal/api/v2/inventory/sources.go:143-146), so the next toolkit created
 * with that id would inherit a branch and a pattern set nobody chose for it,
 * and what gets ingested would change with nothing on screen to say so.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, waitFor } from '@testing-library/react';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { SOURCE_TOOLKIT_TYPES, settingsWithSources, useSaveSources } from './inventorySourcesApi';

const BASE = 'http://elitea.test/api/v2';
const TOOLKIT_ROUTE = `${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`;

const SETTINGS = {
  bucket: 'graphs',
  llm_model: 'gpt-4o-mini',
  sources: ['9110', '9111'],
  source_configs: {
    '9110': { branch: 'main', file_patterns: '**/*.py' },
    '9111': { preset: 'typescript' },
  },
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('settingsWithSources', () => {
  it('narrows source_configs to the sources that remain', () => {
    const next = settingsWithSources(SETTINGS, ['9110']);
    expect(next['sources']).toEqual(['9110']);
    // 9111's preset is GONE. Left behind, it would be applied to whatever
    // toolkit is next created with id 9111 — a different repository ingested
    // with someone else's file patterns.
    expect(next['source_configs']).toEqual({ '9110': { branch: 'main', file_patterns: '**/*.py' } });
  });

  it('keeps every setting the caller did not name', () => {
    const next = settingsWithSources(SETTINGS, []);
    expect(next['bucket']).toBe('graphs');
    expect(next['llm_model']).toBe('gpt-4o-mini');
    expect(next['sources']).toEqual([]);
    expect(next['source_configs']).toEqual({});
  });

  it('adds a source with no overrides without inventing an empty entry', () => {
    // An empty `{}` under an id is not the same as no entry: the facade reads
    // the object and would find a config where the toolkit configured none.
    const next = settingsWithSources(SETTINGS, ['9110', '9112']);
    expect(next['source_configs']).toEqual({ '9110': { branch: 'main', file_patterns: '**/*.py' } });
  });

  it('offers exactly the four source types the descriptor advertises', () => {
    // Narrowing this list to the two the facade has field projections for
    // would hide a source the descriptor says is allowed; the facade's own
    // refusal names the reason, and a missing control names nothing.
    expect([...SOURCE_TOOLKIT_TYPES]).toEqual(['github', 'ado_repos', 'gitlab', 'bitbucket']);
  });
});

describe('useSaveSources', () => {
  it('PUTs the WHOLE toolkit, not a settings patch', async () => {
    const bodies: Record<string, unknown>[] = [];
    server.use(
      http.put(TOOLKIT_ROUTE, async ({ request }) => {
        bodies.push((await request.json()) as Record<string, unknown>);
        return HttpResponse.json({ ok: true });
      }),
    );

    const { result } = renderHookWithProviders(() => useSaveSources());
    act(() => {
      result.current.mutate({
        projectId: '7',
        toolkitId: '42',
        toolkit: { id: 42, name: 'E2E Inventory', type: 'inventory', description: 'kept' },
        settings: SETTINGS,
        sourceIds: ['9110'],
      });
    });

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    const body = bodies[0] ?? {};
    // Every field the row already had survives the save. A body of only
    // `settings` would leave a toolkit with no name and no type.
    expect(body['name']).toBe('E2E Inventory');
    expect(body['type']).toBe('inventory');
    expect(body['description']).toBe('kept');
    expect(body['settings']).toMatchObject({ sources: ['9110'], bucket: 'graphs' });
  });

  it('reports a refused save instead of leaving the screen looking saved', async () => {
    server.use(
      http.put(TOOLKIT_ROUTE, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
    );

    const { result } = renderHookWithProviders(() => useSaveSources());
    act(() => {
      result.current.mutate({
        projectId: '7',
        toolkitId: '42',
        toolkit: { id: 42 },
        settings: SETTINGS,
        sourceIds: [],
      });
    });

    await waitFor(() => {
      expect(result.current.isError).toBe(true);
    });
  });
});
