/**
 * The invite dialog's state, at the pairing that is the whole reason it is a
 * hook: closing the dialog must ALSO drop the rows.
 *
 * A version that kept them would render the previous batch's failures under
 * the next batch's addresses — the same defect class as the stale result list
 * this feature exists to introduce correctly.
 */
import type { ReactNode } from 'react';

import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { useInviteDialog } from './useInviteDialog';
import { readInviteRows } from '@/shared/ui/settings/inviteResults';

const ROWS = readInviteRows([{ email: 'a@x.io', status: 'error', outcome: 'already_member' }]);

/** The router's default search parser JSON-parses raw values, so `?inviteUsers=1` arrives as the NUMBER 1. */
function validateInviteFlag(search: Record<string, unknown>): { inviteUsers?: string } {
  const raw = search['inviteUsers'];
  if (typeof raw === 'string') return { inviteUsers: raw };
  if (typeof raw === 'number' || typeof raw === 'boolean') return { inviteUsers: String(raw) };
  return {};
}

function makeWrapper(initialEntry: string) {
  const rootRoute = createRootRoute();
  let render: (children: ReactNode) => ReactNode = (children) => children;
  const usersRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/settings/users',
    validateSearch: validateInviteFlag,
    component: () => render(null),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([usersRoute]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  });

  function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    render = () => children;
    return <RouterProvider router={router as never} />;
  }

  return { Wrapper, router };
}

describe('useInviteDialog', () => {
  it('starts closed with no rows', async () => {
    const { Wrapper } = makeWrapper('/settings/users');
    const { result } = renderHook(() => useInviteDialog(), { wrapper: Wrapper });

    await waitFor(() => expect(result.current.open).toBe(false));
    expect(result.current.results).toEqual([]);
  });

  it('drops the rows when the dialog is closed, whichever way it closes', async () => {
    const { Wrapper } = makeWrapper('/settings/users');
    const { result } = renderHook(() => useInviteDialog(), { wrapper: Wrapper });
    // The RouterProvider mounts its children on a microtask, so the hook has
    // not run on the first tick and `result.current` is undefined until it has.
    await waitFor(() => expect(result.current).toBeTruthy());

    act(() => result.current.setOpen(true));
    act(() => result.current.showResults(ROWS));
    await waitFor(() => expect(result.current.results).toHaveLength(1));

    act(() => result.current.setOpen(false));
    await waitFor(() => expect(result.current.results).toEqual([]));
    expect(result.current.open).toBe(false);

    // …and the same through `close`, which is the fully-successful path.
    act(() => result.current.setOpen(true));
    act(() => result.current.showResults(ROWS));
    act(() => result.current.close());
    await waitFor(() => expect(result.current.results).toEqual([]));
    expect(result.current.open).toBe(false);
  });

  it('clears the rows without closing, which is what a new submit does', async () => {
    const { Wrapper } = makeWrapper('/settings/users');
    const { result } = renderHook(() => useInviteDialog(), { wrapper: Wrapper });
    // The RouterProvider mounts its children on a microtask, so the hook has
    // not run on the first tick and `result.current` is undefined until it has.
    await waitFor(() => expect(result.current).toBeTruthy());

    act(() => result.current.setOpen(true));
    act(() => result.current.showResults(ROWS));
    act(() => result.current.clearResults());

    await waitFor(() => expect(result.current.results).toEqual([]));
    expect(result.current.open).toBe(true);
  });

  it('opens on the ?inviteUsers=1 deep link and strips the flag so a reload does not re-open it', async () => {
    const { Wrapper, router } = makeWrapper('/settings/users?inviteUsers=1');
    const { result } = renderHook(() => useInviteDialog(), { wrapper: Wrapper });

    await waitFor(() => expect(result.current.open).toBe(true));
    await waitFor(() => expect(router.state.location.search.inviteUsers).toBeUndefined());
  });
});
