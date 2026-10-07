import { useState } from 'react';
import { Alert, RefreshControl } from 'react-native';

import { deviceErrorText, revokeDevice, useDevices, useForgetDevice, type NativeDevice } from '@/api/queries/devices';
import { useDesign } from '@/design/DesignProvider';
import { displayHost } from '@/features/connect/connectFlow';
import { DevicesView } from '@/features/settings/DevicesView';
import { formatAge } from '@/lib/time';
import { getSession, useSession } from '@/state/session';
import { haptic } from '@/ui';

/**
 * Settings → Signed-in devices: the caller's device sessions on this workspace (listNativeDevices),
 * with sign-out for the others (revokeNativeDevice) after a destructive confirmation. Online
 * only; the list lives in memory.
 */
export default function DevicesScreen() {
  const { colors } = useDesign();
  const origin = useSession((s) => s.serverOrigin);
  const devices = useDevices(origin !== null);
  const forget = useForgetDevice();
  const [note, setNote] = useState<string | null>(null);
  const rows = devices.data ?? [];
  const host = origin ? displayHost(origin) : 'this workspace';
  const banner = note ?? (devices.error && rows.length > 0 ? deviceErrorText(devices.error) : null);

  const onRevoke = (device: NativeDevice): Promise<boolean> =>
    new Promise((resolve) => {
      // The Alert outlives a workspace switch: a stale confirmation signs nothing out over there.
      const offeredFor = getSession().workspaceId;
      Alert.alert(
        `Sign out ${device.name}?`,
        `It loses access to ${host} at once. The app on it erases this workspace's data when it next opens.`,
        [
          { text: 'Cancel', style: 'cancel', onPress: () => resolve(false) },
          {
            text: 'Sign out',
            style: 'destructive',
            onPress: () => {
              if (getSession().workspaceId !== offeredFor) {
                resolve(false);
                return;
              }
              revokeDevice(device.id)
                .then(() => forget(device.id))
                .then(() => {
                  haptic('swipeActionThreshold');
                  setNote(null);
                  resolve(true);
                })
                .catch((error: unknown) => {
                  setNote(deviceErrorText(error, 'revoke'));
                  resolve(false);
                });
            },
          },
        ],
        { cancelable: true, onDismiss: () => resolve(false) },
      );
    });

  return (
    <DevicesView
      status={devices.isLoading ? { kind: 'loading' } : devices.error ? { kind: 'error', message: deviceErrorText(devices.error) } : { kind: 'ready' }}
      devices={rows}
      host={host}
      age={(iso) => formatAge(iso)}
      banner={banner}
      onRevoke={onRevoke}
      onRetry={() => void devices.refetch()}
      refreshControl={<RefreshControl refreshing={devices.isRefetching} onRefresh={() => void devices.refetch()} tintColor={colors.accent} />}
    />
  );
}
