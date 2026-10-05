/**
 * The confirmation before a native device is revoked. Revoking is immediate on
 * the server and cannot be undone: the device's refresh-token family is dead,
 * and the app wipes its local data the next time it contacts the server (it
 * answers `device_revoked`). Signing in again from that device creates a new
 * device; it does not restore this one.
 */
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import type { NativeDevice } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export interface NativeDeviceRevokeDialogProps {
  /** The device to revoke, or `undefined` when the dialog is closed. */
  readonly device: NativeDevice | undefined;
  readonly busy: boolean;
  readonly onCancel: () => void;
  readonly onConfirm: (device: NativeDevice) => void;
}

export function NativeDeviceRevokeDialog({ device, busy, onCancel, onConfirm }: NativeDeviceRevokeDialogProps) {
  return (
    <Dialog open={device !== undefined} onClose={onCancel} maxWidth="xs" fullWidth data-testid="native-device-revoke-dialog">
      <DialogTitle>{t('shared.nativeDevices.revokeDialog.title', 'Revoke device')}</DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">
          {t(
            'shared.nativeDevices.revokeDialog.body',
            '“{{name}}” ({{app}}) is signed out now. The app wipes its local data the next time it contacts the server, and signing in again on that device registers it as a new device.',
            { name: device?.device_name ?? '', app: device?.client_name ?? '' },
          )}
        </Typography>
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel} disabled={busy} sx={{ textTransform: 'none' }}>
          {t('shared.nativeDevices.revokeDialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="elitea"
          color="alarm"
          disabled={busy || device === undefined}
          onClick={() => {
            if (device !== undefined) onConfirm(device);
          }}
          sx={{ textTransform: 'none' }}
          data-testid="native-device-revoke-confirm"
        >
          {t('shared.nativeDevices.revokeDialog.confirm', 'Revoke')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
