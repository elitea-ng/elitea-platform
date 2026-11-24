/**
 * What the navigation column shows while the console capability document is
 * not known (ADR-0059 decision 6, R3.1b, issue 2611).
 *
 * Pending: a loading state. Failed or timed out: an error with a retry
 * control and NO navigation item. Never an empty navigation: an empty column
 * would read as "you can open nothing", and a failed read says nothing about
 * what the caller may do.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { SIDE_BAR_WIDTH_REM } from '@/shared/ui/layout/sidebarWidth';

export interface AdminConsoleStatusProps {
  readonly status: 'pending' | 'error' | 'refused';
  readonly onRetry: () => void;
}

/** The server refused the document itself: an answer about access, so no retry is offered. */
function ConsoleRefused(): ReactNode {
  return (
    <Box role="alert" data-testid="admin-console-refused">
      <Typography component="p" variant="labelMedium" sx={{ margin: 0 }}>
        {t('pages.admin.console.refused', 'You do not have access to the administration console.')}
      </Typography>
    </Box>
  );
}

function ConsoleError({ onRetry }: { readonly onRetry: () => void }): ReactNode {
  return (
    <Box role="alert" data-testid="admin-console-error" sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Typography component="p" variant="labelMedium" sx={{ margin: 0 }}>
        {t('pages.admin.console.error.title', "We couldn't load the console")}
      </Typography>
      <Typography
        component="p"
        variant="bodySmall"
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 })}
      >
        {t(
          'pages.admin.console.error.explanation',
          'This is not an answer about your access. Try again in a moment.',
        )}
      </Typography>
      <Button variant="contained" size="small" data-testid="admin-console-retry" onClick={onRetry}>
        {t('pages.admin.console.error.retry', 'Try again')}
      </Button>
    </Box>
  );
}

function StatusBody({ status, onRetry }: AdminConsoleStatusProps): ReactNode {
  if (status === 'refused') return <ConsoleRefused />;
  if (status === 'error') return <ConsoleError onRetry={onRetry} />;
  return (
    <Box component="output" data-testid="admin-console-loading" sx={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}>
      <CircularProgress size="1rem" aria-hidden />
      <Typography component="span" variant="bodySmall">
        {t('pages.admin.console.loading', 'Loading the console…')}
      </Typography>
    </Box>
  );
}

export function AdminConsoleStatus({ status, onRetry }: AdminConsoleStatusProps): ReactNode {
  return (
    <Box
      component="nav"
      aria-label={t('pages.admin.nav.ariaLabel', 'Admin navigation')}
      data-testid="admin-console-status"
      sx={(theme: Theme) => ({
        width: SIDE_BAR_WIDTH_REM,
        minWidth: SIDE_BAR_WIDTH_REM,
        height: '100%',
        boxSizing: 'border-box',
        padding: '1rem',
        borderRight: `0.0625rem solid ${theme.vars.palette.border.lines}`,
        background: theme.vars.palette.background.sideBar,
      })}
    >
      <StatusBody status={status} onRetry={onRetry} />
    </Box>
  );
}
