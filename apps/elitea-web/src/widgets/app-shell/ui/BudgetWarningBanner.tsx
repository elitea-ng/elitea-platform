/**
 * The project budget WARNING banner (issue 312).
 *
 * ## What it ports
 *
 * The reference signals a budget in two stages: a proactive threshold warning
 * before the ceiling, and the hard refusal at it. Only the second half exists
 * here today — `shared/lib/constants/budgetError.constants.js`'s chat error is
 * ported, the threshold half is not — so a project heading for its limit went
 * from "fine" to "every call refused" with nothing in between.
 *
 * The reference carries the warning as a NOTIFICATION type
 * (`entities/notifications/lib/helpers/notification.helpers.js`:
 * `BudgetThresholdReached` / `MemberBudgetThresholdReached`, whose copy reads
 * "Budget warning: Acme has reached 80% of its monthly budget. [View project
 * usage]()" and whose click-through routes to the project's Usage page). Its
 * icon is the attention icon, not the error icon — the same distinction MUI's
 * `warning` severity draws.
 *
 * ## Why a banner and not a notification row
 *
 * A notification needs a PRODUCER: something server-side that decides a
 * threshold was crossed and writes a row. Nothing does. Writing one would put a
 * second source of truth beside the governance accumulators, which is the one
 * thing the budgets subsystem must not have — money drift is already on file
 * against it. The threshold is answered by the read the budgets API already
 * serves (`warning_active`), so the warning is rendered where the reference's
 * notification would have taken the user anyway, and the copy is the
 * reference's own.
 *
 * ## Dismissal is per session and per PERIOD
 *
 * `sessionStorage`, so closing it silences the banner for this tab and it comes
 * back on the next visit — the warning stays true until the operator raises the
 * limit or the month rolls over. The key carries the project id and the period
 * end, so dismissing it for one project does not silence another, and a new
 * billing period is a new warning. `createStorage('session')` namespaces the
 * key (`el.`), so the logout sweep reaches it; a raw key is the class
 * `clearNamespace` cannot see.
 */
import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Link from '@mui/material/Link';
import WarningAmberIcon from '@mui/icons-material/WarningAmber';

import { t } from '@/shared/i18n';
import { createStorage } from '@/shared/lib/storage';

import { useProjectBudgetWarning } from '../model/useProjectBudgetWarning';

export interface BudgetWarningBannerProps {
  /** The selected project, or undefined while nothing is selected. */
  readonly projectId?: string | undefined;
  readonly projectName?: string | undefined;
}

/** Namespaced (`el.`), so the logout sweep clears it. */
function dismissKey(projectId: string, periodEnd: string): string {
  return `budgetWarning.dismissed.${projectId}.${periodEnd}`;
}

export function BudgetWarningBanner({ projectId, projectName }: BudgetWarningBannerProps): ReactNode {
  const storage = createStorage('session');
  const budgetQuery = useProjectBudgetWarning(projectId);
  // Read ONCE into initial state, as PlatformBanner does: re-reading on every
  // render would make the banner reappear or vanish with unrelated re-renders
  // after a dismissal, and this is not state worth synchronising.
  const [dismissedKey, setDismissedKey] = useState<string | null>(null);

  const budget = budgetQuery.data;
  if (projectId === undefined || projectId === '' || budget === undefined) return null;
  if (budget.warning_active !== true) return null;

  const periodEnd = budget.period_end ?? '';
  const key = dismissKey(projectId, periodEnd);
  if (dismissedKey === key || storage.get(key) === '1') return null;

  // The percentage the server computed, not one derived here. It is rounded to
  // two decimals server-side; the banner shows whole percent, because "reached
  // 83.47% of its budget" is precision an operator cannot act on.
  const percent = Math.floor(budget.percent_used ?? budget.warning_pct ?? 0);

  return (
    <Alert
      severity="warning"
      icon={<WarningAmberIcon fontSize="small" />}
      variant="outlined"
      data-testid="budget-warning-banner"
      onClose={() => {
        storage.set(key, '1');
        setDismissedKey(key);
      }}
      // `output` (implicit role `status`, announced politely) rather than
      // MUI's default `role="alert"`, for PlatformBanner's reason: a budget
      // warning is not an interruption, and `alert` would move a screen-reader
      // user's focus on every page load for as long as it is up.
      component="output"
      aria-live="polite"
      slotProps={{ closeButton: { 'aria-label': t('widgets.appShell.budgetWarning.dismiss', 'Dismiss') } }}
      sx={{ alignItems: 'center' }}
    >
      {t(
        'widgets.appShell.budgetWarning.message',
        'Budget warning: {{project}} has reached {{percent}}% of its monthly budget.',
        { project: projectName ?? t('widgets.appShell.budgetWarning.thisProject', 'this project'), percent },
      )}{' '}
      <Link href="/settings/usage" data-testid="budget-warning-usage-link">
        {t('widgets.appShell.budgetWarning.viewUsage', 'View project usage')}
      </Link>
    </Alert>
  );
}
