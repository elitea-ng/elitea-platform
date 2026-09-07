import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Snackbar from '@mui/material/Snackbar';

import { t } from '@/shared/i18n';

/** The four outcomes the notifications toolbar reports. */
export type NotificationToastKind = 'deleted' | 'read' | 'unread' | 'error';

export interface NotificationsToastProps {
  /** `undefined` means "no toast on screen". */
  readonly kind?: NotificationToastKind | undefined;
  readonly onClose: () => void;
}

/** The reference's own strings (`NotificationTable.jsx:145-185`). */
function toastMessage(kind: NotificationToastKind | undefined): string {
  if (kind === 'deleted') {
    return t('routes.settings.notifications.toastDeleted', 'The selected notifications have been successfully deleted.');
  }
  if (kind === 'read') return t('routes.settings.notifications.toastRead', 'Notifications marked as read');
  if (kind === 'unread') return t('routes.settings.notifications.toastUnread', 'Notifications marked as unread');
  return t('routes.settings.notifications.toastError', 'The action could not be completed.');
}

/**
 * The result of a bulk mark-read or bulk delete.
 *
 * The reference toasts every outcome of both actions, success and failure
 * (`NotificationTable.jsx:145-185`). This page reported neither, so a
 * refused mutation looked exactly like a successful one (issue 841).
 *
 * It renders NOTHING while `kind` is undefined. MUI unmounts a closed
 * `Snackbar`, so the settled page carries no `role="alert"` node, which is
 * what journey J31a asserts.
 */
export function NotificationsToast({ kind, onClose }: NotificationsToastProps): ReactNode {
  return (
    <Snackbar
      open={kind !== undefined}
      autoHideDuration={3000}
      onClose={onClose}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
    >
      <Alert
        onClose={onClose}
        severity={kind === 'error' ? 'error' : 'success'}
        variant="filled"
      >
        {toastMessage(kind)}
      </Alert>
    </Snackbar>
  );
}
