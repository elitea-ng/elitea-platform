/**
 * Above the composer of a thread that is not running here: it says so, that
 * changes made here are not seen by the cloud, and offers Take back and
 * Erase cloud copy. A take back that is already asked for (a cloud turn is
 * finishing) and a hand-back in progress say so.
 */
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

import { ExecutorBadge } from './ExecutorBadge';

export interface CloudThreadBannerProps {
  status: HandoffStatus;
  onTakeBack: () => void;
  onErase: () => void;
}

export function CloudThreadBanner({ status, onTakeBack, onErase }: CloudThreadBannerProps): React.JSX.Element {
  const returning = status.executor === 'returning';
  return (
    <Alert
      severity={returning ? 'warning' : 'info'}
      icon={false}
      data-testid="cloud-thread-banner"
      action={
        <Box sx={{ display: 'flex', gap: 1, alignItems: 'center' }}>
          <Button size="small" color="inherit" onClick={onErase}>
            {t('workspace.handoff.erase', 'Erase cloud copy')}
          </Button>
          <Button size="small" variant="contained" disabled={!status.sent_from_here} onClick={onTakeBack}>
            {t('workspace.handoff.takeBack', 'Take back')}
          </Button>
        </Box>
      }
    >
      <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1 }}>
          <ExecutorBadge executor={status.executor} />
          <Typography variant="bodyMedium">
            {t('workspace.handoff.banner', 'This thread is running in the cloud executor. Local changes are not seen by it.')}
          </Typography>
        </Box>
        {status.reclaim_pending === true && (
          <Typography variant="bodySmall" data-testid="banner-reclaim-pending">
            {t('workspace.handoff.reclaimPending', 'Take back requested: it starts when the running cloud turn ends.')}
          </Typography>
        )}
        {returning && (
          <Typography variant="bodySmall" data-testid="banner-returning">
            {t('workspace.handoff.returning', 'The cloud has handed this thread back. Finish taking it back to continue here.')}
          </Typography>
        )}
        {!status.sent_from_here && (
          <Typography variant="bodySmall" data-testid="banner-other-device">
            {status.device_name
              ? t('workspace.handoff.otherDeviceNamed', 'This thread was sent from {{device}}. Take it back there.', { device: status.device_name })
              : t('workspace.handoff.otherDevice', 'This thread was sent from another computer. Take it back there.')}
          </Typography>
        )}
      </Box>
    </Alert>
  );
}
