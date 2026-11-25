/**
 * Admin › People and access › SCIM (ADR-0059 decision 6; R3.6, issue 2537):
 * the SCIM client credentials and the group bindings, which used to sit under
 * the identity provider editor on the Configuration page.
 *
 * The credential comes first: an identity provider needs one before any group
 * it pushes can be bound. Creating, rotating and deleting a client are trust
 * actions, read-only for a holder of the settings half
 * (`./AdminIdentityTrustNotice.tsx`); revoking one and the bindings are not.
 */
import Divider from '@mui/material/Divider';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminScimClientsEditor } from './AdminScimClientsEditor';
import { AdminScimGroupBindingsEditor } from './AdminScimGroupBindingsEditor';
import { AdminViewOnlyNotice, AdminWritable } from './AdminWritable';
import { ADMIN_ITEM } from './adminItemIds';

export function AdminScim() {
  return (
    <AdminWritable itemId={ADMIN_ITEM.scim}>
      <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }} data-testid="admin-scim-page">
        <Typography variant="headingLarge" component="h1">
          {t('pages.admin.nav.scim', 'SCIM')}
        </Typography>
        <AdminViewOnlyNotice />
        <AdminScimClientsEditor />
        <Divider sx={{ mt: '0.5rem' }} />
        <AdminScimGroupBindingsEditor />
      </DrawerPage>
    </AdminWritable>
  );
}
