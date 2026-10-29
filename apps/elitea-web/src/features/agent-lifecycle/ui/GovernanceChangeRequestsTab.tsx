import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import CircularProgress from '@mui/material/CircularProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentChangeRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { useChangeRequests, useRequestChange } from '../api/governanceApi';
import { lifecycleErrorMessage } from '../model/useEntityLifecycle';
import { ChangeRequestDetail } from './ChangeRequestDetail';
import { changeRequestKindLabel, changeRequestStateLabel, formatUser, formatWhen } from './governanceLabels';

/**
 * The Change requests tab: every release and recertification request of the
 * agent, newest first, and one of them in detail. "Start recertification"
 * creates and submits a `recertification` request for the published release,
 * which runs the release gate against it and, once approved, certifies it.
 */
export interface GovernanceChangeRequestsTabProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

export function GovernanceChangeRequestsTab({ projectId, applicationId }: GovernanceChangeRequestsTabProps): ReactNode {
  const list = useChangeRequests(projectId, applicationId);
  const [selected, setSelected] = useState<number>();
  if (projectId === undefined || applicationId === undefined) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.governance.notSaved', 'Save the agent first; its registry record starts with it.')}
      </Typography>
    );
  }
  if (list.isPending) return <CircularProgress size="1.5rem" />;
  if (list.isError) return <Alert severity="error">{lifecycleErrorMessage(list.error)}</Alert>;
  return (
    <Box
      sx={stackSx}
      data-testid="governance-change-requests"
    >
      <StartRecertification
        projectId={projectId}
        applicationId={applicationId}
        onStarted={setSelected}
      />
      {list.data.rows.length === 0 ? (
        <Typography variant="bodyMedium">
          {t(
            'features.agentLifecycle.changeRequests.empty',
            'No change requests yet. Requesting a publish of a medium, high or critical risk agent creates one.',
          )}
        </Typography>
      ) : (
        <ChangeRequestTable
          rows={list.data.rows}
          selected={selected}
          onSelect={setSelected}
        />
      )}
      {selected !== undefined && (
        <ChangeRequestDetail
          projectId={projectId}
          applicationId={applicationId}
          changeRequestId={selected}
        />
      )}
    </Box>
  );
}

function ChangeRequestTable({
  rows,
  selected,
  onSelect,
}: {
  readonly rows: readonly AgentChangeRequest[];
  readonly selected: number | undefined;
  readonly onSelect: (id: number) => void;
}): ReactNode {
  return (
    <Table
      size="small"
      aria-label={t('features.agentLifecycle.governance.tabChanges', 'Change requests')}
    >
      <TableHead>
        <TableRow>
          <TableCell>{t('features.agentLifecycle.changeRequests.column.request', 'Request')}</TableCell>
          <TableCell>{t('features.agentLifecycle.changeRequests.column.state', 'State')}</TableCell>
          <TableCell>{t('features.agentLifecycle.changeRequests.column.version', 'Version')}</TableCell>
          <TableCell>{t('features.agentLifecycle.changeRequests.column.proposed', 'Proposed')}</TableCell>
          <TableCell>{t('features.agentLifecycle.changeRequests.column.approvals', 'Approvals')}</TableCell>
          <TableCell />
        </TableRow>
      </TableHead>
      <TableBody>
        {rows.map((row) => (
          <TableRow
            key={row.id}
            selected={row.id === selected}
            data-testid={`change-request-row-${row.id}`}
          >
            <TableCell>
              #{row.id} {changeRequestKindLabel(row.kind)}
            </TableCell>
            <TableCell>{changeRequestStateLabel(row.state)}</TableCell>
            <TableCell>{row.version_name === '' ? '—' : row.version_name}</TableCell>
            <TableCell>
              {formatUser(row.proposed_by)} · {formatWhen(row.created_at)}
            </TableCell>
            <TableCell>
              {row.approvals.length} / {row.required_approvals}
            </TableCell>
            <TableCell align="right">
              <Button
                size="small"
                onClick={() => onSelect(row.id)}
                data-testid={`change-request-open-${row.id}`}
              >
                {t('features.agentLifecycle.changeRequests.open', 'Details')}
              </Button>
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function StartRecertification({
  projectId,
  applicationId,
  onStarted,
}: {
  readonly projectId: string;
  readonly applicationId: number;
  readonly onStarted: (id: number) => void;
}): ReactNode {
  const [open, setOpen] = useState(false);
  const [summary, setSummary] = useState('');
  const request = useRequestChange(projectId, applicationId);
  if (!open) {
    return (
      <Box>
        <Button
          variant="outlined"
          size="small"
          onClick={() => setOpen(true)}
          data-testid="start-recertification"
        >
          {t('features.agentLifecycle.changeRequests.startRecertification', 'Start recertification')}
        </Button>
      </Box>
    );
  }
  return (
    <Box sx={stackSx}>
      <TextField
        label={t('features.agentLifecycle.changeRequests.recertificationSummary', 'Why recertify, for the approvers')}
        value={summary}
        onChange={(event) => setSummary(event.target.value)}
        size="small"
        slotProps={{ htmlInput: { maxLength: 2000, 'data-testid': 'recertification-summary' } }}
      />
      {request.error !== null && <Alert severity="error">{lifecycleErrorMessage(request.error)}</Alert>}
      <Box sx={actionsSx}>
        <Button
          size="small"
          variant="contained"
          disabled={summary.trim() === '' || request.isPending}
          data-testid="recertification-submit"
          onClick={() =>
            request.mutate(
              { kind: 'recertification', summary: summary.trim() },
              {
                onSuccess: (created) => {
                  setOpen(false);
                  setSummary('');
                  onStarted(created.id);
                },
              },
            )
          }
        >
          {t('features.agentLifecycle.changeRequests.submitRecertification', 'Submit recertification')}
        </Button>
        <Button
          size="small"
          onClick={() => setOpen(false)}
        >
          {t('features.agentLifecycle.requestPublish.cancel', 'Cancel')}
        </Button>
      </Box>
    </Box>
  );
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.75rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', gap: '0.5rem' };
