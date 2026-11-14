import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { PERMISSIONS } from '@/shared/lib/permissions';
import { server } from '@/test/setup';

import { canEditApplication, useApplicationPermissionState } from './applicationEditAccess';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => configureGeneratedClient({ baseUrl: '/api/v2' }));
afterEach(() => resetGeneratedClient());

describe('application edit access', () => {
  it('a published application is read-only for everyone, otherwise the update permission decides', () => {
    expect(canEditApplication(false, true)).toBe(true);
    expect(canEditApplication(false, false)).toBe(false);
    expect(canEditApplication(true, true)).toBe(false);
  });

  it.each([
    ['an admin/editor grant', true],
    ['no grant', false],
  ])('the shared source (read by both editors) answers %s', async (_label, granted) => {
    server.use(
      http.get('*/auth/permissions/prompt_lib/:projectId', () =>
        HttpResponse.json(granted ? [{ name: PERMISSIONS.applications.update, enabled: true }] : [], { status: 200 }),
      ),
    );
    const { result } = renderHook(() => useApplicationPermissionState('p1', PERMISSIONS.applications.update), { wrapper });
    await waitFor(() => expect(result.current.settled).toBe(true));
    expect(result.current.allowed).toBe(granted);
  });
});
