import { act } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderHookWithProviders } from '../__tests__/testUtils';
import { useApplicationBulkActions } from './useApplicationBulkActions';

const BASE = '/api/v2';

// Issue 3009: a sub-agent copy is refused while its parent holds it, and the
// parent's delete takes the copy along. Selected together, copy first, both
// end up deleted, and the bulk delete must not report the copy as failed.
describe('bulk delete', () => {
  let parentDeleted = false;
  let calls: string[] = [];

  beforeEach(() => {
    parentDeleted = false;
    calls = [];
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.delete(`${BASE}/elitea_core/application/prompt_lib/:projectId/:applicationId`, ({ params }) => {
        const id = String(params['applicationId']);
        calls.push(id);
        if (id === '1') {
          parentDeleted = true;
          return new HttpResponse(null, { status: 204 });
        }
        if (id === '2' && !parentDeleted) {
          return HttpResponse.json({ error: 'This agent is the sub-agent copy of another agent.' }, { status: 400 });
        }
        if (id === '3') return HttpResponse.json({ error: 'Unpublish first.' }, { status: 400 });
        return new HttpResponse(null, { status: 204 });
      }),
    );
  });

  it('retries a copy refused before its parent, and reports nothing', async () => {
    const { result } = renderHookWithProviders(() => useApplicationBulkActions('2', 'agent'));
    let outcome: Awaited<ReturnType<typeof result.current.deleteMany>> | undefined;
    await act(async () => {
      outcome = await result.current.deleteMany(['2', '1']);
    });
    expect(outcome?.failed).toEqual([]);
    expect(calls).toEqual(['2', '1', '2']);
    expect(result.current.error).toBeUndefined();
  });

  it('still reports a refusal that the second pass does not clear, in the server words', async () => {
    const { result } = renderHookWithProviders(() => useApplicationBulkActions('2', 'agent'));
    let outcome: Awaited<ReturnType<typeof result.current.deleteMany>> | undefined;
    await act(async () => {
      outcome = await result.current.deleteMany(['3', '1']);
    });
    expect(outcome?.failed).toEqual(['3']);
    expect(result.current.error).toContain('Unpublish first.');
  });
});
