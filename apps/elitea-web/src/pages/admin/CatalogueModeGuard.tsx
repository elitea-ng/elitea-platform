/**
 * The warning before the connector catalogue switches to approved_only
 * (ADR-0044 decision 1, #1501). approved_only refuses every connector no
 * approved request names, including the toolkits already configured, so the
 * Configuration page's `catalogue` section counts the connectors in use with
 * no approved request (the server's inventory) and asks for confirmation
 * before saving the switch while any remain. The count needs
 * `connector.approve`; a caller without it (or a failed read) saves with no
 * warning, as before.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { useConnectorInventory } from './api/adminConnectorRequestsApi';

export const CATALOGUE_SECTION_ID = 'catalogue';

export interface CatalogueModeGuard {
  /** The inline warning, while approved_only is chosen and connectors would be refused. */
  readonly warning: ReactNode;
  /** The confirmation dialog, while it is open. */
  readonly dialog: ReactNode;
  /** Save, or ask first. */
  readonly onSave: () => void;
}

/** Wraps a section's save: for the catalogue section switching to approved_only, warn with the count first. */
export function useCatalogueModeGuard(
  sectionId: string | undefined,
  values: Readonly<Record<string, unknown>>,
  isDirty: boolean,
  save: () => void,
): CatalogueModeGuard {
  const [asking, setAsking] = useState(false);
  const switching = sectionId === CATALOGUE_SECTION_ID && isDirty && values['mode'] === 'approved_only';
  const inventory = useConnectorInventory(switching);
  const missing = switching ? (inventory.data?.without_approval ?? 0) : 0;

  const message = t(
    'pages.admin.configuration.catalogue.unapprovedWarning',
    '{{count}} connector(s) in use have no approved request. Approved only refuses them, including toolkits already configured, until a request for each is approved.',
    { count: missing },
  );
  const hint = t(
    'pages.admin.configuration.catalogue.createRequests',
    'Use "Create requests for connectors in use" under Connector Requests, and have another approver decide them, before switching.',
  );

  const warning =
    missing > 0 ? (
      <Alert severity="warning" data-testid="catalogue-guard-warning">
        <Typography variant="bodySmall">{message}</Typography>
        <Typography variant="bodySmall">{hint}</Typography>
      </Alert>
    ) : null;

  const dialog = asking ? (
    <Dialog open onClose={() => setAsking(false)} maxWidth="sm" fullWidth>
      <DialogTitle>{t('pages.admin.configuration.catalogue.confirmTitle', 'Switch to approved only?')}</DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">{message}</Typography>
      </DialogContent>
      <DialogActions>
        <Button onClick={() => setAsking(false)}>{t('common.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          color="warning"
          data-testid="catalogue-guard-confirm"
          onClick={() => {
            setAsking(false);
            save();
          }}
        >
          {t('pages.admin.configuration.catalogue.confirmSave', 'Save anyway')}
        </Button>
      </DialogActions>
    </Dialog>
  ) : null;

  return {
    warning,
    dialog,
    onSave: () => {
      if (missing > 0) setAsking(true);
      else save();
    },
  };
}
