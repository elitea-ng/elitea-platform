/**
 * The trust half of identity setup (ADR-0059 decision 2, the identity trust
 * rule; R3.6, issue 2537).
 *
 * A holder of `identity.sso.settings.manage` reaches the Identity providers
 * and SCIM pages at `write`, but the trust actions (create, delete, turn on
 * or off a provider, change its issuer, metadata, client credentials or
 * certificates, and create, rotate or delete a SCIM client) are a global
 * administrator's. Which half the caller holds comes from a second item of
 * the capability document, `identity-trust`, never from a permission name.
 *
 * FAILURE MUST NOT READ AS NO ACCESS: while the document is unknown the trust
 * controls stay enabled, and the server refuses a trust action with its own
 * code (`./api/identityTrustRefusal.ts`) whatever the page shows.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';

import { ADMIN_ITEM } from './adminItemIds';
import { useAdminItemAccess } from './adminWriteAccess';

/** True when the document is known and gives the caller no trust half. */
export function useIdentityTrustLocked(): boolean {
  const access = useAdminItemAccess(ADMIN_ITEM.identityTrust);
  return access !== undefined && access !== 'write';
}

/** The notice beside locked trust controls; nothing when they are not locked. */
export function AdminIdentityTrustNotice({ subject }: { readonly subject: 'providers' | 'scim' }): ReactNode {
  if (!useIdentityTrustLocked()) return null;
  return (
    <Alert severity="info" data-testid="admin-identity-trust-notice">
      {subject === 'providers'
        ? t(
            'pages.admin.identityTrust.locked.providers',
            'A global administrator changes the trust settings of a provider: adding or removing it, turning it on or off, and its issuer, metadata URL, client credentials and certificates. Your roles change its name, scopes and login domains.',
          )
        : t(
            'pages.admin.identityTrust.locked.scim',
            'A global administrator creates, rotates and deletes SCIM clients. Your roles can revoke a client and manage the group bindings.',
          )}
    </Alert>
  );
}
