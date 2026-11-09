/**
 * Settings › Notifications on the desktop: "Show system notifications" —
 * whether a new notification also shows a native notice while the app is in
 * the background. On by default.
 */
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import { setSystemNoticesEnabled, useSystemNoticesEnabled } from '../lib/systemNotices';

export function SystemNoticesToggle(): React.JSX.Element {
  const enabled = useSystemNoticesEnabled();
  return (
    <FormControlLabel
      control={<Switch checked={enabled} onChange={(event) => setSystemNoticesEnabled(event.target.checked)} slotProps={{ input: { 'aria-describedby': 'system-notices-hint' } }} />}
      data-testid="settings-system-notifications"
      label={
        <>
          <Typography variant="labelSmall" component="span" sx={{ display: 'block' }}>
            {t('notifications.systemNotices.label', 'Show system notifications')}
          </Typography>
          <Typography id="system-notices-hint" variant="bodySmall" component="span" color="text.metrics">
            {t('notifications.systemNotices.hint', 'While {{product}} is in the background, a new notification also shows as a system notification.')}
          </Typography>
        </>
      }
    />
  );
}
