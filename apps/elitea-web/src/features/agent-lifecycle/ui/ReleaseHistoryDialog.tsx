import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import CircularProgress from '@mui/material/CircularProgress';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Tab from '@mui/material/Tab';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Tabs from '@mui/material/Tabs';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { ApplicationRelease, ApplicationReleaseChange } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import {
  useApplicationRelease,
  useApplicationReleases,
  useRollbackRelease,
  useVersionRevisions,
} from '../api/releasesApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';

/**
 * The agent's release history (ADR-0040 decision 2, #1492).
 *
 * Releases: every publish made an immutable release; one of them is the
 * release a run executes when it names none. "Roll back" re-points that to an
 * earlier release, with an optional reason the audit event keeps (the
 * event's name is 256 characters, so the field takes 200). Revisions:
 * every save of the open version, with what it changed.
 */
export interface ReleaseHistoryDialogProps {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  /** The open version, whose draft revisions the second tab lists. */
  readonly versionId: number | undefined;
  readonly onClose: () => void;
}

export function ReleaseHistoryDialog({
  open,
  projectId,
  applicationId,
  versionId,
  onClose,
}: ReleaseHistoryDialogProps): ReactNode {
  const [tab, setTab] = useState<'releases' | 'revisions'>('releases');
  return (
    <Dialog
      open={open}
      onClose={onClose}
      maxWidth="md"
      fullWidth
      data-testid="release-history-dialog"
    >
      <DialogTitle>{t('features.agentLifecycle.releases.title', 'Release history')}</DialogTitle>
      <DialogContent>
        <Tabs
          value={tab}
          onChange={(_, value: 'releases' | 'revisions') => setTab(value)}
        >
          <Tab
            value="releases"
            label={t('features.agentLifecycle.releases.tabReleases', 'Releases')}
          />
          <Tab
            value="revisions"
            label={t('features.agentLifecycle.releases.tabRevisions', 'Revisions of this version')}
          />
        </Tabs>
        <Box sx={{ paddingTop: '1rem' }}>
          {tab === 'releases' ? (
            <ReleasesTab
              open={open}
              projectId={projectId}
              applicationId={applicationId}
            />
          ) : (
            <RevisionsTab
              projectId={projectId}
              applicationId={applicationId}
              versionId={versionId}
            />
          )}
        </Box>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('features.agentLifecycle.releases.close', 'Close')}</Button>
      </DialogActions>
    </Dialog>
  );
}

function ReleasesTab({
  open,
  projectId,
  applicationId,
}: {
  readonly open: boolean;
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}): ReactNode {
  const releases = useApplicationReleases(projectId, applicationId, open);
  const [selected, setSelected] = useState<number>();
  const [target, setTarget] = useState<ApplicationRelease>();
  if (releases.isPending) return <CircularProgress size="1.5rem" />;
  if (releases.isError) {
    return <Alert severity="error">{lifecycleErrorMessage(releases.error)}</Alert>;
  }
  const rows = releases.data.rows;
  if (rows.length === 0) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.releases.empty', 'This agent has no releases yet. Publishing a version creates one.')}
      </Typography>
    );
  }
  return (
    <>
      <Table
        size="small"
        aria-label={t('features.agentLifecycle.releases.tabReleases', 'Releases')}
      >
        <TableHead>
          <TableRow>
            <TableCell>{t('features.agentLifecycle.releases.column.release', 'Release')}</TableCell>
            <TableCell>{t('features.agentLifecycle.releases.column.published', 'Published')}</TableCell>
            <TableCell>{t('features.agentLifecycle.releases.column.digest', 'Digest')}</TableCell>
            <TableCell>{t('features.agentLifecycle.releases.column.state', 'State')}</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.map((release) => (
            <ReleaseRow
              key={release.id}
              release={release}
              onShowChanges={() => setSelected(release.id)}
              onRollback={() => setTarget(release)}
            />
          ))}
        </TableBody>
      </Table>
      {selected !== undefined && (
        <ReleaseChanges
          projectId={projectId}
          applicationId={applicationId}
          releaseId={selected}
        />
      )}
      <RollbackDialog
        projectId={projectId}
        applicationId={applicationId}
        target={target}
        onClose={() => setTarget(undefined)}
      />
    </>
  );
}

function ReleaseRow({
  release,
  onShowChanges,
  onRollback,
}: {
  readonly release: ApplicationRelease;
  readonly onShowChanges: () => void;
  readonly onRollback: () => void;
}): ReactNode {
  const canRollBack = !release.is_published_release && release.state === 'published';
  return (
    <TableRow data-testid={`release-row-${release.id}`}>
      <TableCell>
        {release.name} <Typography variant="bodySmall" component="span" color="text.secondary">#{release.id}</Typography>
      </TableCell>
      <TableCell data-testid="release-time">{new Date(release.created_at).toLocaleString()}</TableCell>
      <TableCell sx={{ fontFamily: 'monospace' }}>{release.definition_digest.slice(0, 12)}</TableCell>
      <TableCell>
        {release.is_published_release ? (
          <Chip
            size="small"
            color="primary"
            label={t('features.agentLifecycle.releases.current', 'Current')}
          />
        ) : release.state === 'withdrawn' ? (
          t('features.agentLifecycle.releases.withdrawn', 'Withdrawn')
        ) : (
          t('features.agentLifecycle.releases.earlier', 'Earlier release')
        )}
      </TableCell>
      <TableCell align="right">
        <Button
          size="small"
          onClick={onShowChanges}
        >
          {t('features.agentLifecycle.releases.changes', 'Changes')}
        </Button>
        {canRollBack && (
          <Button
            size="small"
            onClick={onRollback}
            data-testid={`release-rollback-${release.id}`}
          >
            {t('features.agentLifecycle.releases.rollback', 'Roll back to this')}
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}

function ReleaseChanges({
  projectId,
  applicationId,
  releaseId,
}: {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly releaseId: number;
}): ReactNode {
  const detail = useApplicationRelease(projectId, applicationId, releaseId);
  if (detail.isPending) return <CircularProgress size="1.5rem" />;
  if (detail.isError) return <Alert severity="error">{lifecycleErrorMessage(detail.error)}</Alert>;
  const previous = detail.data.previous_release_id;
  return (
    <Box
      sx={{ paddingTop: '1rem' }}
      data-testid="release-changes"
    >
      <Typography variant="labelMedium">
        {previous === null
          ? t('features.agentLifecycle.releases.firstRelease', 'Release #{{id}} is the first release.', { id: releaseId })
          : t('features.agentLifecycle.releases.changesSince', 'Release #{{id}} compared with release #{{previous}}', {
              id: releaseId,
              previous,
            })}
      </Typography>
      <ChangeList changes={detail.data.changes} />
    </Box>
  );
}

function RevisionsTab({
  projectId,
  applicationId,
  versionId,
}: {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
}): ReactNode {
  const revisions = useVersionRevisions(projectId, applicationId, versionId, true);
  if (revisions.isPending) return <CircularProgress size="1.5rem" />;
  if (revisions.isError) return <Alert severity="error">{lifecycleErrorMessage(revisions.error)}</Alert>;
  if (revisions.data.rows.length === 0) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.releases.noRevisions', 'This version has no saved revisions yet.')}
      </Typography>
    );
  }
  return (
    <Box data-testid="revision-list">
      {revisions.data.rows.map((revision) => (
        <Box
          key={revision.id}
          sx={{ paddingBottom: '0.75rem' }}
        >
          <Typography variant="labelMedium">
            {t('features.agentLifecycle.releases.revisionHeading', 'Revision {{revision}} · user {{actor}} · {{time}}', {
              revision: revision.revision,
              actor: revision.actor_id ?? '—',
              time: new Date(revision.created_at).toLocaleString(),
            })}
          </Typography>
          {revision.kind === 'baseline' ? (
            <Typography
              variant="bodySmall"
              color="text.secondary"
            >
              {t('features.agentLifecycle.releases.baseline', 'The version as it was before its first recorded save.')}
            </Typography>
          ) : (
            <ChangeList changes={revision.diff} />
          )}
        </Box>
      ))}
    </Box>
  );
}

function ChangeList({ changes }: { readonly changes: readonly ApplicationReleaseChange[] }): ReactNode {
  if (changes.length === 0) {
    return (
      <Typography
        variant="bodySmall"
        color="text.secondary"
      >
        {t('features.agentLifecycle.releases.noChanges', 'No changes.')}
      </Typography>
    );
  }
  return (
    <Box
      component="ul"
      sx={{ margin: 0, paddingLeft: '1.25rem' }}
    >
      {changes.map((change) => (
        <Typography
          key={`${change.op}:${change.path}`}
          component="li"
          variant="bodySmall"
          sx={{ fontFamily: 'monospace', wordBreak: 'break-word' }}
        >
          {change.op} {change.path}
          {change.op !== 'remove' && `: ${preview(change.after)}`}
        </Typography>
      ))}
    </Box>
  );
}

function RollbackDialog({
  projectId,
  applicationId,
  target,
  onClose,
}: {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly target: ApplicationRelease | undefined;
  readonly onClose: () => void;
}): ReactNode {
  const [reason, setReason] = useState('');
  const rollback = useRollbackRelease(projectId, applicationId);
  const close = (): void => {
    setReason('');
    rollback.reset();
    onClose();
  };
  return (
    <Dialog
      open={target !== undefined}
      onClose={close}
      data-testid="release-rollback-dialog"
    >
      <DialogTitle>
        {t('features.agentLifecycle.releases.rollbackTitle', 'Roll back to release #{{id}}?', { id: target?.id ?? '' })}
      </DialogTitle>
      <DialogContent>
        <Typography variant="bodyMedium">
          {t(
            'features.agentLifecycle.releases.rollbackBody',
            'New runs of this agent will use this release. Runs already in progress keep the release they started with.',
          )}
        </Typography>
        <TextField
          fullWidth
          margin="normal"
          label={t('features.agentLifecycle.releases.reason', 'Reason (recorded in the audit event)')}
          value={reason}
          onChange={(event) => setReason(event.target.value.slice(0, 200))}
        />
        {rollback.isError && <Alert severity="error">{lifecycleErrorMessage(rollback.error)}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('features.agentLifecycle.releases.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          disabled={target === undefined || rollback.isPending}
          data-testid="release-rollback-confirm"
          onClick={() => {
            if (target === undefined) return;
            rollback.mutate({ releaseId: target.id, reason: reason.trim() }, { onSuccess: close });
          }}
        >
          {t('features.agentLifecycle.releases.rollbackConfirm', 'Roll back')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

/** A changed value, short enough for one line. */
function preview(value: unknown): string {
  const text = typeof value === 'string' ? value : JSON.stringify(value);
  if (text === undefined) return '';
  return text.length > 160 ? `${text.slice(0, 157)}...` : text;
}
