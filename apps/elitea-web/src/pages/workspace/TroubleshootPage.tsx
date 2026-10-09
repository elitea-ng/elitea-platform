/**
 * Settings › Troubleshoot (desktop build only): the Doctor in a settings
 * page — the same checks and repairs as Help › Run Diagnostics…
 */
import { Stack, Typography } from '@mui/material';
import { useMemo } from 'react';

import { tauriDoctorIpc, type DoctorIpc } from '@/shared/desktop/doctorIpc';
import { t } from '@/shared/i18n';
import { DoctorPanel } from '@/widgets/desktop-shell';

export default function TroubleshootPage({ ipc }: { ipc?: DoctorIpc }): React.JSX.Element {
  const doctor = useMemo(() => ipc ?? tauriDoctorIpc(), [ipc]);
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 760 }}>
      <Typography component="h1" variant="headingMedium">
        {t('desktop.doctor.pageTitle', 'Troubleshoot')}
      </Typography>
      {doctor === undefined ? (
        <Typography>{t('desktop.doctor.noHost', 'Diagnostics run in the desktop app.')}</Typography>
      ) : (
        <DoctorPanel ipc={doctor} />
      )}
    </Stack>
  );
}
