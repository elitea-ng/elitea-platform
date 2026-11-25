/**
 * The words of the Access page (R3.6, R3.9a; issues 2537 and 2540): the
 * label of a console item a role opens, a role's seed status, and the reason
 * a role cannot be assigned, keyed on the server's refusal code. The server
 * decides each of these (`GET /admin/access/role-definitions`); this module
 * only names them.
 */
import { t } from '@/shared/i18n';

import { adminConsoleNavItems } from './adminConsoleAreas';

/** Items with no navigation entry of their own: Configuration tabs and Identity trust. */
function tabItemLabels(): Readonly<Record<string, string>> {
  return {
    'providers-models': t('pages.admin.access.item.providersModels', 'Providers and models'),
    'models-pricing': t('pages.admin.access.item.modelsPricing', 'Models and pricing'),
    'tool-prices': t('pages.admin.access.item.toolPrices', 'Tool prices'),
    usage: t('pages.admin.access.item.usage', 'Usage'),
    logs: t('pages.admin.access.item.logs', 'Logs'),
    'identity-trust': t('pages.admin.access.item.identityTrust', 'Identity trust'),
  };
}

/** The label of a console item; its id when this build does not know it. */
export function consoleItemLabel(itemId: string): string {
  const entry = adminConsoleNavItems().find((candidate) => candidate.id === itemId);
  return entry?.label ?? tabItemLabels()[itemId] ?? itemId;
}

/** A role's seed status, as the server names it. */
export function roleStatusLabel(status: string): string {
  switch (status) {
    case 'built_in':
      return t('pages.admin.access.status.builtIn', 'Built-in');
    case 'as_found':
      return t('pages.admin.access.status.asFound', 'Built-in, kept as found');
    case 'unassignable':
      return t('pages.admin.access.status.unassignable', 'Never assigned');
    default:
      return t('pages.admin.access.status.custom', 'Custom');
  }
}

/** Why the caller cannot give the role to another person. */
export function refusalLabel(code: string | undefined): string {
  switch (code) {
    case 'role_super_admin_required':
      return t('pages.admin.access.refusal.superAdmin', 'Only a global administrator assigns this role.');
    case 'role_set_locked':
      return t('pages.admin.access.refusal.locked', 'This role is never assigned.');
    case 'role_permission_not_held':
      return t('pages.admin.access.refusal.notHeld', 'You do not hold every permission of this role.');
    case undefined:
    default:
      return t('pages.admin.access.refusal.other', 'You cannot assign this role.');
  }
}
