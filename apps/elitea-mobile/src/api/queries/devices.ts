import { useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';

import { sessionApiClient, type ApiClient } from '@/api/client';
import { isApiError, unwrap } from '@/api/errors';

/**
 * The caller's signed-in devices (listNativeDevices, revokeNativeDevice; ADR-0025 decision 4):
 * one row per device session (refresh-token family) of this workspace, newest first, the one
 * this app is using marked `current`. Read online when Settings → Devices opens and kept in
 * memory only (the query cache, cleared with the session); nothing is written to the device.
 */

export interface NativeDevice {
  id: string;
  /** The name the app claimed at sign-in ("iPhone"); never a free-form value from elsewhere. */
  name: string;
  platform: 'ios' | 'android' | 'other';
  clientVersion: string;
  createdAt: string;
  lastSeenAt: string;
  /** The device whose token authenticated the request: this one. */
  current: boolean;
}

export type DeviceFailure = 'offline' | 'not_available' | 'failed';

export class DeviceError extends Error {
  constructor(readonly reason: DeviceFailure) {
    super(`devices: ${reason}`);
    this.name = 'DeviceError';
  }
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const line = (max: number) =>
  z
    .string()
    .max(1000)
    .transform((s) => s.replace(/\s+/g, ' ').trim().slice(0, max));

const Row = z.looseObject({
  id: z.string().regex(UUID),
  device_name: line(80).nullish(),
  client_name: line(80).nullish(),
  platform: z.string().max(32).nullish(),
  client_version: line(32).nullish(),
  created_at: z.string().max(64).nullish(),
  last_seen_at: z.string().max(64).nullish(),
  revoked_at: z.string().max(64).nullish(),
  current: z.boolean().optional(),
});
const Envelope = z.looseObject({ devices: z.array(z.unknown()).max(500) });

/** The list as the app shows it: revoked and malformed rows are dropped, this device first. */
export function devicesOf(data: unknown): NativeDevice[] {
  const envelope = Envelope.safeParse(data);
  if (!envelope.success) throw new DeviceError('failed');
  const out: NativeDevice[] = [];
  for (const raw of envelope.data.devices) {
    const row = Row.safeParse(raw);
    if (!row.success || row.data.revoked_at || out.some((d) => d.id === row.data.id)) continue;
    const platform = row.data.platform === 'ios' || row.data.platform === 'android' ? row.data.platform : 'other';
    out.push({
      id: row.data.id.toLowerCase(),
      name: row.data.device_name || row.data.client_name || 'Unnamed device',
      platform,
      clientVersion: row.data.client_version ?? '',
      createdAt: row.data.created_at ?? '',
      lastSeenAt: row.data.last_seen_at ?? '',
      current: row.data.current === true,
    });
  }
  return [...out.filter((d) => d.current), ...out.filter((d) => !d.current)];
}

function failureOf(error: unknown): DeviceError {
  if (error instanceof DeviceError) return error;
  if (isApiError(error)) {
    if (error.kind === 'network' || error.kind === 'timeout') return new DeviceError('offline');
    if (error.status === 404 || error.status === 501) return new DeviceError('not_available');
  }
  return new DeviceError('failed');
}

export function deviceErrorText(error: unknown, action: 'load' | 'revoke' = 'load'): string {
  switch (error instanceof DeviceError ? error.reason : 'failed') {
    case 'offline':
      return 'Devices need a connection to the workspace.';
    case 'not_available':
      return action === 'revoke' ? 'That device is already signed out.' : 'This workspace does not list devices for this sign-in.';
    default:
      return action === 'revoke' ? "That device wasn't signed out. Try again." : "Couldn't load your devices. Try again.";
  }
}

export async function fetchDevices(client: ApiClient = sessionApiClient(), signal?: AbortSignal): Promise<NativeDevice[]> {
  try {
    return devicesOf(await unwrap(client.GET('/auth/native/devices', { signal })));
  } catch (error) {
    throw failureOf(error);
  }
}

/** Signs another device out (the server revokes its tokens; its next call is refused). */
export async function revokeDevice(deviceId: string, client: ApiClient = sessionApiClient()): Promise<void> {
  if (!UUID.test(deviceId)) throw new DeviceError('failed');
  try {
    await unwrap(client.DELETE('/auth/native/devices/{device_id}', { params: { path: { device_id: deviceId } } }));
  } catch (error) {
    throw failureOf(error);
  }
}

const DEVICES_KEY = ['devices'] as const;

export function useDevices(enabled: boolean) {
  return useQuery({
    queryKey: DEVICES_KEY,
    queryFn: ({ signal }) => fetchDevices(sessionApiClient(), signal),
    enabled,
    retry: false,
    gcTime: 0,
  });
}

/** Drops a revoked device from the cached list (a fetch in flight is cancelled first). */
export function useForgetDevice() {
  const queryClient = useQueryClient();
  return async (deviceId: string): Promise<void> => {
    await queryClient.cancelQueries({ queryKey: DEVICES_KEY });
    queryClient.setQueryData<NativeDevice[]>(DEVICES_KEY, (rows) => rows?.filter((d) => d.id !== deviceId));
  };
}
