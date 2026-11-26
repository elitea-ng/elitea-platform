/**
 * What the dangerous-action dialog says for a governance row write (ADR-0059
 * decision 6; R3.8b, issue 2614). A guardrail rule or its fail mode is
 * critical. The server decides by the kind of the row, so every write of the
 * governance routes carries the reason and the dialog shows the route's
 * level.
 */
import { t } from '@/shared/i18n';

import type { AdminDangerousRequest } from './useAdminDangerousConfirm';

type GovernanceRequest = Omit<AdminDangerousRequest, 'run'>;

/** The copy and action key of saving the row `name` of kind `type`. */
export function governanceSaveRequest(row: {
  readonly id?: string | undefined;
  readonly type: string;
  readonly name: string;
}): GovernanceRequest {
  return {
    action:
      row.id === undefined
        ? 'POST /api/v2/admin/gateway/governance guardrail'
        : 'PUT /api/v2/admin/gateway/governance/{id} guardrail',
    title: t('pages.admin.governance.confirm.saveTitle', 'Save governance entry'),
    change:
      row.type === 'guardrail'
        ? t(
            'pages.admin.governance.confirm.guardrailBody',
            'The guardrail and its fail mode decide what the gateway blocks, redacts or lets through for every project it covers.',
          )
        : t(
            'pages.admin.governance.confirm.saveBody',
            'The gateway applies the saved entry to every request it matches.',
          ),
    reach: { count: undefined, unit: 'projects' },
    target: row.name,
    confirmLabel: t('pages.admin.governance.confirm.save', 'Save'),
  };
}

/** The copy and action key of deleting the row `name`. */
export function governanceDeleteRequest(row: { readonly name: string }): GovernanceRequest {
  return {
    action: 'DELETE /api/v2/admin/gateway/governance/{id} guardrail',
    title: t('pages.admin.governance.confirm.deleteTitle', 'Delete governance entry'),
    change: t(
      'pages.admin.governance.confirm.deleteBody',
      'The gateway stops applying this entry within its refresh interval. A deleted guardrail no longer blocks or redacts anything.',
    ),
    reach: { count: undefined, unit: 'projects' },
    target: row.name,
    confirmLabel: t('pages.admin.governance.confirm.delete', 'Delete'),
  };
}
