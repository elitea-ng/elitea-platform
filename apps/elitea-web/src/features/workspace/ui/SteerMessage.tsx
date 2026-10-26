/**
 * A message the person sent while the turn ran (`steer_injected`), shown as their message at the point of the
 * turn where the agent got it. A reloaded thread folds the same event, so it shows the message there too.
 */
import Box from '@mui/material/Box';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

export function SteerMessage({ text }: { text: string }): React.JSX.Element {
  return (
    <Box
      data-testid="steer-message"
      sx={(theme: Theme) => ({
        alignSelf: 'flex-end',
        maxWidth: '85%',
        paddingX: 1.5,
        paddingY: 1,
        borderRadius: theme.vars.shape.radiusMd,
        background: theme.vars.palette.background.userInputBackground,
      })}
    >
      <Typography variant="bodySmall" component="p" sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, marginBottom: 0.5 })}>
        {t('workspace.steer.delivered', 'Sent while the agent worked')}
      </Typography>
      <Typography variant="bodyMedium" sx={{ whiteSpace: 'pre-wrap' }}>
        {text}
      </Typography>
    </Box>
  );
}
