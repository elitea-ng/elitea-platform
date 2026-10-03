/**
 * Shown for `admin_ui_config.access === 'unavailable'`: the handler could not
 * find out what this operator may do (session store or permission lookup
 * failed). That says nothing about the operator, so the page is neutral: no
 * accusation, no countdown, no redirect — just a way to try again.
 */
import type { ReactNode } from 'react';

import Button from '@mui/material/Button';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { StatusPage } from '@/shared/ui/status-page';

export interface AdminAccessUnavailableProps {
  /** Reload seam so tests do not touch `window.location`. */
  readonly reload?: () => void;
}

function reloadPage(): void {
  window.location.reload();
}

export function AdminAccessUnavailable({ reload = reloadPage }: AdminAccessUnavailableProps = {}): ReactNode {
  return (
    <StatusPage
      testId="admin-access-unavailable"
      title={t('pages.admin.unavailable.title', "We couldn't check your permissions")}
      actions={
        <Button variant="contained" onClick={reload}>
          {t('pages.admin.unavailable.retry', 'Try again')}
        </Button>
      }
    >
      <Typography
        component="p"
        variant="bodyMedium"
        sx={(theme: Theme) => ({ color: theme.vars.palette.text.secondary, margin: 0 })}
      >
        {t(
          'pages.admin.unavailable.explanation',
          'Something went wrong on our side while loading your access. Try again in a moment.',
        )}
      </Typography>
    </StatusPage>
  );
}
