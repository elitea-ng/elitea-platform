/**
 * Mounts a minimal standalone route tree replicating the REAL registered
 * route's id (`/_shell/mcp-auth-callback`, `src/routeTree.gen.ts:633`) so
 * `getRouteApi('/_shell/mcp-auth-callback').useSearch()` resolves for real,
 * without importing R1's own route file (out of this unit's owned paths;
 * `src/routes/**` belongs to R1) — spec §6.2 "no router mocking", satisfied
 * by using the REAL `@tanstack/react-router` runtime, just with a
 * hand-built tree instead of the app's full one.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { installWebStorageShim } from '@/test/webstorage';

installWebStorageShim();

import { createStorage } from '@/shared/lib/storage';

import { McpAuthCallbackPage } from './McpAuthCallbackPage';

interface SearchShape {
  code: string;
  state: string;
  error: string;
  error_description: string;
}

function toText(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function buildRouter(initialPath: string) {
  const rootRoute = createRootRoute();
  const shellRoute = createRoute({ getParentRoute: () => rootRoute, id: '_shell' });
  const mcpAuthCallbackRoute = createRoute({
    getParentRoute: () => shellRoute,
    path: '/mcp-auth-callback',
    validateSearch: (search: Record<string, unknown>): SearchShape => ({
      code: toText(search.code),
      state: toText(search.state),
      error: toText(search.error),
      error_description: toText(search.error_description),
    }),
    component: McpAuthCallbackPage,
  });
  const routeTree = rootRoute.addChildren([shellRoute.addChildren([mcpAuthCallbackRoute])]);
  const history = createMemoryHistory({ initialEntries: [initialPath] });
  return createRouter({ routeTree, history });
}

function mountAt(path: string) {
  const router = buildRouter(path);
  render(<RouterProvider router={router} />);
  return router;
}

afterEach(() => {
  // Unconditional, regardless of whether a test's own vi.useRealTimers()
  // call was reached — a failed assertion mid-test with fake timers still
  // active would otherwise leak them into the NEXT test's router mount
  // (TanStack Router's own internal scheduling depends on real timers).
  vi.useRealTimers();
  cleanup();
  window.localStorage.clear();
  vi.restoreAllMocks();
});

describe('McpAuthCallbackPage', () => {
  it('with a code param: relays {success:true, code} to the opener via postMessage and closes the window', async () => {
    const postMessage = vi.fn();
    vi.stubGlobal('opener', { closed: false, postMessage });
    const close = vi.spyOn(window, 'close').mockImplementation(() => {});
    vi.useFakeTimers();

    mountAt('/mcp-auth-callback?code=auth-code-1&state=state-xyz');
    await vi.waitFor(() => expect(postMessage).toHaveBeenCalled());

    expect(postMessage).toHaveBeenCalledWith(
      { type: 'mcp-auth-result', state: 'state-xyz', success: true, code: 'auth-code-1' },
      window.location.origin,
    );
    // The page posts first and sets its state after, so the copy lands a
    // render later than the postMessage.
    await vi.waitFor(() => expect(screen.getByText(/Authorization successful/i)).toBeInTheDocument());

    vi.advanceTimersByTime(1000);
    expect(close).toHaveBeenCalled();
    vi.useRealTimers();
  });

  it('with an error param: relays {error, error_description} and shows the failure state', async () => {
    const postMessage = vi.fn();
    vi.stubGlobal('opener', { closed: false, postMessage });

    mountAt('/mcp-auth-callback?error=access_denied&error_description=User+declined&state=s1');
    await waitFor(() => expect(postMessage).toHaveBeenCalled());

    expect(postMessage).toHaveBeenCalledWith(
      { type: 'mcp-auth-result', state: 's1', error: 'access_denied', error_description: 'User declined' },
      window.location.origin,
    );
    expect(await screen.findByText('Authorization failed')).toBeInTheDocument();
    expect(screen.getByText('User declined')).toBeInTheDocument();
  });

  it('with neither code nor error: reports invalid_request and shows the generic failure copy', async () => {
    const postMessage = vi.fn();
    vi.stubGlobal('opener', { closed: false, postMessage });

    mountAt('/mcp-auth-callback?state=s2');
    await waitFor(() => expect(postMessage).toHaveBeenCalled());

    expect(postMessage).toHaveBeenCalledWith(
      { type: 'mcp-auth-result', state: 's2', error: 'invalid_request', error_description: 'Missing authorization code' },
      window.location.origin,
    );
    expect(await screen.findByText('Invalid authorization response')).toBeInTheDocument();
  });

  it('also writes the result to localStorage (state-scoped key) as the cross-tab fallback channel', async () => {
    vi.stubGlobal('opener', undefined);

    mountAt('/mcp-auth-callback?code=abc&state=state-storage-test');
    await waitFor(() => {
      const stored = createStorage('local').getJSON<{ code?: string }>('mcp-auth-result-state-storage-test');
      expect(stored?.code).toBe('abc');
    });
  });

  it('does not crash when window.opener is null (popup opened without an opener)', async () => {
    vi.stubGlobal('opener', null);
    mountAt('/mcp-auth-callback?code=no-opener-code&state=s3');
    expect(await screen.findByText(/Authorization successful/i)).toBeInTheDocument();
  });

  it('falls back to the bare error code when error_description is absent', async () => {
    const postMessage = vi.fn();
    vi.stubGlobal('opener', { closed: false, postMessage });

    mountAt('/mcp-auth-callback?error=server_error&state=s4');
    await waitFor(() => expect(postMessage).toHaveBeenCalled());

    expect(await screen.findByText('server_error')).toBeInTheDocument();
  });

  it('auto-closes the window on an OAuth error, but ONLY outside dev mode (baseline: isDev gates this)', async () => {
    vi.stubEnv('DEV', false);
    const close = vi.spyOn(window, 'close').mockImplementation(() => {});
    vi.stubGlobal('opener', { closed: false, postMessage: vi.fn() });
    vi.useFakeTimers();

    mountAt('/mcp-auth-callback?error=access_denied&state=s5');
    await vi.waitFor(() => expect(screen.getByText('Authorization failed')).toBeInTheDocument());

    vi.advanceTimersByTime(2000);
    expect(close).toHaveBeenCalled();

    vi.useRealTimers();
    vi.unstubAllEnvs();
  });
});
