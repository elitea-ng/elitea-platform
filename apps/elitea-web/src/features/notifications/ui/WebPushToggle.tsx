/**
 * Settings › Notifications in a browser: "Web push in this browser" (ADR-0045
 * decision 6, U3 contract §5). On: the browser shows a notice when a run
 * finishes or waits for the person, also while no Elitea tab is open. Not
 * shown when the deployment has web push off, when the browser cannot do
 * push, or in the desktop app (it has native notices).
 */
import FormControlLabel from '@mui/material/FormControlLabel';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { WebPushEnv } from '../lib/webPush';
import { useWebPush } from '../lib/useWebPush';

export interface WebPushToggleProps {
  /** A project the person can read (the personal one): the push routes' shape. */
  readonly projectId: string;
  /** Stands in for the browser (tests). */
  readonly env?: WebPushEnv;
}

function hint(state: string | undefined, failed: boolean): string {
  if (failed) return t('notifications.webPush.failed', 'Web push did not change. Try again.');
  if (state === 'blocked') return t('notifications.webPush.blocked', 'Your browser blocks notifications from this site. Allow them in the site settings of your browser.');
  return t('notifications.webPush.hint', 'This browser shows a notice when a run finishes or waits for you, also when no Elitea tab is open.');
}

export function WebPushToggle({ projectId, env }: WebPushToggleProps): React.JSX.Element | null {
  const push = useWebPush(projectId, env);
  if (!push.visible) return null;
  const blocked = push.state === 'blocked';
  return (
    <FormControlLabel
      control={
        <Switch
          checked={push.state === 'on'}
          disabled={blocked || push.busy}
          onChange={(event) => push.setOn(event.target.checked)}
          slotProps={{ input: { 'aria-describedby': 'web-push-hint' } }}
        />
      }
      data-testid="settings-web-push"
      label={
        <>
          <Typography variant="labelSmall" component="span" sx={{ display: 'block' }}>
            {t('notifications.webPush.label', 'Web push in this browser')}
          </Typography>
          <Typography id="web-push-hint" variant="bodySmall" component="span" color={push.failed ? 'error' : 'text.metrics'}>
            {hint(push.state, push.failed)}
          </Typography>
        </>
      }
    />
  );
}
