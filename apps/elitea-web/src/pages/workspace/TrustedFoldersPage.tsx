/**
 * Settings › Trusted folders (desktop build only; ADR-0061 decision 5i,
 * #3062): the folders whose commands run without a sandbox, with Revoke.
 */
import { Stack, Typography } from '@mui/material';

import { TrustedFoldersList } from '@/features/folder-trust';
import { t } from '@/shared/i18n';

export default function TrustedFoldersPage(): React.JSX.Element {
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 960 }}>
      <Typography component="h1" variant="headingMedium">
        {t('desktop.trust.pageTitle', 'Trusted folders')}
      </Typography>
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'desktop.trust.pageHelp',
          'In a trusted folder, commands run as you: without a sandbox, with your environment, your logins and the network. Your organisation’s policy, your forbidden rules and your hooks still apply. Trust a folder from its menu in the sidebar.',
        )}
      </Typography>
      <TrustedFoldersList />
    </Stack>
  );
}
