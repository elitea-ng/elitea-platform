/**
 * Running one ingestion, and showing what it is doing while it runs.
 *
 * An ingestion is MINUTES long, so every guarantee here is about a wait the
 * user cannot otherwise see into. The progress lines arrive in `custom_events`,
 * which are READ-ONCE — the poll that carries a line is the only poll that ever
 * will — so a run that stops accumulating them leaves a spinner that is
 * indistinguishable from a request never sent. The terminal body is the SPI
 * envelope, not a sentence, so an unpeeled summary is a wall of escaped JSON.
 * The artifacts name the resumability checkpoint, which is reported nowhere
 * else a screen can reach. And `onSettled` must fire exactly once per run: it
 * is what invalidates the graph reads, and firing it twice refetches every
 * panel a second time.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, waitFor } from '@testing-library/react';

import type { InventoryTarget } from '@/entities/inventory';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useIngestionRun } from './useIngestionRun';

const BASE = 'http://elitea.test/api/v2';
const INVOKE_ROUTE = `${BASE}/inventory/tools/:projectId/:family/:tool/invoke`;
const INVOCATION_ROUTE = `${BASE}/inventory/invocations/:projectId/:family/:tool/:invocationId`;

const TARGET: InventoryTarget = {
  projectId: '7',
  toolkitId: '42',
  settings: { bucket: 'graphs', llm_model: 'gpt-5' },
};

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

/** Answer the polls in order, repeating the last one for ever. */
function servePolls(...polls: readonly Record<string, unknown>[]): void {
  let index = 0;
  server.use(
    http.get(INVOCATION_ROUTE, () => {
      const poll = polls[Math.min(index, polls.length - 1)];
      index += 1;
      return HttpResponse.json(poll);
    }),
  );
}

function envelope(...objects: readonly Record<string, unknown>[]): string {
  return JSON.stringify(objects);
}

const COMPLETED = {
  status: 'Completed',
  result: envelope(
    {
      object_type: 'message',
      result_target: 'response',
      data: 'Ingested 4 entities and 3 relations from github:9010.',
    },
    { name: 'graph.json', object_type: 'knowledge_graph', result_target: 'artifact' },
    {
      name: '.ingestion-checkpoint-9010.json',
      object_type: 'checkpoint',
      result_target: 'artifact',
    },
  ),
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

describe('useIngestionRun', () => {
  it('starts run_ingestion for one source and reports it as running', async () => {
    const invokes = serveInvoke();
    servePolls({ status: 'InProgress' });

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', false);
    });

    expect(result.current.runningSourceId).toBe('9010');
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });
    expect(invokes[0]?.path).toBe('/api/v2/inventory/tools/7/inventory/run_ingestion/invoke');
    // `full_rebuild: false` is NOT sent: the provider's merge ignores a falsy
    // tool argument anyway, and sending it suggests it turns a configured
    // rebuild off.
    expect(invokes[0]?.parameters).toEqual({ toolkit_id: '9010' });
  });

  it('sends full_rebuild only when it is asked for', async () => {
    const invokes = serveInvoke();
    servePolls({ status: 'InProgress' });
    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', true);
    });
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });
    expect(invokes[0]?.parameters).toEqual({ toolkit_id: '9010', full_rebuild: true });
  });

  it('refuses a start with no source, which has nothing to ingest', () => {
    const invokes = serveInvoke();
    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('', false);
    });
    expect(result.current.runningSourceId).toBeNull();
    expect(invokes).toEqual([]);
  });

  it('peels the summary and names what the run wrote', async () => {
    serveInvoke();
    servePolls(COMPLETED);
    const onSettled = vi.fn();

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, onSettled));
    act(() => {
      result.current.start('9010', false);
    });

    await waitFor(() => {
      expect(result.current.summary).toBe('Ingested 4 entities and 3 relations from github:9010.');
    });
    // The checkpoint is the record that the run got far enough to be resumable,
    // and it is named in the terminal body and nowhere else.
    expect(result.current.artifacts).toEqual([
      { name: 'graph.json', objectType: 'knowledge_graph' },
      { name: '.ingestion-checkpoint-9010.json', objectType: 'checkpoint' },
    ]);
    expect(result.current.runningSourceId).toBeNull();
    expect(onSettled).toHaveBeenCalledTimes(1);
  });

  it('keeps the progress lines that arrive with the answer', async () => {
    serveInvoke();
    servePolls({
      ...COMPLETED,
      custom_events: [
        { data: { message: 'Reading the source files' } },
        { data: { message: 'Extracting entities' } },
        { data: { message: '' } },
        { data: {} },
      ],
    });

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', false);
    });
    await waitFor(() => {
      expect(result.current.steps).toEqual(['Reading the source files', 'Extracting entities']);
    });
  });

  it('reads a progress event that is an OBJECT rather than a string', async () => {
    serveInvoke();
    servePolls({
      ...COMPLETED,
      custom_events: [
        { data: { message: { text: 'Building the graph' } } },
        { data: { message: { content: 'Writing the checkpoint' } } },
        { data: { message: { unknown: 'not a line' } } },
      ],
    });

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', false);
    });
    await waitFor(() => {
      expect(result.current.steps).toEqual(['Building the graph', 'Writing the checkpoint']);
    });
  });

  it('reports a refused start without leaving the row stuck as running', async () => {
    server.use(http.post(INVOKE_ROUTE, () => HttpResponse.json({ status: 'Started' })));
    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', false);
    });

    await waitFor(() => {
      expect(result.current.error).toMatch(/returned no invocation/);
    });
    expect(result.current.runningSourceId).toBeNull();
  });

  it('reports a run the provider refused', async () => {
    serveInvoke();
    servePolls({ status: 'Error', message: 'This toolkit configures no llm_model.' });
    const onSettled = vi.fn();

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, onSettled));
    act(() => {
      result.current.start('9010', false);
    });
    await waitFor(() => {
      expect(result.current.error).toBe('This toolkit configures no llm_model.');
    });
    expect(result.current.summary).toBeNull();
    expect(onSettled).toHaveBeenCalledTimes(1);
  });

  it('settles a stopped run on its next poll, not on the click', async () => {
    // Clearing the state at the click would leave the poller running against an
    // invocation the screen has forgotten.
    const invokes = serveInvoke();
    const cancels: string[] = [];
    let stopped = false;
    server.use(
      http.delete(INVOCATION_ROUTE, ({ request }) => {
        cancels.push(new URL(request.url).pathname);
        stopped = true;
        return new HttpResponse(null, { status: 204 });
      }),
      http.get(INVOCATION_ROUTE, () =>
        HttpResponse.json(stopped ? { status: 'Stopped' } : { status: 'InProgress' }),
      ),
    );

    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    act(() => {
      result.current.start('9010', false);
    });
    await waitFor(() => {
      expect(invokes).toHaveLength(1);
    });

    act(() => {
      result.current.stop();
    });
    await waitFor(() => {
      expect(cancels).toEqual(['/api/v2/inventory/invocations/7/inventory/run_ingestion/inv-1']);
    });
    // Still running as far as the screen is concerned: the `Stopped` poll is
    // what settles it.
    expect(result.current.runningSourceId).toBe('9010');
  });

  it('does nothing when stop is pressed with no run in flight', () => {
    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));
    expect(() => {
      result.current.stop();
    }).not.toThrow();
  });

  it('clears the previous run when a new one starts', async () => {
    serveInvoke();
    servePolls(COMPLETED);
    const { result } = renderHookWithProviders(() => useIngestionRun(TARGET, () => {}));

    act(() => {
      result.current.start('9010', false);
    });
    await waitFor(() => {
      expect(result.current.summary).not.toBeNull();
    });

    act(() => {
      result.current.start('9011', false);
    });
    expect(result.current.summary).toBeNull();
    expect(result.current.artifacts).toEqual([]);
    expect(result.current.steps).toEqual([]);
    expect(result.current.runningSourceId).toBe('9011');
  });
});
