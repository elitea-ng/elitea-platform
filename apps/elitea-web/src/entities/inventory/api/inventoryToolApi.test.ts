/**
 * The Inventory facade's three routes and the invoke → poll loop over them.
 *
 * Four things break silently if these tests go. An invoke that drops
 * `configuration.parameters` reaches the provider without `bucket` or
 * `llm_model`, so it reads the DEFAULT bucket and refuses to ingest — with a
 * 200 in the network tab. An id taken off the transport envelope rather than
 * the body is `undefined`, and the loop then polls `undefined` for ever
 * (issue #132's shape). A refusal returned as an empty document renders "this
 * graph holds nothing" for a mistyped entity id. And a loop with no deadline
 * is a spinner that never stops.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import {
  cancelInventoryTool,
  INVENTORY_FAMILY,
  INVENTORY_SEARCH_FAMILY,
  pollInventoryTool,
  runInventoryTool,
  startInventoryTool,
  type InventoryTarget,
} from './inventoryToolApi';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = {
  projectId: '7',
  toolkitId: '42',
  settings: { bucket: 'graphs', llm_model: 'gpt-5' },
};

/** One SPI result list, as the facade marshals a terminal body. */
function envelope(...objects: readonly Record<string, unknown>[]): string {
  return JSON.stringify(objects);
}

function message(data: string): Record<string, unknown> {
  return { object_type: 'message', result_target: 'response', result_encoding: 'plain', data };
}

/** Accept every invoke with one id. */
function serveInvoke(invocationId = 'abc'): { readonly urls: string[]; readonly bodies: unknown[] } {
  const urls: string[] = [];
  const bodies: unknown[] = [];
  server.use(
    http.post(INVOKE_ROUTE, async ({ request }) => {
      urls.push(new URL(request.url).pathname);
      bodies.push(await request.json());
      return HttpResponse.json({ invocation_id: invocationId, status: 'Started' });
    }),
  );
  return { urls, bodies };
}

/** Answer the polls in order, repeating the last one for ever. */
function servePolls(...polls: readonly Record<string, unknown>[]): { readonly count: () => number } {
  let index = 0;
  server.use(
    http.get(INVOCATION_ROUTE, () => {
      const poll = polls[Math.min(index, polls.length - 1)];
      index += 1;
      return HttpResponse.json(poll);
    }),
  );
  return { count: () => index };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('the invocation envelope', () => {
  // `buildInventoryRequest` is module-private, so it is pinned where it is
  // observable: on the wire. That is also where it fails — the provider is the
  // only reader of these two fields.
  it('sends the TOOLKIT settings and the TOOL arguments in their own fields', async () => {
    const sent = serveInvoke();
    await startInventoryTool(TARGET, INVENTORY_FAMILY, 'search_graph', {
      query: 'checkout',
      top_k: 200,
    });
    expect(sent.bodies[0]).toEqual({
      configuration: { parameters: { bucket: 'graphs', llm_model: 'gpt-5' } },
      parameters: { query: 'checkout', top_k: 200 },
    });
  });
});

describe('startInventoryTool', () => {
  it('posts the envelope to the facade path and answers the invocation id', async () => {
    const sent = serveInvoke('inv-1');
    await expect(startInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {})).resolves.toBe('inv-1');
    expect(sent.urls).toEqual(['/api/v2/inventory/tools/7/inventory/get_stats/invoke']);
    expect(sent.bodies[0]).toEqual({
      configuration: { parameters: { bucket: 'graphs', llm_model: 'gpt-5' } },
      parameters: {},
    });
  });

  it('addresses the read-only family when it is the one asked for', () => {
    const sent = serveInvoke();
    return startInventoryTool(TARGET, INVENTORY_SEARCH_FAMILY, 'investigate', {}).then(() => {
      expect(sent.urls[0]).toBe('/api/v2/inventory/tools/7/inventory_search/investigate/invoke');
    });
  });

  it('refuses an acceptance with no invocation to follow', async () => {
    // Reading the id off the ENVELOPE gives undefined on a 200, and the loop
    // then polls `/invocations/…/undefined` for ever.
    server.use(http.post(INVOKE_ROUTE, () => HttpResponse.json({ status: 'Started' })));
    await expect(startInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {})).rejects.toThrow(
      /returned no invocation/,
    );
  });
});

describe('pollInventoryTool', () => {
  it('reads the poll off the transport envelope', async () => {
    servePolls({ invocation_id: 'abc', status: 'InProgress' });
    const poll = await pollInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', 'abc');
    expect(poll?.status).toBe('InProgress');
  });
});

describe('cancelInventoryTool', () => {
  it('deletes the invocation, which is the only way to stop a long run', async () => {
    const seen: string[] = [];
    server.use(
      http.delete(INVOCATION_ROUTE, ({ request }) => {
        seen.push(new URL(request.url).pathname);
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await expect(
      cancelInventoryTool(TARGET, INVENTORY_FAMILY, 'run_ingestion', 'inv-1'),
    ).resolves.toBeUndefined();
    expect(seen).toEqual(['/api/v2/inventory/invocations/7/inventory/run_ingestion/inv-1']);
  });
});

describe('runInventoryTool', () => {
  it('answers the document, the text and the poll of a finished read', async () => {
    serveInvoke();
    servePolls({
      invocation_id: 'abc',
      status: 'Completed',
      result: envelope(message('{"node_count":6,"edge_count":5}')),
      result_type: 'String',
    });

    const run = await runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {});
    expect(run.document).toEqual({ node_count: 6, edge_count: 5 });
    expect(run.text).toBe('{"node_count":6,"edge_count":5}');
    expect(run.poll.status).toBe('Completed');
  });

  it('keeps polling until a terminal status', async () => {
    serveInvoke();
    const polls = servePolls(
      { status: 'Started' },
      { status: 'InProgress', custom_events: [{ data: { message: 'Extracting entities' } }] },
      { status: 'Completed', result: envelope(message('{"total":1}')) },
    );

    const run = await runInventoryTool(TARGET, INVENTORY_FAMILY, 'search_graph', {}, { intervalMs: 1 });
    expect(polls.count()).toBe(3);
    expect(run.document).toEqual({ total: 1 });
  });

  it('throws the provider PEELED sentence for a refusal', async () => {
    // A refusal carries the same result envelope a success does. Throwing it
    // unpeeled puts the JSON array in the screen's error banner.
    serveInvoke();
    servePolls({
      status: 'Error',
      result: envelope(message("Entity 'code:nope' is not in this graph.")),
      error_category: 'resource_not_found',
      error_type: 'ToolException',
    });

    await expect(
      runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_entity', { entity_id: 'code:nope' }),
    ).rejects.toThrow("Entity 'code:nope' is not in this graph.");
  });

  it('treats a stopped run as a failure and not as an empty answer', async () => {
    serveInvoke();
    servePolls({ status: 'Stopped' });
    await expect(runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {})).rejects.toThrow(
      "Inventory could not run 'get_stats'.",
    );
  });

  it('gives up on its own deadline rather than hanging for ever', async () => {
    serveInvoke();
    servePolls({ status: 'InProgress' });
    await expect(
      runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}, { timeoutMs: 0 }),
    ).rejects.toThrow("Inventory did not finish 'get_stats' in time.");
  });

  it('stops at once when the caller has already aborted', async () => {
    // react-query passes its own signal; a query cancelled while a poll is in
    // flight must not sleep and poll again.
    serveInvoke();
    const controller = new AbortController();
    server.use(
      http.get(INVOCATION_ROUTE, () => {
        controller.abort();
        return HttpResponse.json({ status: 'InProgress' });
      }),
    );
    await expect(
      runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}, { signal: controller.signal }),
    ).rejects.toThrow('The Inventory request was cancelled.');
  });

  it('stops while it is waiting between polls', async () => {
    serveInvoke();
    servePolls({ status: 'InProgress' });
    const controller = new AbortController();
    const running = runInventoryTool(TARGET, INVENTORY_FAMILY, 'get_stats', {}, {
      signal: controller.signal,
      intervalMs: 5_000,
    });
    setTimeout(() => {
      controller.abort();
    }, 20);
    await expect(running).rejects.toThrow('The Inventory request was cancelled.');
  });
});
