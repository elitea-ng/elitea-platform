/**
 * DEFECT: "New chat" navigated to `/chat` with `?create=1`, and no
 * component read the flag.
 *
 * `widgets/chat-rail`'s `useStartChat` builds
 * `{ to: '/chat', search: { create: '1' } }`. A user already on `/chat` who
 * clicked it stayed on the same pathname. The route did not remount. The
 * previous conversation, its attachments and its streaming state all stayed on
 * screen, and no new chat began. The flag parsed correctly, because the parity
 * check only exercised the schema. An unread flag passed review that way.
 *
 * The tests below assert the two things a reader must do. It must give the
 * chat subtree a new `key`, so that the whole surface remounts. It must write
 * the flag back to `'0'`, so that a second click is a `'0' -> '1'` transition.
 */
import type { ReactNode } from 'react';

import { RouterProvider, createMemoryHistory, createRootRoute, createRoute, createRouter } from '@tanstack/react-router';
import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { useCreateChatReset } from './useCreateChatReset';

/**
 * The router's default search parser runs `JSON.parse` on every raw value, so
 * `?create=1` arrives as the NUMBER 1. `routes/-search/params.ts` normalises
 * that value back to a string for every flag. This `validateSearch` repeats
 * the same normalisation. The hook under test therefore sees the value that
 * production gives it.
 */
function validateCreateFlag(search: Record<string, unknown>): { create?: string } {
  const raw = search['create'];
  if (typeof raw === 'string') return { create: raw };
  if (typeof raw === 'number' || typeof raw === 'boolean') return { create: String(raw) };
  return {};
}

/**
 * A memory router with both chat routes. The hook navigates to `/chat` by
 * name, so `/chat` must exist. The "leave the conversation" case starts on
 * `/chat/$conversationId`. The probe renders under the matched route, which is
 * where the real composition root sits.
 */
function makeWrapper(initialEntry: string) {
  const rootRoute = createRootRoute();
  let render: (children: ReactNode) => ReactNode = (children) => children;
  const chatRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/chat',
    validateSearch: validateCreateFlag,
    component: () => render(null),
  });
  const conversationRoute = createRoute({
    getParentRoute: () => rootRoute,
    path: '/chat/$conversationId',
    validateSearch: validateCreateFlag,
    component: () => render(null),
  });
  const router = createRouter({
    routeTree: rootRoute.addChildren([chatRoute, conversationRoute]),
    history: createMemoryHistory({ initialEntries: [initialEntry] }),
  });

  function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    render = () => children;
    return <RouterProvider router={router as never} />;
  }

  return { Wrapper, router };
}

describe('useCreateChatReset', () => {
  it('does nothing while the flag is absent', async () => {
    const onReset = vi.fn();
    const { Wrapper } = makeWrapper('/chat');

    const { result } = renderHook(() => useCreateChatReset(onReset), { wrapper: Wrapper });

    await waitFor(() => expect(result.current).toBe(0));
    expect(onReset).not.toHaveBeenCalled();
  });

  it('bumps the reset token, closes the editors and clears the flag when create=1 arrives', async () => {
    const onReset = vi.fn();
    const { Wrapper, router } = makeWrapper('/chat?create=1');

    const { result } = renderHook(() => useCreateChatReset(onReset), { wrapper: Wrapper });

    await waitFor(() => expect(result.current).toBe(1));
    expect(onReset).toHaveBeenCalledTimes(1);
    // Written back to '0', and not deleted. The next click must be an
    // observable '0' -> '1' transition. The button navigates with a plain
    // object, which replaces the whole search.
    await waitFor(() => {
      expect(router.state.location.search.create).toBe('0');
    });
  });

  it('reacts again to a second click, so two chats in a row both reset', async () => {
    const onReset = vi.fn();
    const { Wrapper, router } = makeWrapper('/chat?create=1');

    const { result } = renderHook(() => useCreateChatReset(onReset), { wrapper: Wrapper });
    await waitFor(() => expect(result.current).toBe(1));

    await act(async () => {
      await router.navigate({ to: '/chat', search: { create: '1' } });
    });

    await waitFor(() => expect(result.current).toBe(2));
    expect(onReset).toHaveBeenCalledTimes(2);
  });

  it('leaves a conversation path and lands on bare /chat', async () => {
    const onReset = vi.fn();
    const { Wrapper, router } = makeWrapper('/chat/42?create=1');

    renderHook(() => useCreateChatReset(onReset), { wrapper: Wrapper });

    await waitFor(() => expect(router.state.location.pathname).toBe('/chat'));
  });

  it('fires once for one click even when the caller passes a new callback each render', async () => {
    const onReset = vi.fn();
    const { Wrapper } = makeWrapper('/chat?create=1');

    // ChatWithEditors rebuilds `closeEditors` on every render, because the
    // editor hooks return a fresh object each time. The hook must not treat
    // that new identity as a new click.
    const { result } = renderHook(
      () =>
        useCreateChatReset(() => {
          onReset();
        }),
      { wrapper: Wrapper },
    );

    await waitFor(() => expect(result.current).toBe(1));
    await waitFor(() => expect(onReset).toHaveBeenCalledTimes(1));
    expect(result.current).toBe(1);
    expect(onReset).toHaveBeenCalledTimes(1);
  });
});
