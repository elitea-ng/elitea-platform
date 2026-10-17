/**
 * "Take back requested": the web's Take back button (on a thread this
 * computer sent to the cloud) raises a notification of kind
 * `workspace_reclaim_requested`; only this computer can move the thread
 * back, so the shell shows it as a toast with a Take back action. The action
 * opens the thread and starts the take back dialog there.
 *
 * It listens like the bell does (the notifications stream signals, the list
 * says what) and shows each request once.
 */
import { useCallback, useEffect, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import CloseIcon from '@mui/icons-material/Close';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Snackbar from '@mui/material/Snackbar';

import { readThreads, useWorkspaceIpc } from '@/features/workspace';
import { readThreadUuids, useTakeBackRequests } from '@/features/workspace-handoff';
import { NOTIFICATIONS_QUERY_ROOT, useNotificationsList, useNotificationsSSE, useReadNotification, type NormalizedNotification } from '@/features/notifications';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';

/** The notification kind the server raises for a web Take back request (ADR-0034, decision 7). */
const RECLAIM_REQUESTED = 'workspace_reclaim_requested';

const SLOW_POLL_MS = 60_000;

/** The folder and thread on this computer a request names: by conversation id, or by the UUID the thread list remembered. */
async function locate(
  reference: string,
  listWorkspaces: () => Promise<readonly { id: string }[]>,
): Promise<{ workspaceId: string; conversationId: string } | null> {
  const wanted = reference.toLowerCase();
  for (const workspace of await listWorkspaces().catch(() => [])) {
    const uuids = readThreadUuids(workspace.id);
    const thread = readThreads(workspace.id).find((entry) => entry.id.toLowerCase() === wanted || uuids[entry.id]?.toLowerCase() === wanted);
    if (thread !== undefined) return { workspaceId: workspace.id, conversationId: thread.id };
  }
  return null;
}

export function ReclaimRequestToast({ run }: { run: (action: ShellAction) => void }): React.JSX.Element | null {
  const queryClient = useQueryClient();
  const workspaces = useWorkspaceIpc();
  const author = useGetCurrentAuthor();
  const projectId = (unwrapBody(author.data) as SocialAuthorProfile | undefined)?.personal_project_id;
  const markRead = useReadNotification();
  const [handled, setHandled] = useState<ReadonlySet<string>>(new Set());

  const onNotify = useCallback(() => void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT }), [queryClient]);
  const { streamDead } = useNotificationsSSE(projectId === undefined ? undefined : String(projectId), onNotify);
  const list = useNotificationsList(
    { projectId: projectId ?? '', pageSize: 10, params: { only_new: true } },
    { enabled: projectId !== undefined && workspaces !== undefined, refetchInterval: streamDead ? SLOW_POLL_MS : false },
  );

  const request = list.data?.rows.find((row: NormalizedNotification) => String(row.eventType) === RECLAIM_REQUESTED && !handled.has(row.id));
  const reference = request?.meta?.conversationId;

  // A request for a thread this computer does not hold is not ours to show.
  const [located, setLocated] = useState<{ id: string; workspaceId: string; conversationId: string } | null>(null);
  useEffect(() => {
    let live = true;
    if (request === undefined || reference === undefined || workspaces === undefined) {
      setLocated(null);
      return undefined;
    }
    void locate(reference, () => workspaces.list()).then((found) => {
      if (live) setLocated(found === null ? null : { id: request.id, ...found });
    });
    return () => {
      live = false;
    };
  }, [request, reference, workspaces]);

  const finish = (id: string): void => {
    setHandled((current) => new Set(current).add(id));
    if (projectId !== undefined) markRead.mutate({ projectId, id });
  };

  if (located === null || reference === undefined) return null;
  const takeBack = (): void => {
    useTakeBackRequests.getState().request(reference);
    run({ type: 'open_workspace', workspaceId: located.workspaceId, conversationId: located.conversationId });
    finish(located.id);
  };
  return (
    <Snackbar open anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }} onClose={(_event, why) => why !== 'clickaway' && finish(located.id)}>
      <Alert
        severity="info"
        variant="filled"
        data-testid="reclaim-request-toast"
        // `action` replaces the Alert's own close button, so the dismiss is drawn here.
        action={
          <>
            <Button color="inherit" size="small" onClick={takeBack}>
              {t('desktop.handoff.takeBack', 'Take back')}
            </Button>
            <IconButton color="inherit" size="small" aria-label={t('desktop.handoff.dismiss', 'Dismiss')} onClick={() => finish(located.id)}>
              <CloseIcon fontSize="inherit" />
            </IconButton>
          </>
        }
      >
        {t('desktop.handoff.reclaimRequested', 'You asked to take a thread back from the cloud. Take it back here?')}
      </Alert>
    </Snackbar>
  );
}
