/**
 * The read-only primitive's answers (ADR-0059 decision 6; R3.4a, issue
 * 2534): the level comes from the capability document, an item the known
 * document does not list is `none`, an unknown document is `undefined` and
 * keeps writes on, and at `read` the guard sends nothing.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useAdminWritable } from './adminWriteAccess';

function serveDocument(level: 'read' | 'write'): void {
  server.use(
    http.get('*/admin/console', () =>
      HttpResponse.json({
        areas: [
          {
            id: 'models_spend',
            label_key: 'pages.admin.console.area.models_spend',
            items: [{ id: 'budgets-alerts', label_key: 'pages.admin.console.item.budgets-alerts', level }],
          },
        ],
        roles: [],
        elevation: null,
        admin_console_areas: true,
      }),
    ),
  );
}

function wrapper({ children }: { readonly children: ReactNode }): ReactNode {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

function fakeMutation() {
  return { mutate: vi.fn(), mutateAsync: vi.fn(() => Promise.resolve('sent')), isPending: false };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useAdminWritable', () => {
  it('passes the mutation through unchanged at write', async () => {
    serveDocument('write');
    const { result } = renderHook(() => useAdminWritable('budgets-alerts'), { wrapper });
    await waitFor(() => expect(result.current.access).toBe('write'));

    const mutation = fakeMutation();
    expect(result.current.writable).toBe(true);
    expect(result.current.guard(mutation)).toBe(mutation);
  });

  it('sends nothing at read: mutate does nothing and mutateAsync rejects', async () => {
    serveDocument('read');
    const { result } = renderHook(() => useAdminWritable('budgets-alerts'), { wrapper });
    await waitFor(() => expect(result.current.access).toBe('read'));

    const mutation = fakeMutation();
    const guarded = result.current.guard(mutation);
    expect(result.current.writable).toBe(false);
    guarded.mutate();
    await expect(guarded.mutateAsync()).rejects.toThrow(/view only/);
    expect(mutation.mutate).not.toHaveBeenCalled();
    expect(mutation.mutateAsync).not.toHaveBeenCalled();
    expect(guarded.isPending).toBe(false);
  });

  it('answers none for an item the known document does not list', async () => {
    serveDocument('write');
    const { result } = renderHook(() => useAdminWritable('secrets'), { wrapper });
    await waitFor(() => expect(result.current.access).toBe('none'));
    expect(result.current.writable).toBe(false);
  });

  it('keeps writes on while the document cannot be read', async () => {
    let answered = false;
    server.use(
      http.get('*/admin/console', () => {
        answered = true;
        return new HttpResponse(null, { status: 503 });
      }),
    );
    const { result, rerender } = renderHook(() => useAdminWritable('budgets-alerts'), { wrapper });
    await waitFor(() => expect(answered).toBe(true));
    rerender();
    // The failed read leaves the answer unknown, never `none`.
    expect(result.current.access).toBeUndefined();
    const mutation = fakeMutation();
    expect(result.current.guard(mutation)).toBe(mutation);
  });
});
