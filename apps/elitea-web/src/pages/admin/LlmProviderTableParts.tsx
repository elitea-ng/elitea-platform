/**
 * The Providers panel's presentational parts: its alert stack, its table, its
 * three terminal states and its delete confirmation.
 *
 * Their own file so `./LlmProxyProvidersPanel.tsx` stays the panel's STATE —
 * what is being edited, what failed, what is pending — and these stay markup
 * with one decision each. The split was forced by the file-length gate and is
 * the right seam regardless: every function here is pure in its props.
 */
import type { ReactNode } from 'react';

import CancelOutlinedIcon from '@mui/icons-material/CancelOutlined';
import CheckCircleOutlineIcon from '@mui/icons-material/CheckCircleOutlineOutlined';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import LinearProgress from '@mui/material/LinearProgress';
import Paper from '@mui/material/Paper';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableContainer from '@mui/material/TableContainer';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { providerTypeLabel } from './llmProviderForm';
import { configFailureReason } from './api/adminConfigurationApi';
import type { LlmProvider, LlmProviderCheckResult } from './api/adminLlmProvidersApi';

/**
 * The Test/Re-validate actions and their per-row state, grouped into one prop
 * so `ProviderTable`/`ProviderResults` stay under the §3.5 component-props
 * budget as these two actions grew the row — the same collapse
 * `useCredentialConnectionTest.ts`'s `request` object makes for its own hook,
 * and for the same reason.
 */
export interface ProviderRowActions {
  /** Runs the live provider round trip (`POST /{id}/check`) — writes nothing. */
  readonly onCheck: (row: LlmProvider) => void;
  /** Re-runs admission (`POST /{id}/revalidate`) and patches `status_ok`. */
  readonly onRevalidate: (row: LlmProvider) => void;
  /**
   * Opens the adopt-models dialog for this credential — the successor to
   * legacy's `import_llm_models`. It reads the provider's own catalogue; it
   * publishes nothing until the operator picks.
   */
  readonly onAdopt: (row: LlmProvider) => void;
  /** This session's own live-check verdicts, keyed by row id. Absent for a row = Test has not been pressed since the panel loaded. */
  readonly checkResults: Readonly<Record<number, LlmProviderCheckResult>>;
  readonly checkingId: number | undefined;
  readonly revalidatingId: number | undefined;
}

/**
 * The four things that can be wrong at once, each its own statement.
 *
 * Extracted so the panel stays its own state machine rather than a stack of
 * conditionals — the same complexity-gate split `AdminLlmProxyEditor` made for
 * `ModelCatalogueAlerts`, for the same reason.
 */
export function ProviderAlerts({
  loadError,
  saveError,
  deleteError,
  revalidateError,
  unsealed,
}: {
  readonly loadError: unknown;
  readonly saveError: string | undefined;
  readonly deleteError: unknown;
  readonly revalidateError: unknown;
  readonly unsealed: readonly LlmProvider[];
}): ReactNode {
  return (
    <>
      {loadError != null ? (
        <Alert severity="warning" data-testid="llm-providers-load-error">
          {configFailureReason(loadError) ??
            t('pages.admin.llmProviders.loadError', 'Failed to load the platform providers.')}
        </Alert>
      ) : null}

      {saveError !== undefined ? (
        <Alert severity="error" data-testid="llm-providers-save-error">
          {saveError}
        </Alert>
      ) : null}

      {deleteError != null ? (
        <Alert severity="error" data-testid="llm-providers-delete-error">
          {configFailureReason(deleteError) ??
            t('pages.admin.llmProviders.deleteError', 'Failed to delete the provider.')}
        </Alert>
      ) : null}

      {/* Re-validate re-runs ADMISSION only — a failure here means the row
          could not be re-derived, never that the provider is unreachable. */}
      {revalidateError != null ? (
        <Alert severity="error" data-testid="llm-providers-revalidate-error">
          {configFailureReason(revalidateError) ??
            t('pages.admin.llmProviders.revalidateError', 'Failed to re-validate the provider.')}
        </Alert>
      ) : null}

      {/* A finding, not a footnote: the value is readable by every holder of the
          project-scoped configuration permissions on the public project, and
          re-saving the credential is what fixes it. */}
      {unsealed.length > 0 ? (
        <Alert severity="warning" data-testid="llm-providers-unsealed">
          {t(
            'pages.admin.llmProviders.unsealed',
            'Some platform providers store their key directly in the configuration row rather than in the vault. Re-save each one to seal it: {{names}}',
            { names: unsealed.map((row) => row.elitea_title).join(', ') },
          )}
        </Alert>
      ) : null}
    </>
  );
}
/**
 * One row's live-check verdict, rendered UNDER the stored `status_ok` chip —
 * never inside it. `status_ok` is the ADMISSION decision this platform
 * already made; this is a fresh provider round trip the operator just asked
 * for, and the two can disagree (a credential can be admitted and still
 * unreachable right now, or briefly unreachable and still correctly admitted)
 * — collapsing them into one indicator would hide exactly the case an
 * operator presses Test to find.
 */
function ProviderCheckResult({ id, result }: { readonly id: number; readonly result: LlmProviderCheckResult }): ReactNode {
  return (
    <Box
      data-testid={`admin-provider-check-result-${String(id)}`}
      sx={{ display: 'flex', alignItems: 'center', gap: '0.25rem', marginTop: '0.25rem' }}
    >
      {result.success ? (
        <CheckCircleOutlineIcon fontSize="inherit" color="success" />
      ) : (
        <CancelOutlinedIcon fontSize="inherit" color="error" />
      )}
      <Typography variant="bodySmall" color={result.success ? 'success.main' : 'error.main'}>
        {result.success
          ? t('pages.admin.llmProviders.checkSuccess', 'Live check: connected')
          : (result.message ?? t('pages.admin.llmProviders.checkFailed', 'Live check failed'))}
      </Typography>
    </Box>
  );
}

export function ProviderTable({
  items,
  onEdit,
  onDelete,
  rowActions,
}: {
  readonly items: readonly LlmProvider[];
  readonly onEdit: (row: LlmProvider) => void;
  readonly onDelete: (row: LlmProvider) => void;
  readonly rowActions: ProviderRowActions;
}): ReactNode {
  const { onCheck, onRevalidate, onAdopt, checkResults, checkingId, revalidatingId } = rowActions;
  return (
    <TableContainer component={Paper} variant="outlined">
      {/* FIXED column widths, so a row's own content can never resize the
          others. Auto layout let the live-check line below (and the busy
          "Testing…"/"Re-validating…" labels) widen Status and narrow Actions,
          re-wrapping and re-heighting EVERY row. Chromium then hit-tested the
          rows React had not re-rendered at their pre-change offsets: in the
          chat-stream-real journey a click on one row's Delete landed on the
          NEXT row's Re-validate, on every retry, at every scroll position. */}
      <Table size="small" data-testid="llm-providers-table" sx={{ tableLayout: 'fixed' }}>
        <TableHead>
          <TableRow>
            <TableCell sx={{ width: '10rem' }}>{t('pages.admin.llmProviders.column.status', 'Status')}</TableCell>
            <TableCell>{t('pages.admin.llmProviders.column.name', 'Name')}</TableCell>
            <TableCell sx={{ width: '7rem' }}>{t('pages.admin.llmProviders.column.provider', 'Provider')}</TableCell>
            <TableCell sx={{ width: '10rem' }}>{t('pages.admin.llmProviders.column.endpoint', 'Endpoint')}</TableCell>
            <TableCell align="right" sx={{ width: '10rem' }}>
              {t('pages.admin.llmProviders.column.actions', 'Actions')}
            </TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {items.map((row) => {
            const checkResult = checkResults[row.id];
            return (
            <TableRow key={row.id}>
              <TableCell>
                {/* The words matter. "Inactive" would read as a switch someone
                    turned off; the credential did not RESOLVE, and re-saving it
                    with a corrected endpoint or key is the action. */}
                <Chip
                  size="small"
                  color={row.status_ok ? 'success' : 'warning'}
                  variant="outlined"
                  label={
                    row.status_ok
                      ? t('pages.admin.llmProviders.status.live', 'In use')
                      : t('pages.admin.llmProviders.status.unresolved', 'Not resolving')
                  }
                />
                {/* The LIVE result, distinct from the chip above: see
                    ProviderCheckResult's own header for why they never merge. */}
                {checkResult !== undefined ? <ProviderCheckResult id={row.id} result={checkResult} /> : null}
              </TableCell>
              {/* `anywhere`: under a fixed layout an unbroken title or URL
                  would otherwise overflow into the next column. */}
              <TableCell sx={{ overflowWrap: 'anywhere' }}>{row.elitea_title}</TableCell>
              <TableCell>{providerTypeLabel(row.type)}</TableCell>
              <TableCell sx={{ overflowWrap: 'anywhere' }}>
                <Typography variant="bodySmall" color="text.secondary">
                  {row.endpoint === '' ? '—' : row.endpoint}
                </Typography>
              </TableCell>
              <TableCell align="right">
                <Button
            variant="elitea" color="tertiary" size="small" onClick={() => onEdit(row)}>
                  {t('pages.admin.llmProviders.edit', 'Edit')}
                </Button>
                <Button
                  variant="elitea" color="tertiary" size="small"
                  data-testid={`admin-provider-check-${String(row.id)}`}
                  disabled={checkingId === row.id}
                  onClick={() => onCheck(row)}
                >
                  {checkingId === row.id
                    ? t('pages.admin.llmProviders.checking', 'Testing…')
                    : t('pages.admin.llmProviders.check', 'Test')}
                </Button>
                <Button
                  variant="elitea" color="tertiary" size="small"
                  data-testid={`admin-provider-revalidate-${String(row.id)}`}
                  disabled={revalidatingId === row.id}
                  onClick={() => onRevalidate(row)}
                >
                  {revalidatingId === row.id
                    ? t('pages.admin.llmProviders.revalidating', 'Re-validating…')
                    : t('pages.admin.llmProviders.revalidate', 'Re-validate')}
                </Button>
                <Button
                  variant="elitea" color="tertiary" size="small"
                  data-testid={`admin-provider-adopt-${String(row.id)}`}
                  onClick={() => onAdopt(row)}
                >
                  {t('pages.admin.llmProviders.adopt', 'Adopt models…')}
                </Button>
                <Button
            variant="elitea" color="alarm" size="small" onClick={() => onDelete(row)}>
                  {t('pages.admin.llmProviders.delete', 'Delete')}
                </Button>
              </TableCell>
            </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </TableContainer>
  );
}

/**
 * The listing's three terminal states: loading, empty, or a table.
 *
 * The empty state is suppressed when the read FAILED, because "no platform
 * providers yet" and "this could not be read" are different facts and the first
 * is the one an operator would act on — by publishing a duplicate of a
 * credential that already exists.
 */
export function ProviderResults({
  isPending,
  failed,
  items,
  onEdit,
  onDelete,
  rowActions,
}: {
  readonly isPending: boolean;
  readonly failed: boolean;
  readonly items: readonly LlmProvider[];
  readonly onEdit: (row: LlmProvider) => void;
  readonly onDelete: (row: LlmProvider) => void;
  readonly rowActions: ProviderRowActions;
}): ReactNode {
  if (isPending) {
    return <LinearProgress aria-label={t('pages.admin.llmProviders.loading', 'Loading providers')} />;
  }
  if (items.length > 0) {
    return <ProviderTable items={items} onEdit={onEdit} onDelete={onDelete} rowActions={rowActions} />;
  }
  if (failed) return null;
  return (
    <Typography variant="bodyMedium" color="text.secondary" data-testid="llm-providers-empty">
      {t(
        'pages.admin.llmProviders.empty',
        'No platform providers yet. Until one is published, every project must configure its own provider credentials.',
      )}
    </Typography>
  );
}

/**
 * The delete confirmation, inline rather than in a dialog.
 *
 * The whole of the warning is one sentence, and that sentence IS the reason the
 * confirmation exists: a platform credential is withdrawn from every project at
 * once. Counting the affected projects is not offered because it cannot be
 * cheaply computed — model rows live in one schema per project — and a number
 * this screen invented would be worse than the sentence.
 */
export function ConfirmDelete({
  provider,
  onCancel,
  onConfirm,
}: {
  readonly provider: LlmProvider | undefined;
  readonly onCancel: () => void;
  readonly onConfirm: (provider: LlmProvider) => void;
}): ReactNode {
  if (provider === undefined) return null;
  return (
    <Alert
      severity="warning"
      data-testid="llm-providers-confirm-delete"
      action={
        <>
          <Button
            variant="elitea" color="tertiary" size="small" onClick={onCancel}>
            {t('pages.admin.llmProviders.cancel', 'Cancel')}
          </Button>
          <Button
            variant="elitea" color="alarm"
            size="small"
                        data-testid="llm-providers-confirm-delete-button"
            onClick={() => onConfirm(provider)}
          >
            {t('pages.admin.llmProviders.delete', 'Delete')}
          </Button>
        </>
      }
    >
      {t(
        'pages.admin.llmProviders.confirmDelete',
        'Deleting “{{name}}” withdraws it from every project at once, including any whose models name it. Those models stop resolving.',
        { name: provider.elitea_title },
      )}
    </Alert>
  );
}
