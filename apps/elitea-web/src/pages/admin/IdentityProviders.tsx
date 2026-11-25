/**
 * Admin › People and access › Identity providers (ADR-0059 decision 6;
 * R3.6, issue 2537). The editor used to be the Authentication section of
 * the Configuration page; that section now sends the operator here.
 *
 * The server keeps the `identity_providers` managed surface until R4.9
 * (issue 2623) and decides every write: the settings half on
 * `identity.sso.settings.manage`, the trust half on
 * `identity.sso.trust.manage` (`./AdminIdentityTrustNotice.tsx`).
 */
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminIdentityProvidersEditor } from './AdminIdentityProvidersEditor';

export function AdminIdentityProviders() {
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-identity-providers-page">
      <Typography variant="headingLarge" component="h1">
        {t('pages.admin.nav.identityProviders', 'Identity providers')}
      </Typography>
      <AdminIdentityProvidersEditor />
    </DrawerPage>
  );
}
