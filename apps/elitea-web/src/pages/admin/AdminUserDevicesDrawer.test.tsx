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
      return HttpResponse.json({ rows: DEVICES, total: DEVICES.length });
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
    await waitFor(() => expect(listUrls).toHaveLength(1));
    const query = new URL(listUrls[0]!).searchParams;
    expect(query.get('user_id')).toBe('12');
    expect(query.get('state')).toBe('all');

    // The revoked device is listed with its reason and offers no Revoke.
    const revokedRow = within(drawer).getByTestId(`native-device-row-${DEVICES[1]!.id}`);
    expect(within(revokedRow).getByText('App disabled by an administrator')).toBeVisible();
    expect(within(revokedRow).queryByRole('button', { name: /Revoke/ })).not.toBeInTheDocument();

    await user.click(within(drawer).getByRole('button', { name: 'Revoke Cy’s phone' }));
    const confirm = await screen.findByTestId('native-device-revoke-dialog');
    expect(confirm).toHaveTextContent('wipes its local data the next time it contacts the server');
    expect(revoked).toHaveLength(0);

    await user.click(within(confirm).getByTestId('native-device-revoke-confirm'));
    await waitFor(() => expect(revoked).toEqual([DEVICES[0]!.id]));
  });
});
