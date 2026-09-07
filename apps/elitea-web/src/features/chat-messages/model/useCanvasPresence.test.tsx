/**
 * useCanvasPresence.test.tsx — #622.
 *
 * The two halves this hook joins are tested at the boundaries the app really
 * has: msw for the heartbeat (a real `fetch` through `eliteaFetch`) and the
 * `shared/api/sse` `EventSource` double for the fan-out, which is a browser
 * global jsdom does not implement rather than a library being mocked (R-M1).
 *
 * WHAT WOULD FAIL WITHOUT THE FEATURE: every case here. Before #622 the hook
 * did not exist and `useCanvasRoom` had zero consumers, so the editor emitted
 * canvas edits into a room it had never joined and no presence event was ever
 * delivered to anyone.
 */
import type { ReactNode } from 'react';

import { act, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { useCanvasPresence } from './useCanvasPresence';

const globals = globalThis as unknown as Record<string, unknown>;
const PRESENCE_PATH = '/api/v2/elitea_core/canvas/prompt_lib/:projectId/:canvasId/presence';
const CANVAS_UUID = 'aaaaaaaa-0000-4000-8000-000000000001';

let registry: TestEventSourceRegistry;

interface RosterEditor {
  readonly user_id: string;
  readonly user_name: string;
  readonly state?: string;
}

function roster(editors: readonly RosterEditor[], ttlSeconds = 120): Record<string, unknown> {
  return {
    project_id: '7',
    entity_id: CANVAS_UUID,
    entity_type: 'canvas',
    action: 'editors',
    canvas_uuid: CANVAS_UUID,
    message_group_uuid: 'group-1',
    editors,
    ttl_seconds: ttlSeconds,
  };
}

/** Records every beat the hook sends, so the SCHEDULE can be asserted, not only the roster. */
function installPresenceRoute(
  reply: (state: string) => Record<string, unknown>,
): { readonly beats: string[] } {
  const beats: string[] = [];
  server.use(
    http.post(PRESENCE_PATH, async ({ request }) => {
      const body = (await request.json()) as { state?: string };
      const state = body.state ?? 'viewing';
      beats.push(state);
      // The route answers the roster as the BODY. `eliteaFetch` returns the
      // {data,status,headers} envelope whose `data` IS that body (#132), which
      // is why the sender unwraps one level and only one.
      return HttpResponse.json(reply(state));
    }),
  );
  return { beats };
}

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  return children;
}

beforeEach(() => {
  registry = installTestEventSource();
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  registry.restore();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('useCanvasPresence', () => {
  it('announces on mount and renders the roster the server answered', async () => {
    const { beats } = installPresenceRoute(() =>
      roster([
        { user_id: '1', user_name: 'ada@example.com', state: 'editing' },
        { user_id: '2', user_name: 'grace@example.com', state: 'editing' },
      ]),
    );

    const { result } = renderHook(
      () => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID, userName: 'ada@example.com' }),
      { wrapper },
    );

    await waitFor(() => {
      expect(result.current.editors).toHaveLength(2);
    });
    expect(beats).toEqual(['editing']);
    expect(result.current.otherEditors.map((editor) => editor.userName)).toEqual(['grace@example.com']);
    // Somebody else is here, but so is this caller — the reference's rule.
    expect(result.current.isReadOnly).toBe(false);
  });

  it('subscribes to the PROJECT stream, not to a canvas room', async () => {
    installPresenceRoute(() => roster([]));
    renderHook(() => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID }), { wrapper });

    await waitFor(() => {
      expect(registry.getOpen()).toHaveLength(1);
    });
    // The url carries the project and nothing else. A canvas-scoped room name
    // is exactly what the deleted socket prototype got wrong (#622): canvas ids
    // are per-schema integers, so a canvas-only room collides across projects.
    expect(registry.getSources()[0]?.url).toBe('/api/v2/elitea_core/events/prompt_lib/7');
    expect(registry.getSources()[0]?.withCredentials).toBe(true);
  });

  it('applies a canvas.editors frame for THIS canvas and ignores one for another', async () => {
    installPresenceRoute(() => roster([{ user_id: '1', user_name: 'ada@example.com' }]));
    const { result } = renderHook(
      () => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID, userName: 'ada@example.com' }),
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.editors).toHaveLength(1);
    });

    // A frame for a DIFFERENT canvas in the same project. The project channel
    // carries every canvas, so this arrives at every subscriber.
    act(() => {
      registry.emit(
        'canvas.editors',
        JSON.stringify({ ...roster([{ user_id: '9', user_name: 'mallory@example.com' }]), canvas_uuid: 'other-canvas' }),
      );
    });
    expect(result.current.editors.map((editor) => editor.userName)).toEqual(['ada@example.com']);

    // A frame for THIS canvas is applied.
    act(() => {
      registry.emit(
        'canvas.editors',
        JSON.stringify(
          roster([
            { user_id: '1', user_name: 'ada@example.com' },
            { user_id: '2', user_name: 'grace@example.com' },
          ]),
        ),
      );
    });
    expect(result.current.editors).toHaveLength(2);
    expect(result.current.otherEditors.map((editor) => editor.userName)).toEqual(['grace@example.com']);
  });

  it('goes read-only when somebody else holds the canvas and this caller does not', async () => {
    installPresenceRoute(() => roster([{ user_id: '2', user_name: 'grace@example.com' }]));
    const { result } = renderHook(
      () => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID, userName: 'ada@example.com' }),
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.isReadOnly).toBe(true);
    });
  });

  it('is editable with an empty roster — the unchanged-with-no-second-editor case', async () => {
    installPresenceRoute(() => roster([]));
    const { result } = renderHook(
      () => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID, userName: 'ada@example.com' }),
      { wrapper },
    );
    await waitFor(() => {
      expect(registry.getOpen()).toHaveLength(1);
    });
    expect(result.current.isReadOnly).toBe(false);
    expect(result.current.editors).toEqual([]);
  });

  it('sends a left beat on unmount', async () => {
    const { beats } = installPresenceRoute(() => roster([]));
    const { unmount } = renderHook(() => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID }), { wrapper });
    await waitFor(() => {
      expect(beats).toEqual(['editing']);
    });

    unmount();
    await waitFor(() => {
      expect(beats).toEqual(['editing', 'left']);
    });
  });

  it('leaves when the tab is hidden and re-announces when it comes back', async () => {
    const { beats } = installPresenceRoute(() => roster([]));
    renderHook(() => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID }), { wrapper });
    await waitFor(() => {
      expect(beats).toEqual(['editing']);
    });

    const setVisibility = (value: string): void => {
      Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
      document.dispatchEvent(new Event('visibilitychange'));
    };

    act(() => setVisibility('hidden'));
    await waitFor(() => {
      expect(beats).toEqual(['editing', 'left']);
    });

    act(() => setVisibility('visible'));
    await waitFor(() => {
      expect(beats).toEqual(['editing', 'left', 'editing']);
    });
  });

  it('resends on a timer derived from the server ttl, not from a client constant', async () => {
    vi.useFakeTimers();
    try {
      const { beats } = installPresenceRoute(() => roster([], 30));
      renderHook(() => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID }), { wrapper });
      await vi.waitFor(() => {
        expect(beats).toEqual(['editing']);
      });

      // ttl 30s / 3 beats = a 10s interval. At 9s nothing has been sent.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(9_000);
      });
      expect(beats).toEqual(['editing']);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(2_000);
      });
      await vi.waitFor(() => {
        expect(beats).toEqual(['editing', 'editing']);
      });
    } finally {
      vi.useRealTimers();
    }
  });

  it('sends nothing and opens no stream without a canvas', () => {
    renderHook(() => useCanvasPresence({ projectId: '7', canvasId: undefined }), { wrapper });
    expect(registry.getOpen()).toHaveLength(0);
  });

  it('clears the roster rather than keeping a stale lock when a beat fails', async () => {
    let failNext = false;
    server.use(
      http.post(PRESENCE_PATH, () => {
        if (failNext) return HttpResponse.json({ error: 'nope' }, { status: 500 });
        return HttpResponse.json(roster([{ user_id: '2', user_name: 'grace@example.com' }]));
      }),
    );

    const { result, rerender } = renderHook(
      (props: { readonly canvasId: string }) =>
        useCanvasPresence({ projectId: '7', canvasId: props.canvasId, userName: 'ada@example.com' }),
      { wrapper, initialProps: { canvasId: CANVAS_UUID } },
    );
    await waitFor(() => {
      expect(result.current.isReadOnly).toBe(true);
    });

    failNext = true;
    rerender({ canvasId: 'bbbbbbbb-0000-4000-8000-000000000002' });
    await waitFor(() => {
      expect(result.current.editors).toEqual([]);
    });
    expect(result.current.isReadOnly).toBe(false);
  });

  it('drops a malformed frame instead of throwing out of the stream', async () => {
    installPresenceRoute(() => roster([{ user_id: '1', user_name: 'ada@example.com' }]));
    const { result } = renderHook(
      () => useCanvasPresence({ projectId: '7', canvasId: CANVAS_UUID, userName: 'ada@example.com' }),
      { wrapper },
    );
    await waitFor(() => {
      expect(result.current.editors).toHaveLength(1);
    });

    act(() => {
      registry.emit('canvas.editors', 'not json at all');
      registry.emit('canvas.editors', JSON.stringify({ canvas_uuid: CANVAS_UUID }));
    });
    expect(result.current.editors).toHaveLength(1);
  });
});
