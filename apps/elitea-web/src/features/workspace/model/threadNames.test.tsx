import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { clearNamespace } from '@/shared/lib/storage';

import { server } from '../../../test/setup';
import { useServerThreadName, useThreadNaming, type ThreadRef } from './threadNames';
import { readThreads, recordThread } from './threads';

const REF: ThreadRef = { workspaceId: 'w1', projectId: 42, conversationId: '77' };

let client = new QueryClient();

function wrapper({ children }: { children: React.ReactNode }): React.JSX.Element {
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const title = (): ReturnType<typeof readThreads>[number] | undefined => readThreads('w1').find((thread) => thread.id === '77');

describe('thread names', () => {
  beforeEach(() => {
    client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    configureGeneratedClient({ baseUrl: '/api/v2' });
    clearNamespace();
  });
  afterEach(() => resetGeneratedClient());

  it('asks the server to name a thread once its turn is stored, once, with the name it was given', async () => {
    recordThread('w1', { id: '77', title: 'how do I rotate the gateway TLS', updatedAt: 1 });
    const asked: unknown[] = [];
    server.use(
      http.post('*/elitea_core/conversation_name/prompt_lib/42/77', async ({ request }) => {
        asked.push(await request.json());
        return HttpResponse.json({ name: 'Gateway TLS rotation', renamed: true });
      }),
    );
    const hook = renderHook(({ committed }: { committed: boolean }) => useThreadNaming(REF, committed), { wrapper, initialProps: { committed: false } });
    expect(asked).toEqual([]);
    hook.rerender({ committed: true });
    await waitFor(() => expect(title()).toMatchObject({ title: 'Gateway TLS rotation', named: true }));
    hook.rerender({ committed: false });
    hook.rerender({ committed: true });
    expect(asked).toEqual([{ expected_name: 'how do I rotate the gateway TLS' }]);
  });

  it('does not name a thread that is named already', async () => {
    recordThread('w1', { id: '77', title: 'My notes', updatedAt: 1, named: true });
    let asked = 0;
    server.use(
      http.post('*/elitea_core/conversation_name/prompt_lib/42/77', () => {
        asked += 1;
        return HttpResponse.json({ name: 'x', renamed: true });
      }),
    );
    renderHook(() => useThreadNaming(REF, true), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(asked).toBe(0);
    expect(title()?.title).toBe('My notes');
  });

  it('takes a name the conversation got elsewhere when the thread opens', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    server.use(http.get('*/elitea_core/conversation/prompt_lib/42/77', () => HttpResponse.json({ id: 77, name: 'Renamed on the web' })));
    renderHook(() => useServerThreadName(REF), { wrapper });
    await waitFor(() => expect(title()).toMatchObject({ title: 'Renamed on the web', named: true }));
  });

  it('does not put a cached older name back after the thread was named', async () => {
    recordThread('w1', { id: '77', title: 'fix the build', updatedAt: 1 });
    server.use(
      http.get('*/elitea_core/conversation/prompt_lib/42/77', () => HttpResponse.json({ id: 77, name: 'fix the build' })),
      http.post('*/elitea_core/conversation_name/prompt_lib/42/77', () => HttpResponse.json({ name: 'Fix the CI build', renamed: true })),
    );
    const open = renderHook(() => useServerThreadName(REF), { wrapper });
    await waitFor(() => expect(client.getQueryData(['workspace', 'threadName', 42, '77'])).toBe('fix the build'));
    renderHook(() => useThreadNaming(REF, true), { wrapper });
    await waitFor(() => expect(title()).toMatchObject({ title: 'Fix the CI build', named: true }));
    // The thread is opened again within the cache's minute.
    open.unmount();
    renderHook(() => useServerThreadName(REF), { wrapper });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(title()?.title).toBe('Fix the CI build');
  });
});
