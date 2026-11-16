import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getPermissionListMockHandler } from '@/shared/api/generated/auth/auth.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useHasAdminPermission } from './useHasAdminPermission';

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useHasAdminPermission', () => {
  it('is false while there is no project id', () => {
    const { result } = renderHook(() => useHasAdminPermission(undefined), { wrapper });
    expect(result.current).toBe(false);
  });

  it('is false when the permission set does not include the applications-list permission', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.chat.conversations.list', enabled: true }]));
    const { result } = renderHook(() => useHasAdminPermission('1'), { wrapper });
    await waitFor(() => expect(result.current).toBe(false));
  });

  it('is true when the permission set includes the applications-list permission and it is enabled', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: true }]));
    const { result } = renderHook(() => useHasAdminPermission('1'), { wrapper });
    await waitFor(() => expect(result.current).toBe(true));
  });

  it('is false when the permission is present but disabled', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.applications.applications.list', enabled: false }]));
    const { result } = renderHook(() => useHasAdminPermission('1'), { wrapper });
    await waitFor(() => expect(result.current).toBe(false));
  });

  it('is false when only the PUBLIC agents-feed list permission is present — the admin tab needs the private-listing permission, not this one', async () => {
    server.use(getPermissionListMockHandler([{ name: 'models.applications.public_applications.list', enabled: true }]));
    const { result } = renderHook(() => useHasAdminPermission('1'), { wrapper });
    await waitFor(() => expect(result.current).toBe(false));
  });
});
