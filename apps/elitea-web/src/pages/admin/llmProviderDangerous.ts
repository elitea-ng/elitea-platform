/**
 * What the dangerous-action dialog says for a platform LLM provider write
 * (ADR-0059 decision 6; R3.8b, issue 2614). A platform credential resolves in
 * every project, so publishing, editing or withdrawing it reaches all of them.
 * The server cannot tell an endpoint or credential change from a settings
 * edit by the request alone, so every save carries the reason.
 */
import { t } from '@/shared/i18n';

import type { AdminDangerousRequest } from './useAdminDangerousConfirm';

type ProviderRequest = Omit<AdminDangerousRequest, 'run'>;

/** The copy and action key of saving the provider `name`. */
export function providerSaveRequest(name: string, editing: boolean): ProviderRequest {
  return {
    action: editing
      ? 'PUT /api/v2/admin/gateway/providers/{configID} route'
      : 'POST /api/v2/admin/gateway/providers',
    title: t('pages.admin.llmProviders.confirm.saveTitle', 'Save the platform provider'),
    change: t(
      'pages.admin.llmProviders.confirm.saveBody',
      'Every project resolves this credential. A new endpoint, key or keyless identity takes effect for all of them on the next request.',
    ),
    reach: { count: undefined, unit: 'projects' },
    target: name,
    confirmLabel: t('pages.admin.llmProviders.save', 'Save'),
  };
}

/** The copy and action key of deleting the provider `name`. */
export function providerDeleteRequest(name: string): ProviderRequest {
  return {
    action: 'DELETE /api/v2/admin/gateway/providers/{configID}',
    title: t('pages.admin.llmProviders.confirm.deleteTitle', 'Delete the platform provider'),
    change: t(
      'pages.admin.llmProviders.confirmDelete',
      'Deleting “{{name}}” withdraws it from every project at once, including any whose models name it. Those models stop resolving.',
      { name },
    ),
    reach: { count: undefined, unit: 'projects' },
    target: name,
    confirmLabel: t('pages.admin.llmProviders.delete', 'Delete'),
  };
}
