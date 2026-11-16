import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { Application } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useProbationUndo } from './useProbationUndo';

const ACTIONS_URL = '*/elitea_core/agent_workforce/prompt_lib/:projectId/automatic_actions';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

const hired = { id: '7', name: 'Hired', meta: { on_probation: true } } as unknown as Application;
const own = { id: '8', name: 'Own', meta: {} } as unknown as Application;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useProbationUndo (#2091)', () => {
  it('offers Undo on an automatic hire still on probation, and posts the undo of its action', async () => {
    let undone = '';
    server.use(
      http.get(ACTIONS_URL, () =>
        HttpResponse.json({ rows: [{ id: 41, application_id: 7, kind: 'auto_hire', status: 'applied' }], total: 1 }),
      ),
      http.post('*/elitea_core/agent_workforce/prompt_lib/:projectId/automatic_actions/:actionId/undo', ({ params }) => {
        undone = String(params['actionId']);
        return HttpResponse.json({ id: 41, status: 'undone' });
      }),
    );
    const { result } = renderHook(() => useProbationUndo('3', [hired, own]), { wrapper });
    await waitFor(() => expect(result.current(hired)).toBeDefined());
    expect(result.current(own)).toBeUndefined();
    result.current(hired)?.();
    await waitFor(() => expect(undone).toBe('41'));
  });

  it('offers nothing when the person may not read the automatic actions (403)', async () => {
    let reads = 0;
    server.use(
      http.get(ACTIONS_URL, () => {
        reads += 1;
        return HttpResponse.json({ error: 'forbidden' }, { status: 403 });
      }),
    );
    const { result } = renderHook(() => useProbationUndo('3', [hired]), { wrapper });
    await waitFor(() => expect(reads).toBe(1));
    expect(result.current(hired)).toBeUndefined();
  });

  it('does not read the actions at all while no row is on probation', async () => {
    let reads = 0;
    server.use(
      http.get(ACTIONS_URL, () => {
        reads += 1;
        return HttpResponse.json({ rows: [], total: 0 });
      }),
    );
    const { result } = renderHook(() => useProbationUndo('3', [own]), { wrapper });
    expect(result.current(own)).toBeUndefined();
    expect(reads).toBe(0);
  });
});
