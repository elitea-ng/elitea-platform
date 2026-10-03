/**
 * useNotificationsSSE.test.tsx — issue #92.
 *
 * Runtime config comes from a real `globalThis.elitea_ui_config` object +
 * `resetConfigForTests()` (the `ToolkitEditor.test.tsx` pattern), not a
 * `getConfig` spy: the URL this hook builds IS its contract, so resolving it
 * through the real C6 source chain is what makes the assertion meaningful.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { createRealtimeStatusStore, RealtimeStatusContext, useRealtimeStatus } from '@/shared/api/sse/realtimeStatus';
import { installTestEventSource, type TestEventSourceRegistry } from '@/shared/api/sse/testing';
import { resetConfigForTests } from '@/shared/config/get-config';

import { NOTIFICATIONS_QUERY_ROOT } from '../api/useNotifications';
import { useNotificationsSSE } from './useNotificationsSSE';

const globals = globalThis as unknown as Record<string, unknown>;

let registry: TestEventSourceRegistry;

function setConfig(serverUrl?: string): void {
  if (serverUrl === undefined) {
    delete globals['elitea_ui_config'];
  } else {
    globals['elitea_ui_config'] = { vite_server_url: serverUrl, vite_base_uri: '/', vite_public_project_id: 'public-1' };
  }
  resetConfigForTests();
}

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

beforeEach(() => {
  registry = installTestEventSource();
  setConfig('/api/v2');
});

afterEach(() => {
  registry.restore();
  setConfig(undefined);
});

describe('useNotificationsSSE', () => {
  it('subscribes to the project-scoped Go SSE route with credentials', () => {
    const queryClient = new QueryClient();
    renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    expect(registry.getOpen()).toHaveLength(1);
    expect(registry.getSources()[0]?.url).toBe('/api/v2/notifications/events/prompt_lib/7');
    expect(registry.getSources()[0]?.withCredentials).toBe(true);
  });

  it('calls onNotify for every notifications_notify event', () => {
    const queryClient = new QueryClient();
    const onNotify = vi.fn();
    renderHook(() => useNotificationsSSE('7', onNotify), { wrapper: wrapper(queryClient) });

    act(() => {
      registry.emit('notifications_notify');
      registry.emit('notifications_notify');
    });
    expect(onNotify).toHaveBeenCalledTimes(2);
  });

  it('invalidates every notifications query on notifications_ready', () => {
    const queryClient = new QueryClient();
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries');
    renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    act(() => {
      registry.emit('notifications_ready');
    });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: NOTIFICATIONS_QUERY_ROOT });
  });

  it('does not treat notifications_ready as a notify (the two events are not interchangeable)', () => {
    const queryClient = new QueryClient();
    const onNotify = vi.fn();
    renderHook(() => useNotificationsSSE('7', onNotify), { wrapper: wrapper(queryClient) });

    act(() => {
      registry.emit('notifications_ready');
    });
    expect(onNotify).not.toHaveBeenCalled();
  });

  it('opens nothing without a project id, and connects once one arrives', () => {
    const queryClient = new QueryClient();
    const { rerender } = renderHook((projectId?: string) => useNotificationsSSE(projectId, vi.fn()), {
      wrapper: wrapper(queryClient),
      initialProps: undefined,
    });
    expect(registry.getSources()).toHaveLength(0);

    rerender('7');
    expect(registry.getSources()[0]?.url).toBe('/api/v2/notifications/events/prompt_lib/7');
  });

  it('opens nothing when the runtime config never resolved (missing vite_server_url)', () => {
    setConfig(undefined);
    const queryClient = new QueryClient();
    renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });
    expect(registry.getSources()).toHaveLength(0);
  });

  it('closes the stream on unmount', () => {
    const queryClient = new QueryClient();
    const { unmount } = renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });
    unmount();
    expect(registry.getOpen()).toHaveLength(0);
  });

  it('keeps one stream open across re-renders with an unstable onNotify identity', () => {
    const queryClient = new QueryClient();
    const { rerender } = renderHook(() => useNotificationsSSE('7', () => undefined), { wrapper: wrapper(queryClient) });
    rerender();
    rerender();
    expect(registry.getSources()).toHaveLength(1);
  });

  it('reconnects to the new project stream when the project id changes', () => {
    const queryClient = new QueryClient();
    const { rerender } = renderHook((projectId: string) => useNotificationsSSE(projectId, vi.fn()), {
      wrapper: wrapper(queryClient),
      initialProps: '7',
    });
    rerender('9');

    const sources = registry.getSources();
    expect(sources).toHaveLength(2);
    expect(sources[0]?.closed).toBe(true);
    expect(sources[1]?.url).toBe('/api/v2/notifications/events/prompt_lib/9');
  });
});

/*
 * DEFECT: a failed notifications stream was permanent, and the polling
 * fallback its own comment promised did not exist.
 *
 * `onError` wrote one `console.warn` and nothing else; `useEventSource` never
 * reopens; and `refetchInterval` appeared nowhere in the app, so the claim
 * that losing the stream "degrades to the pre-#92 polling behaviour" was
 * false. The route rejects a stream with an HTTP status. It sends 429 with
 * `Retry-After: 2` once the per-principal cap of 4 concurrent streams is
 * saturated (`events.go`'s `newCurrentNotificationAdmission(64, 4)`). It also
 * sends 403 and 503. Per WHATWG an HTTP status fails an `EventSource` for
 * good.
 *
 * Opening a fifth tab therefore killed live notifications in that tab
 * for the life of the mount. Only a console line showed it.
 */
describe('useNotificationsSSE — reconnect', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('reopens a failed stream after the shared backoff, resuming from the last event id', () => {
    const queryClient = new QueryClient();
    renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    act(() => {
      registry.emit('notifications_notify', '', '12');
      registry.fail();
    });
    expect(registry.getSources()).toHaveLength(1);

    act(() => {
      vi.advanceTimersByTime(1_000);
    });

    const sources = registry.getSources();
    expect(sources).toHaveLength(2);
    // A reopened `EventSource` cannot send `Last-Event-ID`. The route
    // reads a missing cursor as "start from the high-water mark". Without
    // the query cursor every notification sent while the stream was down
    // would be skipped.
    expect(sources[1]?.url).toBe('/api/v2/notifications/events/prompt_lib/7?cursor=12');
  });

  // The `error` event also fires on a mid-stream drop, where the browser is
  // already reconnecting. Reopening then runs two streams for one principal.
  // It reaches the server's per-user cap of 4 twice as fast. It therefore
  // manufactures the very 429 the reconnect exists to survive.
  it('does not reopen while the browser is reconnecting by itself', () => {
    const queryClient = new QueryClient();
    renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    act(() => {
      registry.drop();
    });
    act(() => {
      vi.advanceTimersByTime(30_000);
    });

    expect(registry.getSources()).toHaveLength(1);
  });

  it('reports streamDead once the four attempts are spent, and warns exactly once', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const queryClient = new QueryClient();
    const { result } = renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    expect(result.current.streamDead).toBe(false);
    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
      });
      act(() => {
        vi.advanceTimersByTime(delay);
      });
    }
    act(() => {
      registry.fail();
    });

    expect(registry.getSources()).toHaveLength(5);
    expect(result.current.streamDead).toBe(true);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(String(warn.mock.calls[0]?.[0])).toContain('notifications SSE stream failed');
    warn.mockRestore();
  });

  // A stream that really opened proves the connection works, so the next
  // failure must not inherit a spent budget.
  it('restarts the attempt budget after a stream opens', () => {
    const queryClient = new QueryClient();
    const { result } = renderHook(() => useNotificationsSSE('7', vi.fn()), { wrapper: wrapper(queryClient) });

    for (const delay of [1_000, 2_000, 4_000]) {
      act(() => {
        registry.fail();
      });
      act(() => {
        vi.advanceTimersByTime(delay);
      });
    }
    act(() => {
      registry.emit('open');
    });
    act(() => {
      registry.fail();
    });
    act(() => {
      vi.advanceTimersByTime(1_000);
    });

    expect(result.current.streamDead).toBe(false);
    expect(registry.getSources()).toHaveLength(5);
  });
});

/*
 * The app shell's connection dot reads this stream's health. It used to read
 * the socket.io client, which is a permanently-`disconnected` noop on every
 * deployment, so the dot said "Disconnected" on a working app.
 */
describe('useNotificationsSSE — connection indicator reporting', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  function renderWithStatus(projectId: string | undefined) {
    const queryClient = new QueryClient();
    const store = createRealtimeStatusStore();
    const Query = wrapper(queryClient);
    function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
      return (
        <RealtimeStatusContext.Provider value={store}>
          <Query>{children}</Query>
        </RealtimeStatusContext.Provider>
      );
    }
    const stream = renderHook(({ id }: { id: string | undefined }) => useNotificationsSSE(id, vi.fn()), {
      wrapper: Wrapper,
      initialProps: { id: projectId },
    });
    const status = renderHook(() => useRealtimeStatus(), { wrapper: Wrapper });
    return { stream, status };
  }

  it('reports connecting, then connected once the stream opens', () => {
    const { status } = renderWithStatus('7');
    expect(status.result.current).toBe('connecting');
    act(() => {
      registry.emit('open');
    });
    expect(status.result.current).toBe('connected');
  });

  it('reports reconnecting on a browser-driven drop, and connected again on reopen', () => {
    const { status } = renderWithStatus('7');
    act(() => {
      registry.emit('open');
      registry.drop();
    });
    expect(status.result.current).toBe('reconnecting');
    act(() => {
      registry.emit('open');
    });
    expect(status.result.current).toBe('connected');
  });

  it('reports reconnecting while a failed stream waits for its scheduled reopen', () => {
    const { status } = renderWithStatus('7');
    act(() => {
      registry.emit('open');
      registry.fail();
    });
    expect(status.result.current).toBe('reconnecting');
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
    act(() => {
      registry.emit('open');
    });
    expect(status.result.current).toBe('connected');
  });

  it('reports offline once every reconnect attempt is spent', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => undefined);
    const { status } = renderWithStatus('7');
    for (const delay of [1_000, 2_000, 4_000, 8_000]) {
      act(() => {
        registry.fail();
      });
      act(() => {
        vi.advanceTimersByTime(delay);
      });
    }
    act(() => {
      registry.fail();
    });
    expect(status.result.current).toBe('offline');
    warn.mockRestore();
  });

  it('withdraws its report when there is no project to subscribe to, and on unmount', () => {
    const { stream, status } = renderWithStatus(undefined);
    expect(status.result.current).toBe('idle');

    stream.rerender({ id: '7' });
    act(() => {
      registry.emit('open');
    });
    expect(status.result.current).toBe('connected');

    stream.unmount();
    expect(status.result.current).toBe('idle');
  });
});
