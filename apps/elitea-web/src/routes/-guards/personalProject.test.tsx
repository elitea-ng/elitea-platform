/**
 * personalProject.test.tsx
 *
 * DEFECT this file pins. The Settings drawer's "is this my personal project?"
 * gate was `String(selectedProjectId) === String(personalProjectId)`. That is
 * what the reference does, and it is not enough against this backend:
 * `resolvePersonalProjectID`'s third branch answers `personal_project_id` with
 * "the lowest-id project the user actually holds a role in" whenever no
 * `project_user_<uid>` row exists, and provisioning only runs when the resolver
 * answers "" — so for every member of a single shared project the field IS that
 * shared project, permanently.
 *
 * Read as an id comparison, the drawer dropped the Users tab and redirected
 * `/settings/users` back to General for those accounts. Measured on the E2E
 * stack, whose two personas are exactly that shape: J22a–J22e and the
 * `settings-users` visual shot all failed, the invite dialog opening and then
 * being torn out of the DOM when `personal_project_id` resolved.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';

import { server } from '../../test/setup';

import { useIsPersonalProject } from './personalProject';

const BASE = '/api/v2';
const PUBLIC_PROJECT_ID = '11';

/** One shared project (id 1) and one real personal project (id 7). */
const PROJECTS = [
  { id: 1, name: 'Default Project', owner_id: 1, suspended: false, create_success: true },
  { id: 7, name: 'project_user_7', owner_id: 7, suspended: false, create_success: true },
];

function mockProjects(rows: unknown[] = PROJECTS): void {
  server.use(
    http.get(`${BASE}/projects/project/default/${PUBLIC_PROJECT_ID}`, () => HttpResponse.json(rows)),
  );
}

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
}

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', PUBLIC_PROJECT_ID);
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  vi.unstubAllEnvs();
});

describe('useIsPersonalProject', () => {
  it('is false for a SHARED project the resolver merely named as personal', async () => {
    mockProjects();
    // BOTH gates in one render, because a `false` read taken before the list
    // arrives would pass this test against the very defect it exists to catch.
    // The personal one turning true is the settle signal: it can only do that
    // from a resolved list, and it comes from the same query.
    const { result } = renderHook(
      () => ({
        shared: useIsPersonalProject('1', '1'),
        personal: useIsPersonalProject('7', '7'),
      }),
      { wrapper },
    );

    await waitFor(() => {
      expect(result.current.personal).toBe(true);
    });
    expect(result.current.shared).toBe(false);
  });

  it('is true for the caller’s real `project_user_<uid>` project', async () => {
    mockProjects();
    const { result } = renderHook(() => useIsPersonalProject('7', '7'), { wrapper });

    await waitFor(() => {
      expect(result.current).toBe(true);
    });
  });

  it('is false when a different project is selected', async () => {
    mockProjects();
    const { result } = renderHook(() => useIsPersonalProject('1', '7'), { wrapper });

    await waitFor(() => {
      expect(result.current).toBe(false);
    });
  });

  it('is false while the list is unresolved, and false when the row is missing', async () => {
    mockProjects([]);
    const { result } = renderHook(() => useIsPersonalProject('7', '7'), { wrapper });

    expect(result.current).toBe(false);
    await waitFor(() => {
      expect(result.current).toBe(false);
    });
  });

  it('is false when the server names no personal project at all', async () => {
    mockProjects();
    const { result } = renderHook(() => useIsPersonalProject('7', undefined), { wrapper });

    await waitFor(() => {
      expect(result.current).toBe(false);
    });
  });
});
