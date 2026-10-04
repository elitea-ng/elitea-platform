/**
 * Settings › Usage's "Refresh data" header action (#6672).
 *
 * Fetches the active project's usage for the active scope again without a page
 * reload. While the request is in flight the button is disabled and shows a
 * spinner, so a second click cannot queue a duplicate request. A failure keeps
 * the figures already on screen and says so in a toast rather than replacing
 * them with an error panel.
 */
import { memo, useCallback, useState } from 'react';

import Alert from '@mui/material/Alert';
import CircularProgress from '@mui/material/CircularProgress';
import IconButton from '@mui/material/IconButton';
import Snackbar from '@mui/material/Snackbar';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { RefreshIcon } from '@/shared/ui/icons/refresh-icon';

import { useRefreshProjectUsage, type UsageScope } from './api/projectUsageApi';

interface UsageRefreshButtonProps {
  readonly projectId: string | undefined;
  readonly scope?: UsageScope | undefined;
}

const iconStyle = { width: '1rem', height: '1rem' };

export const UsageRefreshButton = memo(({ projectId, scope = 'project' }: UsageRefreshButtonProps) => {
  const { refresh, isRefreshing } = useRefreshProjectUsage(projectId, scope);
  const [failed, setFailed] = useState(false);
  const closeToast = useCallback(() => setFailed(false), []);
  const label = t('settings.usage.refresh', 'Refresh data');
  const noProject = projectId === undefined || projectId === '';

  const onClick = useCallback(async () => {
    setFailed(false);
    const ok = await refresh();
    if (!ok) setFailed(true);
  }, [refresh]);

  return (
    <>
      <Tooltip title={label} placement="top">
        {/* A disabled button fires no events, so the tooltip needs a wrapper. */}
        <span>
          <IconButton
            color="tertiary"
            aria-label={label}
            aria-busy={isRefreshing}
            disabled={isRefreshing || noProject}
            onClick={() => void onClick()}
            data-testid="settings-usage-refresh"
          >
            {isRefreshing ? (
              <CircularProgress size="1rem" data-testid="settings-usage-refreshing" />
            ) : (
              <RefreshIcon style={iconStyle} />
            )}
          </IconButton>
        </span>
      </Tooltip>
      <Snackbar
        open={failed}
        autoHideDuration={6000}
        onClose={closeToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        {failed ? (
          <Alert onClose={closeToast} severity="error" variant="filled" data-testid="settings-usage-refresh-error">
            {t('settings.usage.refreshFailed', 'Unable to refresh usage data. Please try again.')}
          </Alert>
        ) : undefined}
      </Snackbar>
    </>
  );
});

UsageRefreshButton.displayName = 'UsageRefreshButton';
