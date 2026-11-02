/**
 * "Take back requested": the web's Take back button (on a thread this
 * computer sent to the cloud) raises a notification of kind
 * `workspace_reclaim_requested`; only this computer can move the thread
 * back, so the shell shows it as a banner under the toolbar with a Take back action. The action
 * opens the thread and starts the take back dialog there.
 *
 * It reads the latest unread notifications from the desktop's one
 * notifications subscription (`useDesktopNotifications`), which the frame
 * holds through this component, and shows each request once.
 */
import { useEffect, useState } from 'react';

import Button from '@mui/material/Button';

import { readThreads, useWorkspaceIpc } from '@/features/workspace';
import { readThreadUuids, useTakeBackRequests } from '@/features/workspace-handoff';
import { useReadNotification, type NormalizedNotification } from '@/features/notifications';
import { t } from '@/shared/i18n';

import type { ShellAction } from '../lib/shellActions';
import { useDesktopNotifications, type DesktopNotificationsFeed } from '../model/useDesktopNotifications';
import { NoticeBanner } from './NoticeBanner';

/** The notification kind the server raises for a web Take back request (ADR-0034, decision 7). */
const RECLAIM_REQUESTED = 'workspace_reclaim_requested';

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

/** The frame's notifications: the one subscription, and the Take back banner it feeds. */
export function ReclaimRequestToast({ run }: { run: (action: ShellAction) => void }): React.JSX.Element | null {
  const feed = useDesktopNotifications();
  return <ReclaimRequestBanner feed={feed} run={run} />;
}

function ReclaimRequestBanner({ feed, run }: { feed: DesktopNotificationsFeed; run: (action: ShellAction) => void }): React.JSX.Element | null {
  const { projectId, rows } = feed;
  const workspaces = useWorkspaceIpc();
  const markRead = useReadNotification();
  const [handled, setHandled] = useState<ReadonlySet<string>>(new Set());

  const request = workspaces === undefined ? undefined : rows.find((row: NormalizedNotification) => String(row.eventType) === RECLAIM_REQUESTED && !handled.has(row.id));
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
    <NoticeBanner
      tone="info"
      testId="reclaim-request-toast"
      onClose={() => finish(located.id)}
      actions={
        <Button size="small" onClick={takeBack}>
          {t('desktop.handoff.takeBack', 'Take back')}
        </Button>
      }
    >
      {t('desktop.handoff.reclaimRequested', 'You asked to take a thread back from the cloud. Take it back here?')}
    </NoticeBanner>
  );
}
