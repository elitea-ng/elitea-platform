/**
 * Above the composer of a thread that is not running here: a compact inline
 * notice in the shell's neutral banner style (message, then plain buttons). It
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
  return (
    <Box
      component="output"
      data-testid="cloud-thread-banner"
      data-executor={status.executor}
      sx={(theme: Theme) => ({
        display: 'flex',
        alignItems: 'center',
        gap: 1,
        minHeight: 32,
        paddingX: 1.5,
        paddingY: 0.5,
        borderRadius: theme.vars.shape.radiusMd,
        fontSize: '0.8125rem',
        color: theme.vars.palette.text.primary,
        // Neutral, like the shell's other banners: a hairline and a faint tint, no accent.
        background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 5%, transparent)`,
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
      {/* Plain buttons after the message, as in the other banners. */}
      <Button size="small" onClick={onTakeBack} disabled={!status.sent_from_here}>
        {t('workspace.handoff.takeBack', 'Take back')}
      </Button>
      <Button size="small" onClick={onErase}>
        {t('workspace.handoff.erase', 'Erase cloud copy')}
      </Button>
    </Box>
  );
}
