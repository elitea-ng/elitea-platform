/**
 * The bell row of a `run_finished` notification (ADR-0045 decision 5, U3
 * contract §6): the fixed text for its outcome and a link to where the run
 * happened — the conversation, or (in the desktop app, for a chain that ran
 * on this computer) the thread. The row holds no content, so nothing here
 * comes from the server but ids.
 *
 * The conversation opens through `/$projectId/$` (ROUTE-070), which selects
 * the row's project first — the run may be in a project other than the one
 * on screen. The thread opens through `/workspaces?conversation=<thread>`,
 * which finds the folder on this computer that holds it.
 */
import type { MouseEvent, ReactElement } from 'react';

import { useNavigate } from '@tanstack/react-router';

import Link from '@mui/material/Link';
import Typography from '@mui/material/Typography';
import type { TypographyProps } from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { FullNotificationMeta } from '../api/normalize';
import { runFinishedTarget, runFinishedText } from '../lib/runFinished';

export interface RunFinishedMessageProps {
  readonly meta: FullNotificationMeta | undefined;
  readonly projectId: string | undefined;
  readonly textVariant: TypographyProps['variant'];
  readonly textColor: string;
  readonly linkColor: string;
  readonly onCloseNotificationList?: (() => void) | undefined;
}

const IN_DESKTOP_APP = import.meta.env.MODE === 'desktop';

export function RunFinishedMessage(props: RunFinishedMessageProps): ReactElement {
  const { meta, projectId, textVariant, textColor, linkColor, onCloseNotificationList } = props;
  const navigate = useNavigate();
  const { title, body } = runFinishedText(meta?.outcome);
  const target = runFinishedTarget(meta, projectId, IN_DESKTOP_APP);

  const open = (event: MouseEvent): void => {
    event.preventDefault();
    onCloseNotificationList?.();
    if (target === null) return;
    if (target.kind === 'thread') {
      void navigate({ to: '/workspaces', search: { conversation: target.threadId } });
      return;
    }
    void navigate({ to: '/$projectId/$', params: { projectId: target.projectId, _splat: 'chat' }, search: { conversation: target.conversationUuid } });
  };

  return (
    <Typography
      variant={textVariant}
      sx={{ color: textColor }}
      data-testid="run-finished-message"
    >
      <strong>{title}</strong> {body}
      {target !== null && (
        <>
          {' '}
          <Link
            component="button"
            variant={textVariant}
            sx={{ textDecoration: 'underline', cursor: 'pointer', color: linkColor, verticalAlign: 'baseline' }}
            onClick={open}
          >
            {target.kind === 'thread' ? t('notifications.runFinished.openThread', 'Open the thread') : t('notifications.runFinished.openConversation', 'Open the conversation')}
          </Link>
        </>
      )}
    </Typography>
  );
}
