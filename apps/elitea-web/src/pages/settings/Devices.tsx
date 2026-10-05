/**
 * Settings › Devices — the caller's own signed-in mobile and desktop apps
 * (ADR-0025 decision 4, WP3).
 *
 * Reads `GET /auth/native/devices` and revokes through
 * `DELETE /auth/native/devices/{id}` (reason `user`), both through the
 * generated `auth` client. Revoking is allowed for any of the caller's own
 * devices; another user's id answers 404 so ownership is never disclosed.
 *
 * ## A 404 on the LIST is a deployment fact, not an error
 *
 * Every `/auth/native/*` route answers 404 while no native client is
 * registered (coordinator decision 4). For this page that means "this
 * workspace has no app to sign in with", which is said as such — the same
 * calm empty state, not a red failure — and is not retried.
 */
import { memo, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import {
  getListNativeDevicesQueryKey,
  listNativeDevices,
  revokeNativeDevice,
} from '@/shared/api/generated/auth/auth';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import type { NativeDevice } from '@/shared/api/generated/model';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { NativeDeviceRevokeDialog, NativeDevicesTable } from '@/shared/ui/NativeDevicesTable';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';

function statusOf(error: unknown): number | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  return failure.kind === 'http' || failure.kind === 'auth' ? failure.status : undefined;
}

/** The list, with "no native client registered" (404) read as an empty answer. */
function useOwnNativeDevices() {
  return useQuery({
    queryKey: getListNativeDevicesQueryKey(),
    queryFn: async ({ signal }): Promise<{ devices: readonly NativeDevice[]; nativeEnabled: boolean }> => {
      try {
        // `eliteaFetch` resolves the transport envelope, not the body (#132).
        const body = unwrapBody(await listNativeDevices(undefined, { signal })) as
          | { devices?: NativeDevice[] }
          | undefined;
        return { devices: body?.devices ?? [], nativeEnabled: true };
      } catch (error) {
        if (statusOf(error) === 404) return { devices: [], nativeEnabled: false };
        throw error;
      }
    },
  });
}

function EmptyState({ nativeEnabled }: { readonly nativeEnabled: boolean }) {
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem', py: '1rem' }} data-testid="settings-devices-empty">
      <Typography variant="bodyMedium">
        {t('pages.settings.devices.empty.title', 'You are not signed in on any mobile or desktop app.')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {nativeEnabled
          ? t(
              'pages.settings.devices.empty.body',
              'To add one, open the Elitea app on your phone or computer and sign in with your workspace login. You approve the new device in a signed-in browser, and it then appears here.',
            )
          : t(
              'pages.settings.devices.empty.unavailable',
              'Mobile and desktop apps sign in with your workspace login once an administrator enables them for this workspace. None is enabled yet.',
            )}
      </Typography>
    </Box>
  );
}

export const DevicesContent = memo(function DevicesContent() {
  const queryClient = useQueryClient();
  const listQuery = useOwnNativeDevices();
  const revoke = useMutation({
    mutationFn: async (deviceId: string) => {
      await revokeNativeDevice(encodeURIComponent(deviceId));
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: getListNativeDevicesQueryKey() }),
  });
  const [pending, setPending] = useState<NativeDevice | undefined>(undefined);
  const [revokeFailed, setRevokeFailed] = useState(false);

  const devices = listQuery.data?.devices ?? [];
  const nativeEnabled = listQuery.data?.nativeEnabled ?? true;
  const showEmpty = listQuery.isSuccess && devices.length === 0;

  const handleConfirm = (device: NativeDevice): void => {
    setRevokeFailed(false);
    revoke.mutate(device.id, {
      onSettled: () => setPending(undefined),
      onError: () => setRevokeFailed(true),
    });
  };

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader title={t('pages.settings.devices.title', 'Devices')} />
      <Box sx={styles.content} data-testid="settings-devices">
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.settings.devices.description',
            'The mobile and desktop apps signed in to your account. Revoke a device you no longer use or do not recognise: it is signed out at once, and the app wipes its local data the next time it contacts the server.',
          )}
        </Typography>

        {listQuery.isLoading ? <LinearProgress /> : null}
        {listQuery.isError ? (
          <Alert severity="error">{t('pages.settings.devices.error.load', 'Failed to load your devices.')}</Alert>
        ) : null}
        {revokeFailed ? (
          <Alert severity="error" onClose={() => setRevokeFailed(false)}>
            {t('pages.settings.devices.error.revoke', 'Failed to revoke that device.')}
          </Alert>
        ) : null}

        {showEmpty ? <EmptyState nativeEnabled={nativeEnabled} /> : null}

        {devices.length > 0 ? (
          <NativeDevicesTable
            devices={devices}
            busyId={revoke.isPending ? revoke.variables : undefined}
            onRevoke={setPending}
            label={t('pages.settings.devices.tableLabel', 'Your signed-in devices')}
          />
        ) : null}
      </Box>

      <NativeDeviceRevokeDialog
        device={pending}
        busy={revoke.isPending}
        onCancel={() => setPending(undefined)}
        onConfirm={handleConfirm}
      />
    </Paper>
  );
});

const styles: Record<string, SxProps<Theme>> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    flex: 1,
    minHeight: 0,
    padding: '0 1.5rem 1.5rem',
    overflow: 'auto',
  },
};
