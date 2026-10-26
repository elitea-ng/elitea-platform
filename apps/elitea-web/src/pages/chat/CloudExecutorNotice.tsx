/**
 * What a thread shows while its cloud copy runs it (ADR-0034): a "Cloud
 * executor" badge, and the two things a person can do from here.
 *
 * - **Take back** asks the owner's desktop to take the thread back. It
 *   changes nothing in the thread: only the desktop can move it (the
 *   thread never waits on a laptop that is off), so the composer keeps
 *   working in the meantime. Owner only.
 * - **Erase cloud copy** deletes the files the cloud holds and stops its
 *   running turn; the thread goes back to the desktop's folder as it was.
 *   The owner, or anyone who can edit the project's settings.
 *
 * `returning` (the desktop is taking the thread back) replaces the composer:
 * a new cloud turn would not start, so there is nothing to type into.
 */
import { lazy, Suspense, useEffect, useState } from 'react';

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';

import { handoffInfoKey, useHandoffLiveEvents } from '@/features/workspace-handoff';
import { usePermissionList } from '@/shared/api/generated/auth/auth';
import type { Permission } from '@/shared/api/generated/model';
import { eraseWorkspaceHandoff, getWorkspaceHandoff, requestWorkspaceReclaim, type WorkspaceHandoffInfo } from '@/shared/api/workspaceHandoffApi';
import { i18n, t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { changedCount, useCloudChanges } from './cloudChanges';
import cloudCatalogue from './i18n/en.chatCloud.json';

// The Files changed dialog loads when it is opened.
const CloudFilesChanged = lazy(() => import('./CloudFilesChanged'));
// The pull-request credential picker: the owner's only (ADR-0034 decision 10, amended 2026-10-26).
const CloudPullRequestCredential = lazy(() => import('./CloudPullRequestCredential'));

// The notice's strings ride with this (lazy) chat chunk, not the web app's
// initial catalogue: only a handed-off Local work thread renders them. Deep
// merge, no overwrite (scripts/i18n-backfill.mjs, ROUTE_CATALOGUES).
i18n.addResourceBundle('en', 'translation', cloudCatalogue, true, false);

export interface CloudExecutorNoticeProps {
  readonly projectId: string;
  readonly conversationUuid: string;
  readonly executor: 'cloud' | 'returning';
  /** The signed-in user is the conversation's creator. */
  readonly isOwner: boolean;
}

function useCanEditProjectSettings(projectId: string): boolean {
  const query = usePermissionList(projectId, { query: { enabled: projectId !== '' } });
  const list = query.data?.data as Permission[] | undefined;
  return list?.some((entry) => entry.enabled && entry.name === PERMISSIONS.projectSettings.edit) === true;
}

const INFO_POLL_MS = 10_000;

type Toast = { readonly severity: 'success' | 'error'; readonly message: string };

function EraseDialog({ open, pending, onClose, onConfirm }: { open: boolean; pending: boolean; onClose: () => void; onConfirm: () => void }): React.JSX.Element {
  return (
    <Dialog open={open} onClose={pending ? undefined : onClose} aria-labelledby="cloud-erase-title">
      <DialogTitle id="cloud-erase-title">{t('pages.chat.cloud.eraseTitle', 'Erase the cloud copy?')}</DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">
          {t(
            'pages.chat.cloud.eraseBody',
            'The files in the cloud, and the changes the cloud made to them, are deleted, and a turn that is running is stopped. The thread goes back to the desktop it came from.',
          )}
        </Typography>
      </DialogContent>
      <DialogActions>
        <Button disabled={pending} onClick={onClose}>
          {t('pages.chat.cloud.keep', 'Keep it')}
        </Button>
        <Button variant="contained" color="error" disabled={pending} onClick={onConfirm}>
          {t('pages.chat.cloud.erase', 'Erase cloud copy')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

interface ActionsProps {
  readonly returning: boolean;
  readonly isOwner: boolean;
  readonly canErase: boolean;
  readonly requesting: boolean;
  readonly onTakeBack: () => void;
  readonly onErase: () => void;
}

/** Take back for the owner; Erase for the owner or a project settings editor. */
function Actions({ returning, isOwner, canErase, requesting, onTakeBack, onErase }: ActionsProps): React.JSX.Element {
  return (
    <>
      {isOwner && !returning && (
        <Button size="small" variant="contained" disabled={requesting} onClick={onTakeBack}>
          {t('pages.chat.cloud.takeBack', 'Take back')}
        </Button>
      )}
      {canErase && (
        <Button size="small" color="inherit" onClick={onErase}>
          {t('pages.chat.cloud.erase', 'Erase cloud copy')}
        </Button>
      )}
    </>
  );
}

/** "Files changed (N)": what the cloud turns changed since the handoff; hidden until a turn recorded some. */
function FilesChanged({ projectId, conversationUuid, running }: { projectId: string; conversationUuid: string; running: boolean }): React.JSX.Element | null {
  const [open, setOpen] = useState(false);
  const changes = useCloudChanges(projectId, conversationUuid, open, running);
  const count = changedCount(changes.data);
  if (changes.data === undefined || changes.data.turns.length === 0) return null;
  return (
    <>
      <Button size="small" color="inherit" data-testid="cloud-files-changed-button" onClick={() => setOpen(true)}>
        {t('pages.chat.cloud.files.open', 'Files changed ({{n}})', { n: String(count) })}
      </Button>
      {open && (
        <Suspense fallback={null}>
          <CloudFilesChanged projectId={projectId} conversationUuid={conversationUuid} open={open} onClose={() => setOpen(false)} />
        </Suspense>
      )}
    </>
  );
}

interface HandoffView {
  readonly requested: boolean;
  readonly running: boolean;
  readonly pullRequestCredential: WorkspaceHandoffInfo['pull_request_credential'];
}

function useHandoffInfo(projectId: string, conversationUuid: string): HandoffView {
  const queryClient = useQueryClient();
  // The desktop taking the thread back (or an erase elsewhere) moves it without this page doing anything: the live stream says so at once, the poll is the fallback.
  useHandoffLiveEvents(projectId);
  const info = useQuery({
    queryKey: handoffInfoKey(projectId, conversationUuid),
    queryFn: async () => {
      const fresh = await getWorkspaceHandoff(projectId, conversationUuid);
      // A read that left before a delivered frame may answer after it: the
      // frame stays, the pre-commit answer does not move the thread backwards.
      const previous = queryClient.getQueryData<WorkspaceHandoffInfo>(handoffInfoKey(projectId, conversationUuid));
      if (previous !== undefined && previous.epoch > fresh.epoch) return previous;
      return fresh;
    },
    retry: false,
    refetchInterval: INFO_POLL_MS,
  });
  const state = info.data?.state;
  useEffect(() => {
    // The thread is local again: re-read the conversation, so the composer and the badge follow.
    if (state === 'LOCAL') void queryClient.invalidateQueries({ queryKey: ['conversation'] });
  }, [state, queryClient]);
  return { requested: info.data?.reclaim_pending === true, running: state === 'CLOUD_RUNNING', pullRequestCredential: info.data?.pull_request_credential };
}

export default function CloudExecutorNotice({ projectId, conversationUuid, executor, isOwner }: CloudExecutorNoticeProps): React.JSX.Element {
  const queryClient = useQueryClient();
  const canEditSettings = useCanEditProjectSettings(projectId);
  const [confirming, setConfirming] = useState(false);
  const [toast, setToast] = useState<Toast | null>(null);
  const { requested, running, pullRequestCredential } = useHandoffInfo(projectId, conversationUuid);

  const reclaim = useMutation({
    mutationFn: () => requestWorkspaceReclaim(projectId, conversationUuid),
    onSuccess: () => setToast({ severity: 'success', message: t('pages.chat.cloud.sentToDesktop', 'Sent to your desktop') }),
    onError: () => setToast({ severity: 'error', message: t('pages.chat.cloud.requestFailed', 'The request could not be sent. Try again.') }),
  });
  const erase = useMutation({
    mutationFn: () => eraseWorkspaceHandoff(projectId, conversationUuid),
    onSuccess: () => {
      setConfirming(false);
      setToast({ severity: 'success', message: t('pages.chat.cloud.erased', 'The cloud copy was erased') });
      // The conversation's `workspace_executor` is local again: read it, and the thread, afresh.
      void queryClient.invalidateQueries({ queryKey: ['conversation'] });
      void queryClient.invalidateQueries({ queryKey: handoffInfoKey(projectId, conversationUuid) });
    },
    onError: () => {
      setConfirming(false);
      setToast({ severity: 'error', message: t('pages.chat.cloud.eraseFailed', 'The cloud copy could not be erased. Try again.') });
    },
  });

  const returning = executor === 'returning';
  return (
    <>
      <Alert severity={returning ? 'warning' : 'info'} icon={false} data-testid="cloud-executor-notice" sx={{ marginBottom: 1 }}>
        <Box sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 1 }}>
          <Chip
            size="small"
            color={returning ? 'warning' : 'info'}
            data-testid="cloud-executor-badge"
            label={returning ? t('pages.chat.cloud.badgeReturning', 'Returning to desktop') : t('pages.chat.cloud.badge', 'Cloud executor')}
          />
          <Typography variant="bodyMedium" sx={{ flex: 1, minWidth: '12rem' }}>
            {returning
              ? t('pages.chat.cloud.returningBody', 'This thread is being taken back to the desktop it came from. You can read it here; to continue, use the desktop app.')
              : t('pages.chat.cloud.body', 'This thread works on a copy of your desktop folder in the cloud. Take it back to continue on your desktop.')}
          </Typography>
          <FilesChanged projectId={projectId} conversationUuid={conversationUuid} running={running} />
          {isOwner && !returning && (
            <Suspense fallback={null}>
              <CloudPullRequestCredential
                projectId={projectId}
                conversationUuid={conversationUuid}
                current={pullRequestCredential}
                onError={(message) => setToast({ severity: 'error', message })}
              />
            </Suspense>
          )}
          <Actions
            returning={returning}
            isOwner={isOwner}
            canErase={isOwner || canEditSettings}
            requesting={reclaim.isPending}
            onTakeBack={() => reclaim.mutate()}
            onErase={() => setConfirming(true)}
          />
        </Box>
        {requested && (
          <Typography variant="bodySmall" data-testid="cloud-reclaim-pending">
            {t('pages.chat.cloud.reclaimPending', 'Take back requested: it starts when the running cloud turn ends.')}
          </Typography>
        )}
      </Alert>
      <EraseDialog open={confirming} pending={erase.isPending} onClose={() => setConfirming(false)} onConfirm={() => erase.mutate()} />
      <Snackbar open={toast !== null} autoHideDuration={5000} onClose={() => setToast(null)} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
        {toast === null ? undefined : (
          <Alert severity={toast.severity} variant="filled" onClose={() => setToast(null)}>
            {toast.message}
          </Alert>
        )}
      </Snackbar>
    </>
  );
}
