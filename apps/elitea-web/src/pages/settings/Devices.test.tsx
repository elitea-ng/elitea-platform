/**
 * Settings › Devices (ADR-0025 WP3): the caller's own native devices, through
 * the generated `auth` client and MSW.
 */
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { DevicesContent } from './Devices';

const BASE = '/api/v2';
const LIST_PATH = `${BASE}/auth/native/devices`;
const DEVICE_PATH = `${BASE}/auth/native/devices/:deviceId`;

const PHONE = {
  id: '0b6c0d7e-0000-4000-8000-0000000000aa',
  client_id: 'com.example.mobile',
  client_name: 'Example Mobile',
  device_name: 'Pixel 9',
  platform: 'android',
  client_version: '2.1.0',
  created_at: '2026-09-01T10:00:00Z',
  last_seen_at: '2026-10-04T10:00:00Z',
  revoked_at: null,
  revoke_reason: null,
  current: false,
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

function mount() {
  return render(
    <AppProviders>
      <DevicesContent />
    </AppProviders>,
  );
}

describe('Settings › Devices', () => {
  it('lists the caller’s devices and revokes one after confirming', async () => {
    let devices = [PHONE];
    const revoked: string[] = [];
    server.use(
      http.get(LIST_PATH, () => HttpResponse.json({ devices })),
      http.delete(DEVICE_PATH, ({ params }) => {
        revoked.push(String(params['deviceId']));
        devices = [];
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    mount();

    const row = await screen.findByTestId(`native-device-row-${PHONE.id}`);
    expect(within(row).getByText('Pixel 9')).toBeVisible();
    expect(within(row).getByText('Example Mobile')).toBeVisible();
    expect(within(row).getByText('Android')).toBeVisible();
    expect(within(row).getByText('2.1.0')).toBeVisible();

    await user.click(within(row).getByRole('button', { name: 'Revoke Pixel 9' }));
    const dialog = await screen.findByTestId('native-device-revoke-dialog');
    expect(revoked).toEqual([]);
    await user.click(within(dialog).getByTestId('native-device-revoke-confirm'));

    await waitFor(() => expect(revoked).toEqual([PHONE.id]));
    // The list is re-read, so the revoked device leaves the page.
    expect(await screen.findByTestId('settings-devices-empty')).toBeVisible();
  });

  it('explains how an app signs in when the caller has no devices', async () => {
    server.use(http.get(LIST_PATH, () => HttpResponse.json({ devices: [] })));
    mount();

    const empty = await screen.findByTestId('settings-devices-empty');
    expect(empty).toHaveTextContent('sign in with your workspace login');
  });

  it('reads 404 (no native client registered) as "none enabled yet", not as an error', async () => {
    let calls = 0;
    server.use(
      http.get(LIST_PATH, () => {
        calls += 1;
        return HttpResponse.json({ error: 'not_found' }, { status: 404 });
      }),
    );
    mount();

    const empty = await screen.findByTestId('settings-devices-empty');
    expect(empty).toHaveTextContent('once an administrator enables them');
    expect(screen.queryByText('Failed to load your devices.')).not.toBeInTheDocument();
    expect(calls).toBe(1);
  });
});
