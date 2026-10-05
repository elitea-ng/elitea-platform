/**
 * "Reset to defaults" confirmation (ADR-0024 WP4). Confirming writes EVERY
 * key of the section as inherit — empty, 0 or `[]` — which is a full replace
 * of the database layer, not a discard of the draft, so it asks first.
 */
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';

import { t } from '@/shared/i18n';

export interface BrandingResetDialogProps {
  readonly open: boolean;
  readonly onCancel: () => void;
  readonly onConfirm: () => void;
}

export function BrandingResetDialog({ open, onCancel, onConfirm }: BrandingResetDialogProps) {
  return (
    <Dialog open={open} onClose={onCancel} data-testid="branding-reset-dialog">
      <DialogTitle>{t('pages.admin.branding.reset.title', 'Reset branding to defaults?')}</DialogTitle>
      <DialogContent>
        <DialogContentText>
          {t(
            'pages.admin.branding.reset.body',
            'Every branding field is cleared and inherits from the mounted file pack or the product default — including what a package import set beyond this page: the per-scheme colours and the e-mail sender name and support address. Uploaded assets nothing references any more are removed; kept package versions stay and can be imported again. Users see the change on their next page load.',
          )}
        </DialogContentText>
      </DialogContent>
      <DialogActions>
        <Button variant="secondary" size="small" onClick={onCancel}>
          {t('pages.admin.branding.reset.cancel', 'Cancel')}
        </Button>
        <Button variant="alarm" size="small" onClick={onConfirm} data-testid="branding-reset-confirm">
          {t('pages.admin.branding.reset.confirm', 'Reset to defaults')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
