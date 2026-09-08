/**
 * The Inventory workspace — the composition root of this application.
 *
 * WHAT THIS TESTS THAT THE SLICE TESTS CANNOT. Every feature below has its own
 * tests and every one of them passes with the workspace wired wrongly: the
 * defect class this repository keeps finding is two correct halves and no
 * connection between them (a panel rendered with a prop nothing sets, a
 * selection the detail pane never receives, a citation that moves a selection
 * on a tab the user cannot see). All four assertions below are about the WIRING
 * and would pass a slice-level suite regardless:
 *
 *   1. the Sources tab reads and renders the seeded source;
 *   2. a tab that is not on screen does NOT invoke — a read here is work on
 *      someone else's engine, not a cached GET;
 *   3. choosing an entity in the list opens it in the detail pane;
 *   4. a citation clicked in the ask drawer closes the drawer, switches to the
 *      Graph tab and opens the entity — three things, of which any two can
 *      work while the third does nothing visible.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithProviders } from '../__tests__/testUtils';
import { InventoryWorkspace } from './InventoryWorkspace';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;
const TOOLKITS_ROUTE = `${BASE}/elitea_core/tools/prompt_lib/:projectId`;

const SETTINGS = {
  bucket: 'graphs',
  llm_model: 'gpt-4o-mini',
  sources: ['9110'],
  source_configs: { '9110': { branch: 'main' } },
};

const ENTITY = {
  id: 'code:checkout-service',
  name: 'CheckoutService',
  type: 'class',
  layer: 'application',
  source_toolkit: 'code',
  file_path: 'src/checkout/service.py',
};

/** The SPI envelope: a JSON string holding a list of result objects. */
function envelope(data: string): string {
  return JSON.stringify([{ object_type: 'message', result_target: 'response', data }]);
}

/** What each tool answers, as a JSON document under `output_format: 'json'`. */
const DOCUMENTS: Readonly<Record<string, unknown>> = {
  get_sources_status: { sources: [{ source: 'github:9110', status: 'completed', entity_count: 6 }] },
  get_ingestion_status: { running: false, last_status: 'completed' },
  get_stats: {
    node_count: 6,
    edge_count: 5,
    entities_by_type: { class: 3, function: 1, document: 2 },
    entities_by_layer: { application: 2, domain: 1 },
    source_toolkits: ['code', 'docs'],
  },
  get_cache_stats: { cached_graphs: 1, cache_size_bytes: 4096 },
  search_graph: { query: '', results: [ENTITY], total: 1 },
  get_entity: ENTITY,
  get_entity_neighbors: {
    entity_id: ENTITY.id,
    related: [{ entity_id: 'code:place-order', relation_type: 'defines', direction: 'outgoing' }],
  },
  investigate: {
    answer: 'CheckoutService places orders.',
    entities: ['code:checkout-service'],
  },
};

/** Every tool the workspace can reach, answered from one table. */
function serveInventory(): string[] {
  const invoked: string[] = [];
  server.use(
    http.post(INVOKE_ROUTE, ({ params }) => {
      const tool = String(params['tool']);
      invoked.push(tool);
      return HttpResponse.json({ invocation_id: tool, status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, ({ params }) => {
      const tool = String(params['tool']);
      const document = DOCUMENTS[tool] ?? {};
      return HttpResponse.json({
        invocation_id: tool,
        status: 'Completed',
        result: envelope(JSON.stringify(document)),
      });
    }),
    http.get(TOOLKITS_ROUTE, () =>
      HttpResponse.json({
        rows: [{ id: 9110, name: 'e2e-github', type: 'github' }],
        total: 1,
      }),
    ),
  );
  return invoked;
}

function renderWorkspace() {
  return renderWithProviders(
    <InventoryWorkspace
      projectId="7"
      toolkitId="42"
      settings={SETTINGS}
      toolkit={{ id: 42, name: 'E2E Inventory', type: 'inventory' }}
    />,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('InventoryWorkspace', () => {
  it('opens on Sources and renders the configured source with its reported status', async () => {
    serveInventory();
    renderWorkspace();

    const row = await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });
    // The NAME comes from the project's toolkit listing and the STATUS from
    // `get_sources_status`. Both are joined reads; a mounted-but-unfetched
    // screen produces neither.
    expect(row).toHaveTextContent('e2e-github');
    expect(within(row).getByTestId('inventory-source-status')).toHaveAttribute('data-status', 'done');
  });

  it('does NOT read the graph while the Graph tab is closed', async () => {
    const invoked = serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    // A read here is an invocation on the provider's engine. A workspace that
    // ran every panel's read on mount would do four runs of the graph to show
    // one tab.
    expect(invoked).toContain('get_sources_status');
    expect(invoked).not.toContain('search_graph');
    expect(invoked).not.toContain('get_cache_stats');
  });

  it('opens the entity a reader chooses in the list', async () => {
    serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-tab-graph'));
    const entity = await screen.findByTestId('inventory-entity-row', undefined, { timeout: 10_000 });
    await userEvent.click(entity);

    const detail = await screen.findByTestId('inventory-entity-detail', undefined, { timeout: 10_000 });
    expect(detail).toHaveTextContent('CheckoutService');
    // The NEIGHBOUR read is the second half of the detail: an entity with no
    // edges rendered is a table row with extra steps.
    expect(await screen.findByTestId('inventory-entity-relations')).toHaveTextContent('code:place-order');
  });

  it('follows a citation from the ask drawer onto the Graph tab', async () => {
    serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-open-ask'));
    const input = await screen.findByTestId('inventory-ask-input');
    await userEvent.type(input, 'What places orders?');
    await userEvent.click(screen.getByTestId('inventory-ask-send'));

    const citation = await screen.findByTestId('inventory-ask-citation', undefined, { timeout: 15_000 });
    await userEvent.click(citation);

    // Three things at once, and any two can work while the third is silent:
    // the drawer closes, the Graph tab is selected, and the entity opens.
    await waitFor(() => {
      expect(screen.queryByTestId('inventory-ask-input')).toBeNull();
    });
    const detail = await screen.findByTestId('inventory-entity-detail', undefined, { timeout: 10_000 });
    expect(detail).toHaveTextContent('CheckoutService');
  });

  it('shows the statistics and the maintenance controls on the Statistics tab', async () => {
    serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-tab-stats'));
    expect(await screen.findByTestId('inventory-stat-entities', undefined, { timeout: 10_000 })).toHaveTextContent('6');
    expect(screen.getByTestId('inventory-stats-by-type')).toHaveTextContent('class: 3');
    expect(screen.getByTestId('inventory-normalize-types')).toBeEnabled();
  });

  it('offers the project’s repository toolkits as sources to add', async () => {
    serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-add-source'));
    // 9110 is already a source, and it is the project's only repository
    // toolkit — so the dialog must say "already a source", not "create one".
    const empty = await screen.findByTestId('inventory-no-candidates');
    expect(empty).toHaveTextContent(/already a source/i);
  });
  it('starts an ingestion for the source the reader chose, and shows what it does', async () => {
    serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-run-ingestion'));

    // The BANNER is the evidence: an ingestion is minutes long, and a spinner
    // for minutes is indistinguishable from a request that was never sent.
    const banner = await screen.findByTestId('inventory-ingestion-banner', undefined, { timeout: 10_000 });
    expect(banner).toBeVisible();
  });

  it('saves a source list without the source the reader removed', async () => {
    serveInventory();
    const saved: unknown[] = [];
    server.use(
      http.put(`${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`, async ({ request }) => {
        const body = (await request.json()) as { settings?: { sources?: unknown } };
        saved.push(body.settings?.sources);
        return HttpResponse.json({ ok: true });
      }),
    );
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-remove-source'));

    // The only configured source is gone, and the PUT carries the new list —
    // not a patch, and not the old list with a flag on it.
    await waitFor(() => {
      expect(saved).toEqual([[]]);
    });
  });

  it('narrows the entity list through the provider when a query is submitted', async () => {
    const invoked = serveInventory();
    renderWorkspace();
    await screen.findByTestId('inventory-source-row', undefined, { timeout: 10_000 });

    await userEvent.click(screen.getByTestId('inventory-tab-graph'));
    await screen.findByTestId('inventory-entity-row', undefined, { timeout: 10_000 });

    invoked.length = 0;
    await userEvent.type(screen.getByTestId('inventory-search-input'), 'checkout');
    // NOT on every keystroke: each one would be an invocation on the engine.
    expect(invoked).toEqual([]);

    await userEvent.click(screen.getByTestId('inventory-search-submit'));
    await waitFor(() => {
      expect(invoked).toContain('search_graph');
    });
  });
});
