/**
 * The sources screen's three reads, joined.
 *
 * The failures this pins are all "the screen looks right and is wrong". A
 * source whose LABEL never resolves renders as a bare id nobody recognises. A
 * `get_sources_status` read that fails must NOT empty the table — the
 * configured sources are real, and "no sources" would send a user to add ones
 * they already have. And the names listing must not be part of `isPending`: a
 * listing that is still loading costs the rows their labels, not their
 * existence, so waiting for it leaves the panel blank while the ids are known.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import type { InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useInventorySources } from './useInventorySources';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;
const LIST_ROUTE = `${BASE}/elitea_core/tools/prompt_lib/:projectId`;

const TARGET: InventoryTarget = {
  projectId: '7',
  toolkitId: '42',
  settings: {
    bucket: 'graphs',
    llm_model: 'gpt-5',
    sources: [9010, 9011],
    source_configs: { '9010': { branch: 'main', file_patterns: '**/*.py' } },
  },
};

const SOURCES_STATUS = {
  sources: [
    { source: 'github:9010', status: 'completed', entity_count: 4, relation_count: 3 },
    { source: 'docs', status: 'completed', entity_count: 2 },
  ],
};

const INGESTION_STATUS = { running: true, current_source: 'github:9011' };

/** Answer each tool's invocation with its own document, keyed by tool name. */
function serveTools(documents: Record<string, unknown>, failing: readonly string[] = []): void {
  server.use(
    http.get(LIST_ROUTE, () =>
      HttpResponse.json({
        rows: [
          { id: 9010, name: 'Checkout repo', type: 'github' },
          { id: 9011, name: 'Docs space', type: 'confluence' },
        ],
        total: 2,
      }),
    ),
    http.post(INVOKE_ROUTE, ({ params }) =>
      HttpResponse.json({ invocation_id: String(params['tool']), status: 'Started' }),
    ),
    http.get(INVOCATION_ROUTE, ({ params }) => {
      const tool = String(params['tool']);
      const body = JSON.stringify([
        {
          object_type: 'message',
          result_target: 'response',
          data: failing.includes(tool)
            ? 'The graph could not be read from bucket graphs.'
            : JSON.stringify(documents[tool] ?? {}),
        },
      ]);
      return HttpResponse.json(
        failing.includes(tool) ? { status: 'Error', result: body } : { status: 'Completed', result: body },
      );
    }),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useInventorySources', () => {
  it('joins the settings, the toolkit names and the reported statuses', async () => {
    serveTools({ get_sources_status: SOURCES_STATUS, get_ingestion_status: INGESTION_STATUS });

    const { result } = renderHookWithProviders(() => useInventorySources(TARGET));
    await waitFor(() => {
      expect(result.current.sources).toHaveLength(3);
    });

    const [checkout, docsSpace, orphan] = result.current.sources;
    // The per-source overrides come from the toolkit, the label from the
    // listing, the counts from the status document.
    expect(checkout).toMatchObject({
      toolkitId: '9010',
      name: 'Checkout repo',
      type: 'github',
      branch: 'main',
      filePatterns: '**/*.py',
      status: 'completed',
      entityCount: 4,
    });
    // Configured, never ingested — still a row, waiting.
    expect(docsSpace).toMatchObject({ toolkitId: '9011', name: 'Docs space', status: '' });
    // Reported, no longer configured — still a row, with no id to re-ingest.
    expect(orphan).toMatchObject({ toolkitId: '', name: 'docs', status: 'completed' });
  });

  it('reads the ingestion status document', async () => {
    serveTools({ get_sources_status: SOURCES_STATUS, get_ingestion_status: INGESTION_STATUS });
    const { result } = renderHookWithProviders(() => useInventorySources(TARGET));
    await waitFor(() => {
      expect(result.current.ingestion.running).toBe(true);
    });
    expect(result.current.ingestion.source).toBe('github:9011');
  });

  it('keeps the configured rows when the STATUS read fails', async () => {
    // Emptying the table here sends a user to add sources they already have.
    serveTools({ get_ingestion_status: {} }, ['get_sources_status']);

    const { result } = renderHookWithProviders(() => useInventorySources(TARGET));
    await waitFor(() => {
      expect(result.current.statusError).toBe('The graph could not be read from bucket graphs.');
    });
    expect(result.current.sources.map((source) => source.toolkitId)).toEqual(['9010', '9011']);
    expect(result.current.sources.every((source) => source.status === '')).toBe(true);
  });

  it('reads an unobtainable ingestion status as "nothing is running"', async () => {
    serveTools({ get_sources_status: SOURCES_STATUS }, ['get_ingestion_status']);
    const { result } = renderHookWithProviders(() => useInventorySources(TARGET));
    await waitFor(() => {
      expect(result.current.sources).toHaveLength(3);
    });
    expect(result.current.ingestion.running).toBe(false);
  });

  it('does not read at all while the panel is off screen', async () => {
    // An invocation is work on the engine. A hidden tab that polls is load for
    // nothing — and `isPending` must not report a read that never started.
    let invokes = 0;
    server.use(
      http.get(LIST_ROUTE, () => HttpResponse.json({ rows: [], total: 0 })),
      http.post(INVOKE_ROUTE, () => {
        invokes += 1;
        return HttpResponse.json({ invocation_id: 'x' });
      }),
    );

    const { result } = renderHookWithProviders(() =>
      useInventorySources(TARGET, { enabled: false }),
    );
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(invokes).toBe(0);
    // The configured ids are known from the settings alone, so the rows exist
    // before any read does.
    expect(result.current.sources.map((source) => source.toolkitId)).toEqual(['9010', '9011']);
  });

  it('names a source by its id while the toolkit listing is still loading', async () => {
    // The names query is deliberately outside `isPending`: waiting for it would
    // blank a panel whose ids are already known.
    server.use(
      http.get(LIST_ROUTE, () => HttpResponse.json({ rows: [], total: 0 })),
      http.post(INVOKE_ROUTE, () => HttpResponse.json({ invocation_id: 'x' })),
      http.get(INVOCATION_ROUTE, () =>
        HttpResponse.json({
          status: 'Completed',
          result: JSON.stringify([
            { object_type: 'message', result_target: 'response', data: '{}' },
          ]),
        }),
      ),
    );

    const { result } = renderHookWithProviders(() => useInventorySources(TARGET));
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(result.current.sources.map((source) => source.name)).toEqual(['9010', '9011']);
  });
});
