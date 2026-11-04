/**
 * "Runs on <thread>" — the line a schedule or a trigger card shows for a
 * `workspace_thread` target, and nothing for any other target. The thread's
 * name links to its conversation, as the `run_finished` notification of an
 * occurrence does.
 *
 * The name comes from the one conversation's read (`getConversation` takes
 * the UUID), so a colleague's thread is named too when the viewer may read
 * it. A viewer who may not reads "a handed-off thread".
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';
import { Link } from '@tanstack/react-router';

import { useGetConversation } from '@/shared/api/generated/chat/chat';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { isRecord, nonBlankText } from '@/shared/lib/object';

import { isThreadTarget, type StoredThreadTarget } from '../model/threadTarget';

export interface ThreadTargetLineProps {
  readonly projectId: string | undefined;
  /** The stored schedule or trigger. */
  readonly target: StoredThreadTarget | undefined;
  readonly testId?: string;
}

export function ThreadTargetLine({ projectId, target, testId = 'thread-target-line' }: ThreadTargetLineProps): ReactNode {
  const uuid = isThreadTarget(target) ? (target?.conversation_uuid ?? '') : '';
  const query = useGetConversation(projectId ?? '', uuid, undefined, { query: { enabled: uuid !== '' && projectId !== undefined, retry: false } });
  if (uuid === '') return null;
  const body = unwrapBody(query.data);
  const title = (isRecord(body) ? nonBlankText(body['name']) : undefined) ?? t('threadTarget.unnamedThread', 'a handed-off thread');
  return (
    <Typography variant="bodySmall" color="text.secondary" data-testid={testId}>
      {t('threadTarget.runsOn', 'Runs on')}{' '}
      {projectId === undefined ? (
        title
      ) : (
        <Link to="/$projectId/$" params={{ projectId, _splat: 'chat' }} search={{ conversation: uuid }}>
          {title}
        </Link>
      )}
    </Typography>
  );
}
