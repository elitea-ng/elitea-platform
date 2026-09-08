/**
 * Resolving the toolkit both Inventory routes need before anything can render.
 *
 * A TOOLKIT THAT CANNOT BE READ IS NOT AN EMPTY GRAPH, and this is the whole
 * reason the page has an error branch of its own. Rendering the workspace for
 * an unreadable toolkit would show "No sources configured" and an empty
 * knowledge graph — a confident description of a graph whose bucket the screen
 * never learned. The user would go and add a source to a toolkit that is not
 * there.
 *
 * The settings reach the workspace under two spellings — `settings` and
 * `toolkit_config` — because a row saved by the old application carries the
 * second. A page that read only the first would hand the workspace an empty
 * configuration, and every read would go to the default bucket.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen } from '@testing-library/react';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithProviders } from '@/widgets/inventory/__tests__/testUtils';
import { InventoryToolkit } from './InventoryToolkit';

const BASE = 'http://elitea.test/api/v2';
const TOOLKIT_ROUTE = `${BASE}/elitea_core/tool/prompt_lib/:projectId/:toolkitId`;
const TOOLKITS_ROUTE = `${BASE}/elitea_core/tools/prompt_lib/:projectId`;
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

/** Enough of the provider for the workspace to mount without hanging. */
function serveProvider(): void {
  server.use(
    http.get(TOOLKITS_ROUTE, () => HttpResponse.json({ rows: [], total: 0 })),
    http.post(INVOKE_ROUTE, () => HttpResponse.json({ invocation_id: 'inv', status: 'Started' })),
    http.get(INVOCATION_ROUTE, () =>
      HttpResponse.json({
        invocation_id: 'inv',
        status: 'Completed',
        result: JSON.stringify([
          { object_type: 'message', result_target: 'response', data: JSON.stringify({ sources: [] }) },
        ]),
      }),
    ),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('InventoryToolkit', () => {
  it('reports a toolkit it could not read, and renders no workspace', async () => {
    server.use(http.get(TOOLKIT_ROUTE, () => HttpResponse.json({ error: 'not found' }, { status: 404 })));

    renderWithProviders(<InventoryToolkit projectId="7" toolkitId="9999" />);

    expect(await screen.findByTestId('inventory-toolkit-error', undefined, { timeout: 10_000 })).toBeVisible();
    // NOT an empty workspace. The two look nothing alike to a reader and
    // exactly alike to a screen that renders the workspace regardless.
    expect(screen.queryByTestId('inventory-workspace')).toBeNull();
  });

  it('renders the workspace for a toolkit it read', async () => {
    serveProvider();
    server.use(
      http.get(TOOLKIT_ROUTE, () =>
        HttpResponse.json({
          id: 42,
          name: 'E2E Inventory',
          type: 'inventory',
          settings: { bucket: 'graphs', llm_model: 'gpt-4o-mini', sources: [] },
        }),
      ),
    );

    renderWithProviders(<InventoryToolkit projectId="7" toolkitId="42" />);
    expect(await screen.findByTestId('inventory-workspace', undefined, { timeout: 10_000 })).toBeVisible();
  });

  it('reads a row saved by the old application, whose settings are under toolkit_config', async () => {
    serveProvider();
    server.use(
      http.get(TOOLKIT_ROUTE, () =>
        HttpResponse.json({
          id: 42,
          name: 'Legacy Inventory',
          type: 'inventory',
          // No `settings` at all — the shape the previous application saved.
          toolkit_config: { bucket: 'graphs', sources: [] },
        }),
      ),
    );

    renderWithProviders(<InventoryToolkit projectId="7" toolkitId="42" />);
    await screen.findByTestId('inventory-workspace', undefined, { timeout: 10_000 });
    // No model configured, so ingestion is refused and the screen says so
    // rather than offering a control that would fail inside the provider.
    expect(await screen.findByText(/configures no LLM model/i)).toBeVisible();
  });
});
