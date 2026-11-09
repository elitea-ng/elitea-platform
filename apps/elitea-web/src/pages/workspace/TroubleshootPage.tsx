/**
 * Settings › Troubleshoot (desktop build only): the Doctor in a settings
 * page — the same checks and repairs as Help › Run Diagnostics…
 */
import { Link, Stack, Typography } from '@mui/material';
import { useMemo } from 'react';

import { tauriDoctorIpc, type DoctorIpc } from '@/shared/desktop/doctorIpc';
import { t } from '@/shared/i18n';
import { useHostInfo, webSupportHref } from '@/widgets/account-menu';
import { DoctorPanel } from '@/widgets/desktop-shell';

export default function TroubleshootPage({ ipc, supportContact }: { ipc?: DoctorIpc; supportContact?: string }): React.JSX.Element {
  const doctor = useMemo(() => ipc ?? tauriDoctorIpc(), [ipc]);
  const host = useHostInfo();
  const line = supportContact ?? host.supportContact;
  const href = line === undefined ? null : webSupportHref(line);
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 760 }}>
      <Typography component="h1" variant="headingMedium">
        {t('desktop.doctor.pageTitle', 'Troubleshoot')}
      </Typography>
      {line !== undefined && (
        // The IT support line (ADR-0052 W6.2): managed, else the workspace's. The host opens only
        // https links; an email address or phone number stays text to select and copy.
        <Typography data-testid="troubleshoot-it-support" sx={{ userSelect: 'text' }}>
          {t('desktop.doctor.itSupport', 'IT support:')}{' '}
          {href === null ? (
            line
          ) : (
            <Link href={href} target="_blank" rel="noopener noreferrer">
              {line}
            </Link>
          )}
        </Typography>
      )}
      {doctor === undefined ? (
        <Typography>{t('desktop.doctor.noHost', 'Diagnostics run in the desktop app.')}</Typography>
      ) : (
        <DoctorPanel ipc={doctor} />
      )}
    </Stack>
  );
}
