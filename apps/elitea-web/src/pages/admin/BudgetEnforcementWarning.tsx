/**
 * The banner for gap G12: budgets are AUTHORED here and ENFORCED by the LLM
 * gateway's NATS counter, and the second half can be absent while the first
 * half looks completely healthy.
 *
 * A gateway started without `GATEWAY_NATS_URL` serves `/llm` with no budget
 * enforcement. Every PUT on this page still answers 200, every ceiling still
 * round-trips, every spend figure still renders — and no call is ever refused.
 * There is no signal for that anywhere in the authoring surface, which is
 * exactly the condition this banner exists to break.
 *
 * Same shape and same discipline as `LlmProviderScopeWarning.tsx`: it warns
 * only on a POSITIVE report from the gateway. An unreachable hop and a gateway
 * too old to carry the field both render nothing — see `useBudgetEnforcementOff`
 * for why a silent gateway must not be read as "enforcement is off".
 *
 * It is additionally gated on there being something to enforce. On a deployment
 * where nobody has set a budget, "budgets are not enforced" is true and
 * useless; the warning appears once a row exists, when it starts describing a
 * control an operator believes they applied.
 */
import type { ReactNode } from 'react';
import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

import { useBudgetEnforcementOff } from './api/adminBudgetsApi';

export function BudgetEnforcementWarning({
  hasBudgets,
}: {
  readonly hasBudgets: boolean;
}): ReactNode {
  const enforcementOff = useBudgetEnforcementOff();

  if (!hasBudgets) return null;
  if (!enforcementOff) return null;

  return (
    <Alert severity="warning" data-testid="admin-budgets-enforcement-off">
      {t(
        'pages.admin.budgets.enforcementOff',
        'The LLM gateway is running without its NATS counter, so no budget on this page is enforced. Limits are stored and reported, and every call is admitted. Set GATEWAY_NATS_URL on the gateway to enforce them.',
      )}
    </Alert>
  );
}
