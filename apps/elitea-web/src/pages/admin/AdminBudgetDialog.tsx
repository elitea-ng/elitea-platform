/**
 * The budget edit dialog, for a project and for one member.
 *
 * The two differ in exactly one way, and it is a server rule rather than a
 * layout choice: `budget_period` and `nats_fail_mode` are PROJECT-scoped, and
 * the member PUT answers 400 for either. `gateway.user_budget` has neither
 * column, and the gateway reads a project's fail mode from the OWNING project's
 * row even on the member path. So `scope="member"` hides the policy fields and
 * `buildBudgetWrite` then omits them from the payload — the field is not sent
 * disabled, it is not sent at all.
 *
 * ## The three states of "no limit"
 *
 * `monthly_limit` empty means NO CEILING, and that is not the same as `0`. Zero
 * is a real ceiling that refuses every call. The field is therefore allowed to
 * be blank and the helper text says what blank means, rather than defaulting to
 * a number nobody typed.
 *
 * `enabled: false` is a third state again: it keeps the authored number and
 * makes the project unlimited to the gateway. The dialog offers it as
 * "deliberately exempt" rather than pretending it deletes the limit — clearing
 * is a separate action with its own button.
 */
import { useEffect, useState, type ReactNode } from 'react';
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import TextField from '@mui/material/TextField';

import { t } from '@/shared/i18n';

import {
  BUDGET_PERIODS,
  INHERIT_FAIL_MODE,
  NATS_FAIL_MODES,
  type BudgetFormValues,
  type BudgetPeriod,
  type FailModeChoice,
} from './api/adminBudgetsApi';

/** What the dialog opens on. Every field is the STORED value or its default. */
export interface BudgetDialogInitialValues {
  readonly monthlyLimit: number | null;
  readonly enabled: boolean;
  readonly softAlertPct: number | null;
  readonly budgetPeriod?: string | undefined;
  readonly natsFailMode?: string | null | undefined;
}

export interface AdminBudgetDialogProps {
  readonly open: boolean;
  readonly scope: 'project' | 'member';
  readonly subjectName: string;
  readonly initial: BudgetDialogInitialValues | undefined;
  readonly isLoading: boolean;
  readonly isSaving: boolean;
  readonly serverError: string | undefined;
  readonly onClose: () => void;
  readonly onSubmit: (values: BudgetFormValues) => void;
}

/** A blank field is NO CEILING; anything else must be a number >= 0. */
function parseLimit(raw: string): { value: number | null } | { error: 'nan' | 'negative' } {
  const trimmed = raw.trim();
  if (trimmed === '') return { value: null };
  const parsed = Number(trimmed);
  if (!Number.isFinite(parsed)) return { error: 'nan' };
  if (parsed < 0) return { error: 'negative' };
  return { value: parsed };
}

/** Blank leaves the stored threshold alone; a value must be 1..100. */
function parseThreshold(raw: string): { value: number | null } | { error: 'range' } {
  const trimmed = raw.trim();
  if (trimmed === '') return { value: null };
  const parsed = Number(trimmed);
  if (!Number.isInteger(parsed) || parsed < 1 || parsed > 100) return { error: 'range' };
  return { value: parsed };
}

/**
 * A stored `null` fail mode means "inherit the platform baseline", which is a
 * real choice and gets its own option rather than an empty select.
 */
function initialFailMode(stored: string | null | undefined): FailModeChoice {
  if (stored === null || stored === undefined || stored === '') return INHERIT_FAIL_MODE;
  return NATS_FAIL_MODES.includes(stored as (typeof NATS_FAIL_MODES)[number])
    ? (stored as FailModeChoice)
    : INHERIT_FAIL_MODE;
}

function initialPeriod(stored: string | undefined): BudgetPeriod {
  return BUDGET_PERIODS.includes(stored as BudgetPeriod) ? (stored as BudgetPeriod) : 'monthly';
}

export function AdminBudgetDialog({
  open,
  scope,
  subjectName,
  initial,
  isLoading,
  isSaving,
  serverError,
  onClose,
  onSubmit,
}: AdminBudgetDialogProps): ReactNode {
  const [limit, setLimit] = useState('');
  const [enabled, setEnabled] = useState(true);
  const [threshold, setThreshold] = useState('');
  const [period, setPeriod] = useState<BudgetPeriod>('monthly');
  const [failMode, setFailMode] = useState<FailModeChoice>(INHERIT_FAIL_MODE);
  const [localError, setLocalError] = useState('');

  // Reseeded whenever the dialog opens OR the stored values arrive. The second
  // half matters: the dialog opens before the per-project read resolves, and a
  // form seeded only on `open` would submit the blanks it rendered first and
  // overwrite a policy it never showed anyone.
  useEffect(() => {
    if (!open || initial === undefined) return;
    setLimit(initial.monthlyLimit === null ? '' : String(initial.monthlyLimit));
    setEnabled(initial.enabled);
    setThreshold(initial.softAlertPct === null ? '' : String(initial.softAlertPct));
    setPeriod(initialPeriod(initial.budgetPeriod));
    setFailMode(initialFailMode(initial.natsFailMode));
    setLocalError('');
  }, [open, initial]);

  const handleSubmit = (): void => {
    const parsedLimit = parseLimit(limit);
    if ('error' in parsedLimit) {
      setLocalError(
        parsedLimit.error === 'nan'
          ? t('pages.admin.budgets.error.limitNumber', 'The monthly limit must be a number.')
          : t('pages.admin.budgets.error.limitNegative', 'The monthly limit cannot be negative.'),
      );
      return;
    }
    const parsedThreshold = parseThreshold(threshold);
    if ('error' in parsedThreshold) {
      setLocalError(
        t('pages.admin.budgets.error.thresholdRange', 'The warning threshold must be 1 to 100.'),
      );
      return;
    }
    setLocalError('');
    onSubmit({
      monthlyLimit: parsedLimit.value,
      enabled,
      softAlertPct: parsedThreshold.value,
      // Omitted entirely at member scope: the server refuses both fields there,
      // and sending them disabled would still be sending them.
      ...(scope === 'project' ? { budgetPeriod: period, failMode } : {}),
    });
  };

  const error = localError !== '' ? localError : serverError;

  return (
    <Dialog open={open} onClose={onClose} maxWidth="sm" fullWidth data-testid="admin-budget-dialog">
      <DialogTitle>
        {scope === 'project'
          ? t('pages.admin.budgets.dialog.projectTitle', 'Project budget — {{name}}', {
              name: subjectName,
            })
          : t('pages.admin.budgets.dialog.memberTitle', 'Member budget — {{name}}', {
              name: subjectName,
            })}
      </DialogTitle>
      <DialogContent>
        {error !== undefined && error !== '' ? (
          <Alert severity="error" sx={{ marginBottom: '1rem' }} data-testid="admin-budget-dialog-error">
            {error}
          </Alert>
        ) : null}

        <TextField
          margin="dense"
          fullWidth
          label={t('pages.admin.budgets.field.limit', 'Monthly limit (USD)')}
          value={limit}
          disabled={isSaving || isLoading}
          onChange={(event) => setLimit(event.target.value)}
          helperText={t(
            'pages.admin.budgets.field.limitHelp',
            'Leave empty for no ceiling. 0 is a real ceiling and refuses every call.',
          )}
          slotProps={{ htmlInput: { 'data-testid': 'admin-budget-limit' } }}
        />

        <TextField
          margin="dense"
          fullWidth
          label={t('pages.admin.budgets.field.threshold', 'Warning threshold (%)')}
          value={threshold}
          disabled={isSaving || isLoading}
          onChange={(event) => setThreshold(event.target.value)}
          helperText={t(
            'pages.admin.budgets.field.thresholdHelp',
            'Leave empty to inherit the platform threshold.',
          )}
          slotProps={{ htmlInput: { 'data-testid': 'admin-budget-threshold' } }}
        />

        {scope === 'project' ? (
          <>
            <TextField
              select
              margin="dense"
              fullWidth
              label={t('pages.admin.budgets.field.period', 'Budget period')}
              value={period}
              disabled={isSaving || isLoading}
              onChange={(event) => setPeriod(event.target.value as BudgetPeriod)}
              helperText={t(
                'pages.admin.budgets.field.periodHelp',
                'The gateway bills on the calendar month. No other period is enforced.',
              )}
              slotProps={{ htmlInput: { 'data-testid': 'admin-budget-period' } }}
            >
              {BUDGET_PERIODS.map((value) => (
                <MenuItem key={value} value={value}>
                  {periodLabel(value)}
                </MenuItem>
              ))}
            </TextField>

            <TextField
              select
              margin="dense"
              fullWidth
              label={t('pages.admin.budgets.field.failMode', 'NATS failure policy')}
              value={failMode}
              disabled={isSaving || isLoading}
              onChange={(event) => setFailMode(event.target.value as FailModeChoice)}
              helperText={t(
                'pages.admin.budgets.field.failModeHelp',
                'What the gateway does for this project when the budget counter is unreachable.',
              )}
              slotProps={{ htmlInput: { 'data-testid': 'admin-budget-fail-mode' } }}
            >
              <MenuItem value={INHERIT_FAIL_MODE}>
                {t('pages.admin.budgets.failMode.inherit', 'Inherit the platform default')}
              </MenuItem>
              {NATS_FAIL_MODES.map((value) => (
                <MenuItem key={value} value={value}>
                  {failModeLabel(value)}
                </MenuItem>
              ))}
            </TextField>
          </>
        ) : null}

        <FormControlLabel
          control={
            <Switch
              checked={enabled}
              disabled={isSaving || isLoading}
              onChange={(event) => setEnabled(event.target.checked)}
              data-testid="admin-budget-enabled"
            />
          }
          label={t('pages.admin.budgets.field.enabled', 'Enforce this budget')}
        />
      </DialogContent>
      <DialogActions sx={{ padding: '0 1.5rem 1rem' }}>
        <Button variant="text" disabled={isSaving} onClick={onClose}>
          {t('pages.admin.budgets.dialog.cancel', 'Cancel')}
        </Button>
        <Button
          variant="contained"
          disabled={isSaving || isLoading}
          onClick={handleSubmit}
          data-testid="admin-budget-save"
        >
          {t('pages.admin.budgets.dialog.save', 'Save')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

/**
 * The period options.
 *
 * A `switch` rather than a template key, so every string is a LITERAL the
 * i18n backfill can see. A `t(\`…period.${value}\`)` key is invisible to it,
 * which means the bundle silently never gets the entry and the fallback is
 * what ships forever.
 */
function periodLabel(period: (typeof BUDGET_PERIODS)[number]): string {
  switch (period) {
    case 'monthly':
      return t('pages.admin.budgets.period.monthly', 'Monthly');
  }
}

/** The three modes, named for what they DO rather than for their enum value. */
function failModeLabel(mode: (typeof NATS_FAIL_MODES)[number]): string {
  switch (mode) {
    case 'fail_open':
      return t('pages.admin.budgets.failMode.failOpen', 'Fail open — admit the call');
    case 'fail_closed':
      return t('pages.admin.budgets.failMode.failClosed', 'Fail closed — refuse the call');
    case 'tiered_hybrid':
      return t(
        'pages.admin.budgets.failMode.tieredHybrid',
        'Tiered hybrid — fall back to Postgres',
      );
  }
}
