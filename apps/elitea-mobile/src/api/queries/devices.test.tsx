import { fireEvent, render, screen } from '@testing-library/react-native';

import { DeviceError, deviceErrorText, devicesOf, revokeDevice, type NativeDevice } from '@/api/queries/devices';
import { DevicesView, deviceMeta } from '@/features/settings/DevicesView';

const A = '11111111-1111-4111-8111-111111111111';
const B = '22222222-2222-4222-8222-222222222222';
const C = '33333333-3333-4333-8333-333333333333';

const row = (id: string, extra: Record<string, unknown> = {}) => ({
  id,
  client_id: 'ca.zefir.agent',
  client_name: 'Agent Zefir',
  device_name: 'iPhone',
  platform: 'ios',
  client_version: '0.1.0',
  created_at: '2026-10-01T10:00:00Z',
  last_seen_at: '2026-10-06T10:00:00Z',
  revoked_at: null,
  revoke_reason: null,
  current: false,
  ...extra,
});

describe('devicesOf', () => {
  it('puts this device first, drops revoked and malformed rows, and bounds the names', () => {
    const list = devicesOf({
      devices: [
        row(A, { device_name: 'Pixel', platform: 'android' }),
        row(B, { current: true }),
        row(C, { revoked_at: '2026-10-05T00:00:00Z' }),
        { id: 'not-a-uuid' },
        row(A),
        row('44444444-4444-4444-8444-444444444444', { device_name: `  long\n${'x'.repeat(200)}`, platform: 'windows' }),
      ],
    });
    expect(list.map((d) => d.id)).toEqual([B, A, '44444444-4444-4444-8444-444444444444']);
    expect(list[1]).toMatchObject({ name: 'Pixel', platform: 'android', current: false });
    expect(list[2].name.length).toBe(80);
    expect(list[2].platform).toBe('other');
  });

  it('a body that is not a device list is a failure, not an empty list', () => {
    expect(() => devicesOf({ rows: [] })).toThrow(DeviceError);
  });

  it('never sends a revoke for an id that is not a uuid', async () => {
    const client = { DELETE: jest.fn() };
    await expect(revokeDevice('../me', client as never)).rejects.toBeInstanceOf(DeviceError);
    expect(client.DELETE).not.toHaveBeenCalled();
  });

  it('says what went wrong in words', () => {
    expect(deviceErrorText(new DeviceError('offline'))).toBe('Devices need a connection to the workspace.');
    expect(deviceErrorText(new DeviceError('not_available'), 'revoke')).toBe('That device is already signed out.');
  });
});

describe('DevicesView', () => {
  const devices: NativeDevice[] = devicesOf({
    devices: [row(B, { current: true }), row(A, { device_name: 'Pixel 9', platform: 'android', client_version: '0.1.0' })],
  });
  const view = (onRevoke = jest.fn(async () => true)) =>
    render(
      <DevicesView status={{ kind: 'ready' }} devices={devices} host="acme.elitea.ai" age={() => '1d'} banner={null} onRevoke={onRevoke} onRetry={() => {}} />,
    );

  it('shows this device apart, with no sign-out of its own', async () => {
    await view();
    const mine = screen.getByLabelText('iPhone, this device, iOS · Agent Zefir 0.1.0, Active now');
    expect(mine.props.accessibilityActions).toBeUndefined();
  });

  it('another device signs out through its row action (the swipe path), host named in the footer', async () => {
    const onRevoke = jest.fn(async () => true);
    await view(onRevoke);
    const other = screen.getByLabelText('Pixel 9, Android · Agent Zefir 0.1.0, Last active 1d');
    expect(other.props.accessibilityActions).toEqual([{ name: 'delete', label: 'Sign out' }]);
    await fireEvent(other, 'accessibilityAction', { nativeEvent: { actionName: 'delete' } });
    expect(onRevoke).toHaveBeenCalledWith(expect.objectContaining({ id: A }));
    expect(screen.getByText(/ends its access to acme\.elitea\.ai/)).toBeTruthy();
  });

  it('formats the meta line', () => {
    expect(deviceMeta({ platform: 'other', clientVersion: '' })).toBe('Other');
  });
});
