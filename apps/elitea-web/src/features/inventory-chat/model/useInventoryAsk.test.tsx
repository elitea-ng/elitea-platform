/**
 * Asking the knowledge graph a question.
 *
 * `investigate` is declared on `inventory_search` and NOWHERE else. Sending it
 * to the `inventory` family is an unknown tool, refused as invalid input, which
 * reads on screen as a broken chat rather than as the wrong address — so the
 * family in the request path is asserted here.
 *
 * The other three guarantees are all about a wait the user cannot see into. The
 * agent streams what it is doing down `custom_events`, which are READ-ONCE, so
 * a run that stops draining them leaves a spinner. The answer arrives inside
 * the SPI envelope, so an unpeeled one is a wall of escaped JSON where the
 * answer belongs. And the CITATIONS are what make the answer checkable — losing
 * them leaves the reader nothing to verify it against.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { act, waitFor } from '@testing-library/react';

import type { InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useInventoryAsk } from './useInventoryAsk';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = { projectId: '7', toolkitId: '42', settings: { bucket: 'graphs' } };

interface Invoke {
  readonly path: string;
  readonly parameters: Record<string, unknown>;
}

function serveInvoke(): Invoke[] {
  const invokes: Invoke[] = [];
  server.use(
    http.post(INVOKE_ROUTE, async ({ request }) => {
      const body = (await request.json()) as { parameters: Record<string, unknown> };
      invokes.push({ path: new URL(request.url).pathname, parameters: body.parameters });
      return HttpResponse.json({ invocation_id: 'inv-1', status: 'Started' });
    }),
  );
  return invokes;
}

function servePoll(poll: Record<string, unknown>): void {
  server.use(http.get(INVOCATION_ROUTE, () => HttpResponse.json(poll)));
}

function envelope(data: string): string {
  return JSON.stringify([{ object_type: 'message', result_target: 'response', data }]);
}

function answered(document: unknown): Record<string, unknown> {
  return { status: 'Completed', result: envelope(JSON.stringify(document)) };
}

const ANSWER = {
  answer: 'CheckoutService places orders through the order gateway.',
  entities: ['code:checkout-service', 'code:place-order', 'code:checkout-service', ''],
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useInventoryAsk', () => {
  it('sends the question to investigate on the SEARCH family', async () => {
    const invokes = serveInvoke();
    servePoll({ status: 'InProgress' });

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('  What places orders?  ');
    });
    expect(result.current.pendingQuestion).toBe('What places orders?');

    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });
    expect(invokes[0]?.path).toBe('/api/v2/inventory/tools/7/inventory_search/investigate/invoke');
    expect(invokes[0]?.parameters).toEqual({
      question: 'What places orders?',
      output_format: 'json',
    });
  });

  it('records the answer and the ids it came from', async () => {
    serveInvoke();
    servePoll(answered(ANSWER));

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });

    await waitFor(() => {
      expect(result.current.turns).toHaveLength(1);
    });
    expect(result.current.turns[0]).toEqual({
      question: 'What places orders?',
      answer: 'CheckoutService places orders through the order gateway.',
      // Duplicates and empties dropped: a citation the reader cannot open is
      // not a citation.
      entities: ['code:checkout-service', 'code:place-order'],
    });
    expect(result.current.pendingQuestion).toBeNull();
  });

  it('falls back to the peeled text when the engine answered no document', async () => {
    // A provider answering prose still answered. An empty bubble here reports a
    // working engine as broken.
    serveInvoke();
    servePoll({ status: 'Completed', result: envelope('The graph holds no order flow.') });

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.turns[0]?.answer).toBe('The graph holds no order flow.');
    });
    expect(result.current.turns[0]?.entities).toEqual([]);
  });

  it('shows what the agent said it was doing', async () => {
    serveInvoke();
    servePoll({
      ...answered(ANSWER),
      custom_events: [
        { data: { message: 'Searching the graph' } },
        { data: { message: { text: 'Following relations' } } },
        { data: { message: { unreadable: true } } },
      ],
    });

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.steps).toEqual(['Searching the graph', 'Following relations']);
    });
  });

  it('PEELS a refusal, so the banner shows a sentence and not the envelope', async () => {
    serveInvoke();
    servePoll({
      status: 'Error',
      result: envelope('This inventory has no graph to investigate.'),
      error_category: 'resource_not_found',
    });

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.error).toBe('This inventory has no graph to investigate.');
    });
    expect(result.current.turns).toEqual([]);
    expect(result.current.pendingQuestion).toBeNull();
  });

  it('reports a question that could not be started', async () => {
    server.use(http.post(INVOKE_ROUTE, () => HttpResponse.json({ status: 'Started' })));
    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.error).toMatch(/returned no invocation/);
    });
    expect(result.current.pendingQuestion).toBeNull();
  });

  it('refuses an empty question', () => {
    const invokes = serveInvoke();
    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('   ');
    });
    expect(result.current.pendingQuestion).toBeNull();
    expect(invokes).toEqual([]);
  });

  it('refuses a second question while one is running', async () => {
    // Two runs interleave their events on a screen that shows one transcript.
    const invokes = serveInvoke();
    servePoll({ status: 'InProgress' });

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('first');
    });
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });

    act(() => {
      result.current.ask('second');
    });
    expect(result.current.pendingQuestion).toBe('first');
    expect(invokes).toHaveLength(1);
  });

  it('cancels the run the user stopped, addressing the search family', async () => {
    serveInvoke();
    servePoll({ status: 'InProgress' });
    const cancels: string[] = [];
    server.use(
      http.delete(INVOCATION_ROUTE, ({ request }) => {
        cancels.push(new URL(request.url).pathname);
        return new HttpResponse(null, { status: 204 });
      }),
    );

    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.pendingQuestion).toBe('What places orders?');
    });

    act(() => {
      result.current.stop();
    });
    await waitFor(() => {
      expect(cancels).toEqual([
        '/api/v2/inventory/invocations/7/inventory_search/investigate/inv-1',
      ]);
    });
  });

  it('does nothing when stop is pressed with no question in flight', () => {
    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));
    expect(() => {
      result.current.stop();
    }).not.toThrow();
  });

  it('clears the transcript without cancelling the run', async () => {
    // Clearing is a VIEW action. Stopping an invocation the user did not ask to
    // stop would lose an answer they are still waiting for.
    serveInvoke();
    servePoll(answered(ANSWER));
    const { result } = renderHookWithProviders(() => useInventoryAsk(TARGET));

    act(() => {
      result.current.ask('What places orders?');
    });
    await waitFor(() => {
      expect(result.current.turns).toHaveLength(1);
    });

    act(() => {
      result.current.clear();
    });
    expect(result.current.turns).toEqual([]);
    expect(result.current.error).toBeNull();
  });
});
