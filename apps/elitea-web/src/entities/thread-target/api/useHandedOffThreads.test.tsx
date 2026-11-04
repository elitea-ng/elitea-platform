import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getListConversationsMockHandler } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useHandedOffThreads } from './useHandedOffThreads';

/** A listing of `total` Local work threads, every third one in the cloud, served page by page. */
function serveThreads(total: number): { offsets: number[] } {
  const offsets: number[] = [];
  server.use(
    getListConversationsMockHandler(({ request }) => {
      const url = new URL(request.url);
      const offset = Number(url.searchParams.get('offset'));
      const limit = Number(url.searchParams.get('limit'));
      offsets.push(offset);
      const count = Math.max(0, Math.min(limit, total - offset));
      const rows = Array.from({ length: count }, (_, i) => {
        const n = offset + i;
        return {
          id: n,
          uuid: `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`,
          name: `Thread ${n}`,
          source: 'local_work',
          workspace_executor: n % 3 === 0 ? 'cloud' : 'local',
          created_at: '2026-11-04T09:00:00Z',
          updated_at: '2026-11-04T09:00:00Z',
          duration: -1,
          message_groups_count: 1,
          is_pinned: false,
          is_archived: false,
        };
      });
      return { total, rows };
    }),
  );
  return { offsets };
}

function wrapper({ children }: { children: ReactNode }): ReactNode {
  return <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>;
}

describe('useHandedOffThreads', () => {
  beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
  afterEach(() => resetGeneratedClient());

  it('reads every page until the list ends', async () => {
    const { offsets } = serveThreads(250);
    const { result } = renderHook(() => useHandedOffThreads('1'), { wrapper });
    await waitFor(() => expect(result.current.names.size).toBe(250));
    expect(offsets).toEqual([0, 100, 200]);
    expect(result.current.threads).toHaveLength(84);
    expect(result.current.capped).toBe(false);
  });

  it('stops at 1000 rows and says so', async () => {
    const { offsets } = serveThreads(5000);
    const { result } = renderHook(() => useHandedOffThreads('1'), { wrapper });
    await waitFor(() => expect(result.current.capped).toBe(true));
    expect(offsets).toHaveLength(10);
    expect(result.current.names.size).toBe(1000);
  });
});
