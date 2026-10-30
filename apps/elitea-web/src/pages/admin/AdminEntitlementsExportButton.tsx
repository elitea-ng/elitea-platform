/**
 * "Export entitlements" — the CSV download of ADR-0044 decision 5 (#1501) on
 * the admin Users page: every account's roles on the platform, its projects
 * and its agents, with the governance capabilities each role carries, for an
 * identity governance tool's access review.
 *
 * Rendered only for an operator whose injected admin permissions include
 * `entitlements.export` (presentation only — the route is gated server-side
 * and every export is audited there).
 */
import { useState } from 'react';

import FactCheckOutlinedIcon from '@mui/icons-material/FactCheckOutlined';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Snackbar from '@mui/material/Snackbar';

import { t } from '@/shared/i18n';

import { adminUiShowsControlFor } from './adminUiConfig';
import { downloadEntitlementsCsv, ENTITLEMENTS_EXPORT_PERMISSION } from './api/adminEntitlementsApi';

export function AdminEntitlementsExportButton() {
  const [exporting, setExporting] = useState(false);
  const [failed, setFailed] = useState(false);

  if (!adminUiShowsControlFor(ENTITLEMENTS_EXPORT_PERMISSION)) return null;

  const onExport = () => {
    setFailed(false);
    setExporting(true);
    downloadEntitlementsCsv()
      .catch(() => setFailed(true))
      .finally(() => setExporting(false));
  };

  return (
    <>
      <Button
        variant="elitea"
        color="tertiary"
        size="small"
        startIcon={exporting ? <CircularProgress size={14} /> : <FactCheckOutlinedIcon fontSize="small" />}
        onClick={onExport}
        disabled={exporting}
        sx={{ flexShrink: 0, whiteSpace: 'nowrap' }}
        data-testid="admin-entitlements-export"
      >
        {t('pages.admin.users.action.exportEntitlements', 'Export entitlements')}
      </Button>
      <Snackbar
        open={failed}
        autoHideDuration={6000}
        onClose={() => setFailed(false)}
        message={t('pages.admin.users.error.exportEntitlements', 'Failed to export entitlements.')}
        data-testid="admin-entitlements-export-error"
      />
    </>
  );
}
