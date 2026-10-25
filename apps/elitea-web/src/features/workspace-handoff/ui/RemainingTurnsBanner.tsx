/**
 * Above the composer of a thread that was taken back up to an earlier cloud
 * turn: the later turns are still kept with the cloud copy until its grace
 * period ends, and "Take back the remaining turns" brings them here on top
 * of the folder as it is now. Same neutral inline style as the cloud banner.
 */
import HistoryOutlinedIcon from '@mui/icons-material/HistoryOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import { t } from '@/shared/i18n';

export interface RemainingTurnsBannerProps {
  status: HandoffStatus;
  onTakeBack: () => void;
}

/** How many cloud turns can still be taken back: only on the computer that took the thread back, while it runs here. */
export function remainingTurns(status: HandoffStatus | undefined): number {
  if (status === undefined || status.executor !== 'local' || !status.sent_from_here) return 0;
  return status.rest_turns ?? 0;
}

function until(status: HandoffStatus): string | null {
  if (status.delete_after === undefined || status.delete_after === null) return null;
  const date = new Date(status.delete_after);
  return Number.isNaN(date.getTime()) ? null : date.toLocaleString();
}

export function RemainingTurnsBanner({ status, onTakeBack }: RemainingTurnsBannerProps): React.JSX.Element | null {
  const n = remainingTurns(status);
  if (n === 0) return null;
  const deadline = until(status);
  return (
    <Box
      component="output"
      data-testid="remaining-turns-banner"
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
        background: `color-mix(in srgb, ${theme.vars.palette.text.primary} 5%, transparent)`,
        boxShadow: `inset 0 0 0 0.5px color-mix(in srgb, ${theme.vars.palette.text.primary} 14%, transparent)`,
      })}
    >
      <HistoryOutlinedIcon sx={{ fontSize: 15 }} aria-hidden />
      <Box sx={{ flex: 1, minWidth: 0, display: 'flex', flexDirection: 'column' }}>
        <Typography variant="labelSmall" component="span">
          {t('workspace.handoff.remaining.banner', '{{n}} later cloud changes were not taken back. They are kept with the cloud copy for now.', { n: String(n) })}
        </Typography>
        {deadline !== null && (
          <Typography variant="bodySmall" data-testid="remaining-turns-until" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary })}>
            {t('workspace.handoff.remaining.until', 'You can take them back until {{date}}.', { date: deadline })}
          </Typography>
        )}
      </Box>
      <Button size="small" onClick={onTakeBack}>
        {t('workspace.handoff.remaining.takeBack', 'Take back the remaining turns')}
      </Button>
    </Box>
  );
}
