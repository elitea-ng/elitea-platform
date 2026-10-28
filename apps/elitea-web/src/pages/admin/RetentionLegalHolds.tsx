/**
 * The Legal holds section of Admin > Data retention (issue 1495).
 *
 * A hold on a project, an agent (the agent's own project and its id) or a
 * user (the conversation owner) suspends every deletion of the conversations
 * it covers: by the retention sweep and by the user's own delete. A release
 * resumes deletion on the next sweep. A hold is released once.
 */
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Switch from '@mui/material/Switch';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import type { LegalHold, LegalHoldCreate } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import {
  retentionFailureReason,
  useCreateLegalHold,
  useLegalHolds,
  useReleaseLegalHold,
} from './api/adminRetentionApi';

type HoldScope = 'project' | 'agent' | 'user';

function subject(hold: LegalHold): string {
  switch (hold.scope) {
    case 'project':
      return t('pages.admin.retention.holds.subject.project', 'Project {{project}}', {
        project: hold.project_id ?? '',
      });
    case 'agent':
      return t('pages.admin.retention.holds.subject.agent', 'Agent {{agent}} in project {{project}}', {
        agent: hold.agent_id ?? '',
        project: hold.project_id ?? '',
      });
    case 'user':
      return t('pages.admin.retention.holds.subject.user', 'User {{user}}', { user: hold.user_id ?? '' });
    default:
      return hold.scope;
  }
}

function positiveId(value: string): number | undefined {
  const trimmed = value.trim();
  if (!/^\d+$/.test(trimmed)) return undefined;
  const id = Number(trimmed);
  return id > 0 ? id : undefined;
}

function CreateHoldDialog({ open, onClose }: { readonly open: boolean; readonly onClose: () => void }) {
  const create = useCreateLegalHold();
  const [scope, setScope] = useState<HoldScope>('project');
  const [projectId, setProjectId] = useState('');
  const [agentId, setAgentId] = useState('');
  const [userId, setUserId] = useState('');
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');

  const close = () => {
    setError('');
    onClose();
  };

  const onSubmit = () => {
    const body: LegalHoldCreate = { scope, reason: reason.trim() };
    const project = positiveId(projectId);
    const agent = positiveId(agentId);
    const user = positiveId(userId);
    if (scope === 'project' || scope === 'agent') {
      if (project === undefined) {
        setError(t('pages.admin.retention.holds.error.project', 'Enter a project id.'));
        return;
      }
      body.project_id = project;
    }
    if (scope === 'agent') {
      if (agent === undefined) {
        setError(t('pages.admin.retention.holds.error.agent', 'Enter an agent id.'));
        return;
      }
      body.agent_id = agent;
    }
    if (scope === 'user') {
      if (user === undefined) {
        setError(t('pages.admin.retention.holds.error.user', 'Enter a user id.'));
        return;
      }
      body.user_id = user;
    }
    if (body.reason === '') {
      setError(t('pages.admin.retention.holds.error.reason', 'Enter a reason.'));
      return;
    }
    create.mutate(body, {
      onSuccess: () => {
        setProjectId('');
        setAgentId('');
        setUserId('');
        setReason('');
        close();
      },
      onError: (failure) =>
        setError(
          retentionFailureReason(failure) ??
            t('pages.admin.retention.holds.error.create', 'Failed to place the legal hold.'),
        ),
    });
  };

  return (
    <Dialog open={open} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>{t('pages.admin.retention.holds.create', 'Place a legal hold')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', paddingTop: '0.5rem' }}>
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.retention.holds.createHelp',
            'The hold stops every deletion of the conversations it covers, including a user deleting their own conversation, until it is released.',
          )}
        </Typography>
        {error !== '' ? <Alert severity="error">{error}</Alert> : null}
        <TextField
          select
          label={t('pages.admin.retention.holds.scope', 'Scope')}
          value={scope}
          onChange={(event) => setScope(event.target.value as HoldScope)}
        >
          <MenuItem value="project">{t('pages.admin.retention.holds.scope.project', 'Project')}</MenuItem>
          <MenuItem value="agent">{t('pages.admin.retention.holds.scope.agent', 'Agent')}</MenuItem>
          <MenuItem value="user">{t('pages.admin.retention.holds.scope.user', 'User')}</MenuItem>
        </TextField>
        {scope !== 'user' ? (
          <TextField
            label={t('pages.admin.retention.override.project', 'Project id')}
            value={projectId}
            onChange={(event) => setProjectId(event.target.value)}
          />
        ) : null}
        {scope === 'agent' ? (
          <TextField
            label={t('pages.admin.retention.holds.agentId', 'Agent id')}
            value={agentId}
            onChange={(event) => setAgentId(event.target.value)}
          />
        ) : null}
        {scope === 'user' ? (
          <TextField
            label={t('pages.admin.retention.holds.userId', 'User id')}
            value={userId}
            onChange={(event) => setUserId(event.target.value)}
          />
        ) : null}
        <TextField
          label={t('pages.admin.retention.holds.reason', 'Reason')}
          value={reason}
          multiline
          minRows={2}
          onChange={(event) => setReason(event.target.value)}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('pages.admin.retention.cancel', 'Cancel')}</Button>
        <Button variant="contained" disabled={create.isPending} onClick={onSubmit}>
          {t('pages.admin.retention.holds.place', 'Place hold')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function ReleaseHoldDialog({ hold, onClose }: { readonly hold: LegalHold | null; readonly onClose: () => void }) {
  const release = useReleaseLegalHold();
  const [reason, setReason] = useState('');
  const [error, setError] = useState('');

  const close = () => {
    setReason('');
    setError('');
    onClose();
  };

  return (
    <Dialog open={hold !== null} onClose={close} fullWidth maxWidth="sm">
      <DialogTitle>{t('pages.admin.retention.holds.releaseTitle', 'Release the legal hold?')}</DialogTitle>
      <DialogContent sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem', paddingTop: '0.5rem' }}>
        <Typography variant="bodyMedium">{hold ? subject(hold) : ''}</Typography>
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'pages.admin.retention.holds.releaseHelp',
            'Deletion of what this hold covers resumes on the next retention sweep. A released hold cannot be placed again; place a new one if you need it.',
          )}
        </Typography>
        {error !== '' ? <Alert severity="error">{error}</Alert> : null}
        <TextField
          label={t('pages.admin.retention.holds.releaseReason', 'Release reason (optional)')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={close}>{t('pages.admin.retention.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          color="error"
          disabled={release.isPending || hold === null}
          onClick={() => {
            if (hold === null) return;
            release.mutate(
              { id: hold.id, reason: reason.trim() },
              {
                onSuccess: close,
                onError: (failure) =>
                  setError(
                    retentionFailureReason(failure) ??
                      t('pages.admin.retention.holds.error.release', 'Failed to release the legal hold.'),
                  ),
              },
            );
          }}
        >
          {t('pages.admin.retention.holds.release', 'Release')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

export function RetentionLegalHolds() {
  const [activeOnly, setActiveOnly] = useState(true);
  const [creating, setCreating] = useState(false);
  const [releasing, setReleasing] = useState<LegalHold | null>(null);
  const holds = useLegalHolds(activeOnly);

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
      <Box sx={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap' }}>
        <FormControlLabel
          control={<Switch checked={activeOnly} onChange={(event) => setActiveOnly(event.target.checked)} />}
          label={t('pages.admin.retention.holds.activeOnly', 'Active holds only')}
        />
        <Button variant="contained" onClick={() => setCreating(true)}>
          {t('pages.admin.retention.holds.create', 'Place a legal hold')}
        </Button>
      </Box>
      {holds.isFetching ? <LinearProgress /> : null}
      {holds.isError ? (
        <Alert severity="warning" data-testid="admin-legal-holds-unavailable">
          {retentionFailureReason(holds.error) ??
            t('pages.admin.retention.holds.error.load', 'Failed to load legal holds.')}
        </Alert>
      ) : null}
      {holds.data && holds.data.length === 0 ? (
        <Typography variant="bodyMedium" color="text.secondary">
          {t('pages.admin.retention.holds.empty', 'No legal holds.')}
        </Typography>
      ) : null}
      {holds.data && holds.data.length > 0 ? (
        <Table size="small" aria-label={t('pages.admin.retention.tab.holds', 'Legal holds')}>
          <TableHead>
            <TableRow>
              <TableCell>{t('pages.admin.retention.holds.column.subject', 'Covers')}</TableCell>
              <TableCell>{t('pages.admin.retention.holds.reason', 'Reason')}</TableCell>
              <TableCell>{t('pages.admin.retention.holds.column.placed', 'Placed')}</TableCell>
              <TableCell>{t('pages.admin.retention.holds.column.status', 'Status')}</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {holds.data.map((hold) => (
              <TableRow key={hold.id} data-testid={`admin-legal-hold-${hold.id}`}>
                <TableCell>{subject(hold)}</TableCell>
                <TableCell>{hold.reason}</TableCell>
                <TableCell>{new Date(hold.created_at).toLocaleString()}</TableCell>
                <TableCell>
                  {hold.active
                    ? t('pages.admin.retention.holds.status.active', 'Active')
                    : t('pages.admin.retention.holds.status.released', 'Released')}
                </TableCell>
                <TableCell>
                  {hold.active ? (
                    <Button size="small" onClick={() => setReleasing(hold)}>
                      {t('pages.admin.retention.holds.release', 'Release')}
                    </Button>
                  ) : null}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : null}
      <CreateHoldDialog open={creating} onClose={() => setCreating(false)} />
      <ReleaseHoldDialog hold={releasing} onClose={() => setReleasing(null)} />
    </Box>
  );
}
