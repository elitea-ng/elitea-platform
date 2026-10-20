/**
 * Above the composer of a thread that is not running here: a compact inline
 * notice (a native "this document is read-only" strip, not an alert card). It
 * says where the thread runs, that changes made here are not seen by the
 * cloud, and offers Take back and Erase cloud copy. A take back that is
 * already asked for (a cloud turn is finishing) and a hand-back in progress
 * say so.
 */
import CloudOutlinedIcon from '@mui/icons-material/CloudOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

export interface CloudThreadBannerProps {
  status: HandoffStatus;
  onTakeBack: () => void;
  onErase: () => void;
}

const quiet = (theme: Theme) => ({ color: theme.vars.palette.text.secondary });

function Details({ status }: { status: HandoffStatus }): React.JSX.Element {
  return (
    <>
      {status.reclaim_pending === true && (
        <Typography variant="bodySmall" data-testid="banner-reclaim-pending" sx={quiet}>
          {t('workspace.handoff.reclaimPending', 'Take back requested: it starts when the running cloud turn ends.')}
        </Typography>
      )}
      {status.executor === 'returning' && (
        <Typography variant="bodySmall" data-testid="banner-returning" sx={quiet}>
          {t('workspace.handoff.returning', 'The cloud has handed this thread back. Finish taking it back to continue here.')}
        </Typography>
      )}
      {!status.sent_from_here && (
        <Typography variant="bodySmall" data-testid="banner-other-device" sx={quiet}>
          {status.device_name
            ? t('workspace.handoff.otherDeviceNamed', 'This thread was sent from {{device}}. Take it back there.', { device: status.device_name })
            : t('workspace.handoff.otherDevice', 'This thread was sent from another computer. Take it back there.')}
        </Typography>
      )}
    </>
  );
}

export function CloudThreadBanner({ status, onTakeBack, onErase }: CloudThreadBannerProps): React.JSX.Element {
  const returning = status.executor === 'returning';
  return (
    <Box
      component="output"
      data-testid="cloud-thread-banner"
      data-executor={status.executor}
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        flexWrap: 'wrap',
        minHeight: 28,
        paddingX: 1.25,
        paddingY: 0.5,
        borderRadius: '8px',
        fontSize: '0.75rem',
        background: `color-mix(in srgb, ${returning ? theme.vars.palette.warning.main : theme.vars.palette.primary.main} 10%, transparent)`,
        boxShadow: `inset 0 0 0 0.5px color-mix(in srgb, ${theme.vars.palette.text.primary} 14%, transparent)`,
      })}
    >
      <CloudOutlinedIcon sx={{ fontSize: 15 }} aria-hidden />
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <Typography variant="labelSmall" component="span">
          {t('workspace.handoff.banner', 'This thread is running in the cloud executor. Local changes are not seen by it.')}
        </Typography>
        <Details status={status} />
      </Box>
      <Button size="small" onClick={onErase}>
        {t('workspace.handoff.erase', 'Erase cloud copy')}
      </Button>
      <Button size="small" variant="outlined" disabled={!status.sent_from_here} onClick={onTakeBack}>
        {t('workspace.handoff.takeBack', 'Take back')}
      </Button>
    </Box>
  );
}
