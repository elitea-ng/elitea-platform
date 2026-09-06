/**
 * Admin › E-mail — the page around `./AdminEmailEditor.tsx` (gap G7).
 *
 * The editor is the same component the Configuration page's "E-mail" section
 * renders through its server-declared `managed_surface`. It exists in two
 * places for one reason: the settings belong on the Configuration page beside
 * the other platform sections, AND an operator whose deployment cannot deliver
 * an invitation needs to find them without knowing that "E-mail" is a
 * Configuration section. One component, so the two entry points cannot drift.
 *
 * Authorisation: `runtime.plugins`, server-side, on every route the editor
 * calls. The nav item's permission list is presentation only — see
 * `./adminUiConfig`.
 */
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AdminEmailEditor } from './AdminEmailEditor';

export function AdminEmail() {
  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem', gap: '0.75rem' }}>
      <Typography variant="h5" sx={{ fontWeight: 600 }}>
        {t('pages.admin.email.title', 'E-mail')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.email.subtitle',
          'The relay this deployment sends invitations and notices through. A change takes effect on the next message; no restart.',
        )}
      </Typography>
      <AdminEmailEditor />
    </DrawerPage>
  );
}
