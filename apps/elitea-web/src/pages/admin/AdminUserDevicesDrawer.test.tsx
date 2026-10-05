/**
 * Admin › Users → Devices (ADR-0025 WP3), mounted through the real Users page
 * so the WIRING is under test: the row control exists for an operator holding
 * `admin.auth.users`, opens the drawer for THAT row's user, lists every device
 * (live and revoked) and revokes through the admin route after a confirmation.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminUsers } from './Users';
import { renderAdminRoute } from './__tests__/testRouter';

const USERS_BODY = {
  rows: [
    { id: 11, name: 'Ada Admin', email: 'ada@example.com', last_login: null, suspended: false, is_admin: true, admin_role: 'admin' },
    { id: 12, name: 'Cy Client', email: 'cy@example.com', last_login: null, suspended: false, is_admin: false, admin_role: null },
  ],
  total: 2,
  counts: { platform: 2, system: 0 },
};

const DEVICES = [
  {
    id: '6f1c2b1e-0000-4000-8000-000000000001',
    client_id: 'com.example.mobile',
    client_name: 'Example Mobile',
    device_name: 'Cy’s phone',
    platform: 'ios',
    client_version: '1.4.0',
    created_at: '2026-09-01T10:00:00Z',
    last_seen_at: '2026-10-01T10:00:00Z',
    revoked_at: null,
    revoke_reason: null,
    current: false,
    user_id: 12,
    email: 'cy@example.com',
  },
  {
    id: '6f1c2b1e-0000-4000-8000-000000000002',
    client_id: 'com.example.desktop',
    client_name: 'Example Desktop',
    device_name: 'Old laptop',
    platform: 'windows',
    client_version: '',
    created_at: '2026-08-01T10:00:00Z',
    last_seen_at: '2026-08-02T10:00:00Z',
    revoked_at: '2026-08-03T10:00:00Z',
    revoke_reason: 'client_disabled',
    current: false,
    user_id: 12,
    email: 'cy@example.com',
  },
];

let listUrls: string[] = [];
let revoked: string[] = [];

beforeEach(() => {
  listUrls = [];
  revoked = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/auth_users/administration', () => HttpResponse.json(USERS_BODY)),
    http.get('*/admin/native_devices/administration', ({ request }) => {
      listUrls.push(request.url);
      const state = new URL(request.url).searchParams.get('state');
      const rows = DEVICES.filter((d) => (state === 'revoked' ? d.revoked_at !== null : d.revoked_at === null));
      return HttpResponse.json({ rows, total: rows.length });
    }),
    http.delete('*/admin/native_devices/administration/:deviceId', ({ params }) => {
      revoked.push(String(params['deviceId']));
      return new HttpResponse(null, { status: 204 });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  delete window.admin_ui_config;
});

describe('Admin › Users → Devices', () => {
  it('is offered only to an operator holding admin.auth.users', async () => {
    window.admin_ui_config = { permissions: [], vite_server_url: '/api/v2' };
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Cy Client');
    expect(screen.queryByRole('button', { name: 'Devices' })).not.toBeInTheDocument();
  });

  it('opens for the clicked user, lists live and revoked devices, and revokes after confirming', async () => {
    window.admin_ui_config = { permissions: ['admin.auth.users'], vite_server_url: '/api/v2' };
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Cy Client');

    await user.click(screen.getByTestId('admin-user-devices-12'));

    const drawer = await screen.findByTestId('admin-user-devices-drawer');
    expect(within(drawer).getByText('Cy Client (ID: 12)')).toBeVisible();
    expect(await within(drawer).findByText('Cy’s phone')).toBeVisible();
    await waitFor(() => expect(listUrls.length).toBeGreaterThanOrEqual(2));
    const queries = listUrls.map((url) => new URL(url).searchParams);
    expect(queries.every((query) => query.get('user_id') === '12')).toBe(true);
    // Live devices are paged in full; revoked history is a separate request.
    expect(queries.map((query) => query.get('state'))).toEqual(['active', 'revoked']);

    // The revoked device is listed with its reason and offers no Revoke.
    const revokedRow = within(drawer).getByTestId(`native-device-row-${DEVICES[1]!.id}`);
    expect(within(revokedRow).getByText('App disabled by an administrator')).toBeVisible();
    expect(within(revokedRow).queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Revoke Cy’s phone' }));
    const confirm = await screen.findByTestId('native-device-revoke-dialog');
    expect(confirm).toHaveTextContent('wipes its local data the next time it contacts the server');
    // Before the click nothing has happened yet: the body says what WILL.
    expect(confirm).toHaveTextContent('will be signed out at once');
    expect(revoked).toHaveLength(0);

    await user.click(within(confirm).getByTestId('native-device-revoke-confirm'));
    await waitFor(() => expect(revoked).toEqual([DEVICES[0]!.id]));
  });
  // Regression: a failed revoke showed the server's machine word.
  it('reports a failed revoke in words, not the server code', async () => {
    window.admin_ui_config = { permissions: ['admin.auth.users'], vite_server_url: '/api/v2' };
    server.use(
      http.delete('*/admin/native_devices/administration/:deviceId', () =>
        HttpResponse.json({ error: 'store_unavailable' }, { status: 503 }),
      ),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Cy Client');
    await user.click(screen.getByTestId('admin-user-devices-12'));
    const drawer = await screen.findByTestId('admin-user-devices-drawer');
    await user.click(await within(drawer).findByRole('button', { name: 'Revoke Cy’s phone' }));
    await user.click(within(await screen.findByTestId('native-device-revoke-dialog')).getByTestId('native-device-revoke-confirm'));

    const alert = await within(drawer).findByTestId('admin-user-devices-revoke-error');
    expect(alert).toHaveTextContent('Failed to revoke that device.');
    expect(alert).not.toHaveTextContent('store_unavailable');
  });
  // Regression (PR #1051 review F1): the drawer asked for ONE page of
  // state=all (limit 200, ordered by last_seen_at DESC) and ignored `total`.
  // Revoked rows are never purged, so an account with more than 200 newer
  // revoked sessions pushed a still-live, idle device past row 200: it was not
  // listed, could not be revoked here, and nothing said the list was cut.
  it('always lists every live device, and says when older revoked history is cut', async () => {
    window.admin_ui_config = { permissions: ['admin.auth.users'], vite_server_url: '/api/v2' };
    const live = { ...DEVICES[0]!, device_name: 'Forgotten tablet', last_seen_at: '2026-01-01T00:00:00Z' };
    const history = Array.from({ length: 250 }, (_, i) => ({
      ...DEVICES[1]!,
      id: `6f1c2b1e-0000-4000-8000-${String(1000 + i).padStart(12, '0')}`,
      device_name: `Re-sign-in ${i}`,
      last_seen_at: new Date(Date.UTC(2026, 8, 1) + i * 60_000).toISOString(),
      revoked_at: '2026-09-30T00:00:00Z',
      revoke_reason: 'refresh_reuse',
    }));
    const all = [live, ...history];
    // The server's semantics (internal/nativeauth/devices.go AdminDevices):
    // state filter, total counted before paging, limit capped at 200,
    // ordered by last_seen_at DESC.
    server.use(
      http.get('*/admin/native_devices/administration', ({ request }) => {
        const query = new URL(request.url).searchParams;
        const state = query.get('state') ?? 'active';
        const matching = all
          .filter((d) => (state === 'all' ? true : state === 'revoked' ? d.revoked_at !== null : d.revoked_at === null))
          .sort((a, b) => b.last_seen_at.localeCompare(a.last_seen_at));
        let limit = Number(query.get('limit') ?? '50');
        if (!(limit > 0 && limit <= 200)) limit = 50;
        const offset = Math.max(0, Number(query.get('offset') ?? '0'));
        return HttpResponse.json({ rows: matching.slice(offset, offset + limit), total: matching.length });
      }),
    );
    const user = userEvent.setup();
    renderAdminRoute(<AdminUsers />);
    await screen.findByText('Cy Client');
    await user.click(screen.getByTestId('admin-user-devices-12'));
    const drawer = await screen.findByTestId('admin-user-devices-drawer');

    expect(await within(drawer).findByRole('button', { name: 'Revoke Forgotten tablet' })).toBeVisible();
    expect(within(drawer).getByTestId('admin-user-devices-truncated')).toHaveTextContent(
      'Showing the 200 most recent of 250 revoked devices.',
    );
  });
});
