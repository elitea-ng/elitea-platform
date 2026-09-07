/**
 * The boot-time admin access gate (smoke finding: a non-admin who opened
 * `/admin/app` saw the full console shell, because `router.tsx` mounted the
 * route tree unconditionally).
 *
 * This mounts the REAL entry composition — `AdminApp`, which is exactly what
 * `src/entries/admin/main.tsx` renders — rather than the router directly the
 * way `AdminNav.test.tsx` does, because the property under test is "the
 * router never mounts for a refused caller", and a test that builds the
 * router itself cannot see that gate skip its own construction.
 *
 * `window.admin_ui_config.permissions` is the ONLY source this reads (there
 * is no live "my administration permissions" endpoint — see `adminUiConfig.ts`
 * and `services/elitea-main/internal/api/adminui/handler.go`: the Go handler
 * injects it once into the served HTML, before this bundle's script tag, so
 * there is nothing here for msw to intercept). Both "denied" cases below —
 * an explicit empty list and a wholly absent config — assert the SAME
 * outcome, because the server already fails closed to `[]` on any resolver
 * error (`handler.go`'s `resolvePermissions`): a client that told those two
 * cases apart would have to invent a distinction the server does not make.
 */
import { cleanup, configure, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { adminNavGroups } from '@/pages/admin/adminNavItems';

import { AdminApp } from './AdminApp';

// This file mounts the real router through `AdminApp` (lazy route chunks
// included), same as `pages/pipelines/EditPipeline.test.tsx`: both the
// async-util wait and vitest's own per-test limit need headroom under CI
// coverage instrumentation.
configure({ asyncUtilTimeout: 5_000 });
vi.setConfig({ testTimeout: 30_000 });

const ALL_PERMISSIONS = adminNavGroups().flatMap((group) => group.items.flatMap((item) => item.anyPermission));

interface AdminUiConfigWindow {
  admin_ui_config?: { permissions?: readonly string[] };
}

afterEach(() => {
  cleanup();
  delete (window as unknown as AdminUiConfigWindow).admin_ui_config;
});

describe('AdminApp boot gate', () => {
  it('mounts the admin router and sidebar for a caller with an admin permission', async () => {
    (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: ALL_PERMISSIONS };

    render(<AdminApp />);

    expect(await screen.findByTestId('admin-nav')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-access-denied')).not.toBeInTheDocument();
  });

  it('renders the 403 page, not the router, for a caller with an empty permission list', async () => {
    (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: [] };

    render(<AdminApp />);

    expect(await screen.findByTestId('admin-access-denied')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-nav')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Access denied' })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Back to the app' })).toHaveAttribute('href', '/app/');
  });

  it('renders the 403 page for a caller the handler never resolved at all', async () => {
    // No `window.admin_ui_config` at all — `readAdminUiConfig()`'s EMPTY
    // fallback. This is what a permission-resolution failure looks like from
    // this bundle's side (handler.go returns `[]` on every error), so it must
    // fail the same way as an explicit empty list, not render the console.
    render(<AdminApp />);

    expect(await screen.findByTestId('admin-access-denied')).toBeInTheDocument();
    expect(screen.queryByTestId('admin-nav')).not.toBeInTheDocument();
  });

  it('never constructs the admin route tree for a refused caller', async () => {
    (window as unknown as AdminUiConfigWindow).admin_ui_config = { permissions: [] };

    render(<AdminApp />);
    await screen.findByTestId('admin-access-denied');

    // Give any lazy route chunk a tick to have mounted if it were going to.
    await waitFor(() => {
      expect(screen.queryByRole('heading', { name: 'Users' })).not.toBeInTheDocument();
    });
  });
});
