/**
 * "Runs on <thread>" — the line a schedule or a trigger card shows for a
 * `workspace_thread` target, and nothing for any other target. The thread's
 * name links to its conversation, as the `run_finished` notification of an
 * occurrence does.
 */
import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';
import { Link } from '@tanstack/react-router';

import { t } from '@/shared/i18n';

import { useHandedOffThreads } from '../api/useHandedOffThreads';
import { isThreadTarget, type StoredThreadTarget } from '../model/threadTarget';

export interface ThreadTargetLineProps {
  readonly projectId: string | undefined;
  /** The stored schedule or trigger. */
  readonly target: StoredThreadTarget | undefined;
  readonly testId?: string;
}

export function ThreadTargetLine({ projectId, target, testId = 'thread-target-line' }: ThreadTargetLineProps): ReactNode {
  const uuid = isThreadTarget(target) ? (target?.conversation_uuid ?? '') : '';
  const { names } = useHandedOffThreads(projectId, uuid !== '');
  if (uuid === '') return null;
  const title = names.get(uuid) ?? t('threadTarget.unnamedThread', 'a handed-off thread');
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
