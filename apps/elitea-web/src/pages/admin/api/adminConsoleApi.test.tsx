/**
 * The console capability document's client (ADR-0059 R3.1b, issue 2611):
 * the selectors, the 403 refetch and the focus refetch.
 *
 * The document comes from the `v2.yaml` example of `GET /admin/console`
 * (orval's generated mock), never a hand-written fixture.
 *
 * The 403 test drives a real admin save (`createExecutionSuspension`, the
 * kill switch) through the real query client: what matters is that the
 * document is read again once per burst of refusals, not that some
 * invalidation function was called.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  getGetAdminConsoleDocumentMockHandler,
  getGetAdminConsoleDocumentResponseMock,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useCreateAdminSuspension } from './adminKillSwitchApi';
import { useAdminConsole, useAdminConsoleRefresh } from './adminConsoleApi';

const BASE = 'https://elitea.example';

let consoleReads = 0;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  consoleReads = 0;
  server.use(
    getGetAdminConsoleDocumentMockHandler(() => {
      consoleReads += 1;
      return getGetAdminConsoleDocumentResponseMock();
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function renderConsole() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return renderHook(
    () => {
      useAdminConsoleRefresh();
      return { console: useAdminConsole(), save: useCreateAdminSuspension() };
    },
    { wrapper: Wrapper },
  );
}

/** Lets a burst window pass with real timers, so a second refetch would have had time to fire. */
function settle(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

describe('useAdminConsole', () => {
  it('answers undefined while the document is unknown, then the levels, roles and elevation it carries', async () => {
    const example = getGetAdminConsoleDocumentResponseMock();
    const firstArea = example.areas[0]!;
    const firstItem = firstArea.items[0]!;
    const { result } = renderConsole();

    expect(result.current.console.status).toBe('pending');
    expect(result.current.console.itemLevel(firstItem.id)).toBeUndefined();
    expect(result.current.console.areaLevel(firstArea.id)).toBeUndefined();
    expect(result.current.console.roles).toBeUndefined();
    expect(result.current.console.elevation).toBeUndefined();

    await waitFor(() => expect(result.current.console.status).toBe('success'));
    expect(result.current.console.itemLevel(firstItem.id)).toBe(firstItem.level);
    const expectedArea = firstArea.items.some((item) => item.level === 'write') ? 'write' : 'read';
    expect(result.current.console.areaLevel(firstArea.id)).toBe(expectedArea);
    expect(result.current.console.itemLevel('no-such-item')).toBeNull();
    expect(result.current.console.areaLevel('no-such-area')).toBeNull();
    expect(result.current.console.roles).toEqual(example.roles);
    expect(result.current.console.elevation).toEqual(example.elevation);
  });

  it('reports a failed read as an error, not as an empty document', async () => {
    server.use(http.get('*/admin/console', () => new HttpResponse(null, { status: 503 })));
    const { result } = renderConsole();

    await waitFor(() => expect(result.current.console.status).toBe('error'));
    expect(result.current.console.itemLevel('users')).toBeUndefined();
    expect(result.current.console.roles).toBeUndefined();
  });

  it('reports a read refused with 403 as refused, which a retry cannot change', async () => {
    server.use(http.get('*/admin/console', () => new HttpResponse(null, { status: 403 })));
    const { result } = renderConsole();

    await waitFor(() => expect(result.current.console.status).toBe('refused'));
    await settle(400);
    expect(result.current.console.status).toBe('refused');
  });

  it('turns a read that never answers into the error state after the timeout', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      server.use(getGetAdminConsoleDocumentMockHandler(() => new Promise(() => {})));
      const { result } = renderConsole();
      expect(result.current.console.status).toBe('pending');

      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });

      await waitFor(() => expect(result.current.console.status).toBe('error'));
    } finally {
      vi.useRealTimers();
    }
  });
});

describe('useAdminConsoleRefresh', () => {
  it('fetches the document again after a save answers 403, once per burst of refusals', async () => {
    server.use(http.post('*/admin/execution_suspensions', () => new HttpResponse(null, { status: 403 })));
    const { result } = renderConsole();
    await waitFor(() => expect(result.current.console.status).toBe('success'));
    expect(consoleReads).toBe(1);

    act(() => {
      for (let attempt = 0; attempt < 5; attempt += 1) {
        result.current.save.mutate({ body: { scope: 'platform', reason: 'refused' } });
      }
    });

    await waitFor(() => expect(consoleReads).toBe(2));
    await settle(400);
    expect(consoleReads).toBe(2);
  });

  it('does not refetch on an error other than 403', async () => {
    server.use(http.post('*/admin/execution_suspensions', () => new HttpResponse(null, { status: 409 })));
    const { result } = renderConsole();
    await waitFor(() => expect(result.current.console.status).toBe('success'));

    act(() => {
      result.current.save.mutate({ body: { scope: 'platform', reason: 'conflict' } });
    });
    await waitFor(() => expect(result.current.save.isError).toBe(true));
    await settle(400);
    expect(consoleReads).toBe(1);
  });

  it('fetches the document again when the window regains focus', async () => {
    const { result } = renderConsole();
    await waitFor(() => expect(result.current.console.status).toBe('success'));
    expect(consoleReads).toBe(1);

    fireEvent(window, new Event('focus'));

    await waitFor(() => expect(consoleReads).toBe(2));
  });

  it('leaves a failed read to the retry control on focus, so the error panel does not flash', async () => {
    server.use(http.get('*/admin/console', () => new HttpResponse(null, { status: 503 })));
    const { result } = renderConsole();
    await waitFor(() => expect(result.current.console.status).toBe('error'));

    fireEvent(window, new Event('focus'));

    await settle(100);
    expect(result.current.console.status).toBe('error');
  });
});
