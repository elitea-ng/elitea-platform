/**
 * The per-user native device drawer (Admin › Users → the row's Devices control).
 *
 * Lists every mobile/desktop app session of one account — live and revoked,
 * because the status column is the point when an operator is looking at a
 * compromised account — and revokes any live one through
 * `DELETE /admin/native_devices/administration/{id}` (reason `admin`).
 *
 * Gated like user suspension (`admin.auth.users`): whoever may suspend an
 * account may cut off its devices. The server enforces it; the page only hides
 * the control from an operator who would be refused.
 */
import { useState } from 'react';

import CloseIcon from '@mui/icons-material/Close';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Drawer from '@mui/material/Drawer';
import IconButton from '@mui/material/IconButton';
import LinearProgress from '@mui/material/LinearProgress';
import Typography from '@mui/material/Typography';

import type { NativeDevice } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { NativeDeviceRevokeDialog, NativeDevicesTable } from '@/shared/ui/NativeDevicesTable';

import type { AdminUserRow } from './api/adminUsersApi';
import {
  isNativeServerAbsent,
  nativeClientFailure,
  useAdminUserNativeDevices,
  useRevokeAdminNativeDevice,
} from './api/adminNativeClientsApi';

export interface AdminUserDevicesDrawerProps {
  /** The user to describe, or `null` when the drawer is closed. */
  readonly user: AdminUserRow | null;
  readonly onClose: () => void;
}

export function AdminUserDevicesDrawer({ user, onClose }: AdminUserDevicesDrawerProps) {
  return (
    <Drawer
      anchor="right"
      open={user !== null}
      onClose={onClose}
      slotProps={{ paper: { sx: { width: { xs: '100%', md: '60vw' } } } }}
    >
      {/* Keyed on the user, so opening user B never shows user A's state. */}
      {user !== null ? <DevicesContent key={user.id} user={user} onClose={onClose} /> : null}
    </Drawer>
  );
}

function loadErrorSentence(error: unknown): string {
  if (isNativeServerAbsent(error)) {
    return t(
      'pages.admin.users.devices.absent',
      'This deployment does not serve native sign-in, so no account has mobile or desktop devices.',
    );
  }
  const failure = nativeClientFailure(error);
  return failure.message ?? failure.code ?? t('pages.admin.users.devices.loadError', 'Failed to load this user’s devices.');
}

function DevicesContent({ user, onClose }: { readonly user: AdminUserRow; readonly onClose: () => void }) {
  const devicesQuery = useAdminUserNativeDevices(user.id);
  const revoke = useRevokeAdminNativeDevice();
  const [pending, setPending] = useState<NativeDevice | undefined>(undefined);
  const [revokeError, setRevokeError] = useState<string | undefined>(undefined);

  const devices = devicesQuery.data ?? [];
  const showEmpty = !devicesQuery.isLoading && devicesQuery.error == null && devices.length === 0;

  const handleConfirm = (device: NativeDevice): void => {
    setRevokeError(undefined);
    revoke.mutate(device.id, {
      onSettled: () => setPending(undefined),
      onError: (error: unknown) => {
        const failure = nativeClientFailure(error);
        setRevokeError(
          failure.message ?? failure.code ?? t('pages.admin.users.devices.revokeError', 'Failed to revoke that device.'),
        );
      },
    });
  };

  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', height: '100%', padding: '1rem 1.25rem', gap: '0.75rem' }}
      data-testid="admin-user-devices-drawer"
    >
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: '1rem' }}>
        <Box>
          <Typography variant="headingSmall" component="h2">
            {t('pages.admin.users.devices.heading', 'Devices')}
          </Typography>
          <Typography variant="bodyMedium" color="text.secondary">
            {`${user.name || user.email} (ID: ${user.id})`}
          </Typography>
        </Box>
        <IconButton size="small" onClick={onClose} aria-label={t('pages.admin.users.devices.close', 'Close')}>
          <CloseIcon fontSize="small" />
        </IconButton>
      </Box>

      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.users.devices.description',
          'The mobile and desktop apps this account is signed in to. Revoking a device signs it out at once; the app wipes its local data the next time it contacts the server.',
        )}
      </Typography>

      {devicesQuery.isLoading ? <LinearProgress /> : null}
      {devicesQuery.error != null ? <Alert severity="warning">{loadErrorSentence(devicesQuery.error)}</Alert> : null}
      {revokeError !== undefined ? (
        <Alert severity="error" onClose={() => setRevokeError(undefined)}>
          {revokeError}
        </Alert>
      ) : null}

      {showEmpty ? (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-user-devices-empty">
          {t('pages.admin.users.devices.empty', 'This account has never signed in from a mobile or desktop app.')}
        </Typography>
      ) : null}

      {devices.length > 0 ? (
        <NativeDevicesTable
          devices={devices}
          busyId={revoke.isPending ? revoke.variables : undefined}
          onRevoke={setPending}
          label={t('pages.admin.users.devices.tableLabel', 'Devices of this account')}
        />
      ) : null}

      <NativeDeviceRevokeDialog
        device={pending}
        busy={revoke.isPending}
        onCancel={() => setPending(undefined)}
        onConfirm={handleConfirm}
      />
    </Box>
  );
}
