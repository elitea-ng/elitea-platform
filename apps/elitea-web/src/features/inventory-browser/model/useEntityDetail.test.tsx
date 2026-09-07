/**
 * One entity, and what it is joined to.
 *
 * The two tools this reads declare DIFFERENT argument names for the same value
 * — `entity_name` for `get_entity`, `entity_id` for `get_entity_neighbors` —
 * and the fixture runner reads a third set. Sending only one spelling gets a
 * `resource_not_found` from a graph that holds the entity, which reads on
 * screen as "this entity does not exist" on the row the user just clicked. So
 * every spelling goes in every body, and that is what these tests pin.
 *
 * The other rule is that the NEIGHBOUR read failing must not hide the entity:
 * knowing what something is, without knowing what it touches, is still an
 * answer.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { waitFor } from '@testing-library/react';

import type { InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useEntityDetail } from './useEntityDetail';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = { projectId: '7', toolkitId: '42', settings: { bucket: 'graphs' } };

const ENTITY = {
  id: 'code:checkout-service',
  name: 'CheckoutService',
  type: 'class',
  layer: 'application',
  source_toolkit: 'code',
  file_path: 'src/checkout/service.py',
};

const NEIGHBOURS = {
  entity_id: 'code:checkout-service',
  related: [
    { entity_id: 'code:place-order', relation_type: 'defines', direction: 'outgoing' },
    { entity_id: 'docs:checkout', relation_type: 'documents', direction: 'incoming' },
  ],
  total: 2,
};

/** Answer each tool with its own document; `failing` refuses instead. */
function serveTools(
  documents: Record<string, unknown>,
  failing: readonly string[] = [],
): Record<string, Record<string, unknown>> {
  const bodies: Record<string, Record<string, unknown>> = {};
  server.use(
    http.post(INVOKE_ROUTE, async ({ params, request }) => {
      const tool = String(params['tool']);
      const body = (await request.json()) as { parameters: Record<string, unknown> };
      bodies[tool] = body.parameters;
      return HttpResponse.json({ invocation_id: tool, status: 'Started' });
    }),
    http.get(INVOCATION_ROUTE, ({ params }) => {
      const tool = String(params['tool']);
      const refused = failing.includes(tool);
      const result = JSON.stringify([
        {
          object_type: 'message',
          result_target: 'response',
          data: refused
            ? `Entity 'code:checkout-service' is not in this graph.`
            : JSON.stringify(documents[tool] ?? {}),
        },
      ]);
      return HttpResponse.json(refused ? { status: 'Error', result } : { status: 'Completed', result });
    }),
  );
  return bodies;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useEntityDetail', () => {
  it('reads the entity and its edges', async () => {
    serveTools({ get_entity: ENTITY, get_entity_neighbors: NEIGHBOURS });
    const { result } = renderHookWithProviders(() =>
      useEntityDetail(TARGET, 'code:checkout-service'),
    );

    await waitFor(() => {
      expect(result.current.entity?.name).toBe('CheckoutService');
    });
    await waitFor(() => {
      expect(result.current.neighbours).toHaveLength(2);
    });
    expect(result.current.neighbours[0]).toEqual({
      entityId: 'code:place-order',
      relationType: 'defines',
      direction: 'outgoing',
    });
  });

  it('sends EVERY spelling of "which entity" to both tools', async () => {
    // The two tools declare different keys for the same value; a body carrying
    // all of them is refused by neither.
    const bodies = serveTools({ get_entity: ENTITY, get_entity_neighbors: NEIGHBOURS });
    renderHookWithProviders(() => useEntityDetail(TARGET, 'code:checkout-service'));

    await waitFor(() => {
      expect(bodies['get_entity_neighbors']).toBeDefined();
    });
    expect(bodies['get_entity']).toEqual({
      output_format: 'json',
      entity_id: 'code:checkout-service',
      entity_name: 'code:checkout-service',
      entity: 'code:checkout-service',
      id: 'code:checkout-service',
      include_relations: true,
    });
    expect(bodies['get_entity_neighbors']).toMatchObject({
      entity_id: 'code:checkout-service',
      entity_name: 'code:checkout-service',
      depth: 1,
    });
  });

  it('still describes the entity when the NEIGHBOUR read fails', async () => {
    serveTools({ get_entity: ENTITY }, ['get_entity_neighbors']);
    const { result } = renderHookWithProviders(() =>
      useEntityDetail(TARGET, 'code:checkout-service'),
    );
    await waitFor(() => {
      expect(result.current.entity?.name).toBe('CheckoutService');
    });
    expect(result.current.neighbours).toEqual([]);
    expect(result.current.error).toBeNull();
  });

  it('reports the provider sentence when the entity itself cannot be read', async () => {
    serveTools({ get_entity_neighbors: NEIGHBOURS }, ['get_entity']);
    const { result } = renderHookWithProviders(() =>
      useEntityDetail(TARGET, 'code:checkout-service'),
    );
    await waitFor(() => {
      expect(result.current.error).toBe("Entity 'code:checkout-service' is not in this graph.");
    });
    expect(result.current.entity).toBeUndefined();
  });

  it('reads nothing until an entity is selected', async () => {
    // Enabled with no id, both tools are invoked for the empty string, and the
    // pane shows a refusal before the user has clicked anything.
    let invokes = 0;
    server.use(
      http.post(INVOKE_ROUTE, () => {
        invokes += 1;
        return HttpResponse.json({ invocation_id: 'x' });
      }),
    );

    const { result, rerender } = renderHookWithProviders(
      ({ id }: { id: string | null }) => useEntityDetail(TARGET, id),
      undefined,
      { initialProps: { id: null as string | null } },
    );
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(invokes).toBe(0);

    rerender({ id: '' });
    await waitFor(() => {
      expect(result.current.isPending).toBe(false);
    });
    expect(invokes).toBe(0);
  });
});
