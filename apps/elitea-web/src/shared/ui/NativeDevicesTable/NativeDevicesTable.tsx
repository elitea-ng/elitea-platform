/**
 * A list of native device sessions (ADR-0025 WP3) with a Revoke control per
 * live device. Rendered by Settings › Devices (the user's own) and by the admin
 * Users › Devices drawer (any user's); the two differ only in which endpoint
 * they revoke through, so that is the one thing the caller supplies.
 */
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import type { NativeDevice } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import {
  formatNativeDeviceTime,
  isNativeDeviceActive,
  nativePlatformLabel,
  nativeRevokeReasonLabel,
} from '@/shared/lib/nativeDevices';

export interface NativeDevicesTableProps {
  readonly devices: readonly NativeDevice[];
  /** The device whose revoke is in flight; its control is disabled. */
  readonly busyId: string | undefined;
  readonly onRevoke: (device: NativeDevice) => void;
  /** Accessible name for the table. */
  readonly label: string;
}

function StatusCell({ device }: { readonly device: NativeDevice }) {
  if (isNativeDeviceActive(device)) {
    return (
      <Chip size="small" variant="outlined" color="success" label={t('shared.nativeDevices.status.active', 'Active')} />
    );
  }
  return (
    <>
      <Chip size="small" variant="outlined" label={t('shared.nativeDevices.status.revoked', 'Revoked')} />
      <Typography variant="bodySmall" color="text.secondary" component="div">
        {nativeRevokeReasonLabel(device.revoke_reason)}
      </Typography>
    </>
  );
}

export function NativeDevicesTable({ devices, busyId, onRevoke, label }: NativeDevicesTableProps) {
  return (
    <TableContainer>
      <Table size="small" aria-label={label} data-testid="native-devices-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('shared.nativeDevices.column.device', 'Device')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.app', 'App')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.platform', 'Platform')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.version', 'App version')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.created', 'Signed in')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.lastSeen', 'Last active')}</TableCell>
            <TableCell>{t('shared.nativeDevices.column.status', 'Status')}</TableCell>
            <TableCell align="right">{t('shared.nativeDevices.column.actions', 'Actions')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {devices.map((device) => {
            const active = isNativeDeviceActive(device);
            return (
              <TableRow key={device.id} hover data-testid={`native-device-row-${device.id}`}>
                <TableCell>
                  <Typography variant="bodyMedium">{device.device_name}</Typography>
                  {device.current ? (
                    <Chip size="small" color="primary" variant="outlined" label={t('shared.nativeDevices.current', 'This device')} />
                  ) : null}
                </TableCell>
                <TableCell>{device.client_name}</TableCell>
                <TableCell>{nativePlatformLabel(device.platform)}</TableCell>
                <TableCell>{device.client_version !== '' ? device.client_version : '—'}</TableCell>
                <TableCell>{formatNativeDeviceTime(device.created_at)}</TableCell>
                <TableCell>{formatNativeDeviceTime(device.last_seen_at)}</TableCell>
                <TableCell>
                  <StatusCell device={device} />
                </TableCell>
                <TableCell align="right">
                  {active ? (
                    <Button
                      variant="elitea"
                      color="alarm"
                      size="small"
                      disabled={busyId === device.id}
                      onClick={() => {
                        onRevoke(device);
                      }}
                      sx={{ textTransform: 'none' }}
                      aria-label={t('shared.nativeDevices.revokeLabel', 'Revoke {{name}}', { name: device.device_name })}
                    >
                      {t('shared.nativeDevices.revoke', 'Revoke')}
                    </Button>
                  ) : null}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}
