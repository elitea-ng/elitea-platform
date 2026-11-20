/**
 * The super admin refusals on identity trust (issue 2558).
 *
 * Creating an identity provider, changing its issuer, metadata, signing
 * certificates or identity attributes, re-binding its accounts, and creating,
 * rotating or deleting a SCIM client are super admin decisions. The server
 * refuses everyone else with 403 and a stable `code`
 * (`services/elitea-main/internal/api/v2/admin/identity_trust.go`); a provider
 * refusal also names the trust `fields` the save changed. This module turns
 * that body into a translated sentence, and falls back to the server's `error`
 * sentence for any other failure.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';

import { configFailureReason } from './adminConfigurationApi';

const TRUST_REFUSAL_MESSAGES: Readonly<Record<string, () => string>> = {
  identity_provider_trust_super_admin_required: () =>
    t(
      'pages.admin.identityTrust.refusal.provider',
      'Only a super admin can create an identity provider or change its issuer, metadata, signing certificates or identity attributes.',
    ),
  identity_binding_super_admin_required: () =>
    t(
      'pages.admin.identityTrust.refusal.rebind',
      'Only a super admin can re-bind accounts to an identity provider.',
    ),
  scim_client_super_admin_required: () =>
    t(
      'pages.admin.identityTrust.refusal.scimClient',
      'Only a super admin can create, rotate or delete a SCIM client.',
    ),
  role_caller_unresolved: () =>
    t(
      'pages.admin.identityTrust.refusal.unresolved',
      'Your own permissions could not be checked, so the change was not saved. Try again.',
    ),
};

interface TrustRefusalBody {
  readonly code?: unknown;
  readonly fields?: unknown;
}

function refusalBody(error: unknown): TrustRefusalBody | undefined {
  if (!(error instanceof EliteaApiError)) return undefined;
  const failure = error.failure;
  if (failure.kind !== 'http' || failure.status !== 403) return undefined;
  const body = failure.body;
  return typeof body === 'object' && body !== null ? body : undefined;
}

/**
 * The translated refusal, then the trust fields it is about. `undefined` when
 * the failure is not one of these refusals and carries no `error` sentence.
 */
export function identityTrustFailureReason(error: unknown): string | undefined {
  const body = refusalBody(error);
  const code = typeof body?.code === 'string' ? body.code : '';
  const message = TRUST_REFUSAL_MESSAGES[code];
  if (message === undefined) return configFailureReason(error);
  const fields = Array.isArray(body?.fields)
    ? body.fields.filter((field): field is string => typeof field === 'string')
    : [];
  const sentences = [message()];
  if (fields.length > 0) {
    sentences.push(
      t('pages.admin.identityTrust.refusal.fields', 'Changed: {{fields}}.', {
        fields: fields.join(', '),
      }),
    );
  }
  return sentences.join(' ');
}
