/**
 * Admin › Connector requests (ADR-0044 decision 1, #1501): the approval
 * queue of the default-deny connector catalogue. Each request shows what an
 * approver needs to judge it: the requester, the connector, the projects (or
 * the whole platform), the declared tools with their ADR-0041 categories,
 * the egress hosts and the data classes. Approve writes the toolkit-type
 * grants and the worker egress allowlist entries in one transaction; reject
 * needs a reason. The requester can never decide their own request (the
 * server refuses it). Behind `connector.approve`, like the server routes.
 *
 * "Create requests for connectors in use" is the on-ramp to approved_only:
 * one submitted request per toolkit type or MCP server already configured in
 * a project, for ANOTHER approver to decide (the creator cannot).
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import LinearProgress from '@mui/material/LinearProgress';
import MenuItem from '@mui/material/MenuItem';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ConnectorRequest, ConnectorRequestState } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import {
  describeDecisionError,
  useAdminConnectorRequests,
  useConnectorInventory,
  useCreateInventoryRequests,
  useDecideConnectorRequest,
} from './api/adminConnectorRequestsApi';

const STATES: readonly (ConnectorRequestState | '')[] = ['submitted', 'approved', 'rejected', 'withdrawn', 'draft', ''];

function stateLabel(state: ConnectorRequestState | ''): string {
  return state === '' ? t('pages.admin.connectorRequests.state.all', 'All') : state;
}

function requester(row: ConnectorRequest): string {
  const who = row.requester_name ?? row.requester_email ?? `#${row.requested_by ?? row.created_by ?? ''}`;
  return row.origin === 'inventory'
    ? t('pages.admin.connectorRequests.inventoryOrigin', 'Connectors in use (created by {{who}})', { who })
    : who;
}

function scope(row: ConnectorRequest): string {
  return row.platform_wide
    ? t('pages.admin.connectorRequests.platformWide', 'Every project')
    : row.project_ids.join(', ');
}

interface Pending {
  readonly row: ConnectorRequest;
  readonly approve: boolean;
}

export function AdminConnectorRequests(): ReactNode {
  const [state, setState] = useState<ConnectorRequestState | ''>('submitted');
  const [pending, setPending] = useState<Pending>();
  const list = useAdminConnectorRequests(state);
  const rows = list.data ?? [];

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem' }}>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.connectorRequests.title', 'Connector requests')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.connectorRequests.subtitle',
          'With the connector catalogue set to approved only, a toolkit type or MCP server is usable once a request for it is approved here. Approving grants the type and adds the egress hosts.',
        )}
      </Typography>

      <InventoryAction />

      <TextField
        select
        size="small"
        label={t('pages.admin.connectorRequests.filter.state', 'State')}
        value={state}
        onChange={(event) => setState(event.target.value as ConnectorRequestState | '')}
        sx={filterSx}
        slotProps={{ htmlInput: { 'data-testid': 'connector-requests-state' } }}
      >
        {STATES.map((value) => (
          <MenuItem key={value === '' ? 'all' : value} value={value}>{stateLabel(value)}</MenuItem>
        ))}
      </TextField>

      {list.isFetching && <LinearProgress data-testid="connector-requests-loading" />}
      {list.isError && (
        <Alert severity="error">{t('pages.admin.connectorRequests.loadError', 'Could not read the connector requests.')}</Alert>
      )}

      <Table size="small" data-testid="connector-requests-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.connectorRequests.column.connector', 'Connector')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.requester', 'Requester')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.projects', 'Projects')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.tools', 'Tools')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.egress', 'Egress hosts')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.dataClasses', 'Data classes')}</TableCell>
            <TableCell>{t('pages.admin.connectorRequests.column.state', 'State')}</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.length === 0 && !list.isLoading && (
            <TableRow>
              <TableCell colSpan={8}>{t('pages.admin.connectorRequests.empty', 'No connector request in this state.')}</TableCell>
            </TableRow>
          )}
          {rows.map((row) => (
            <RequestRow key={row.id} row={row} onDecide={(approve) => setPending({ row, approve })} />
          ))}
        </TableBody>
      </Table>

      {pending !== undefined && <DecisionDialog pending={pending} onClose={() => setPending(undefined)} />}
    </DrawerPage>
  );
}

/**
 * The on-ramp to approved_only: how many connectors in use no approved
 * request covers, and the button that creates a submitted request for each
 * one without an open or approved request.
 */
function InventoryAction(): ReactNode {
  const inventory = useConnectorInventory();
  const create = useCreateInventoryRequests();
  const missing = inventory.data?.without_approval;

  return (
    <Box sx={inventorySx} data-testid="connector-inventory">
      <Box sx={inventoryRowSx}>
        <Button
          size="small"
          variant="contained"
          disabled={create.isPending || missing === 0}
          onClick={() => create.mutate()}
          data-testid="connector-inventory-create"
        >
          {t('pages.admin.connectorRequests.inventory.create', 'Create requests for connectors in use')}
        </Button>
        {missing !== undefined && (
          <Typography variant="bodySmall" color="text.secondary" data-testid="connector-inventory-count">
            {t(
              'pages.admin.connectorRequests.inventory.count',
              '{{missing}} of {{inUse}} connectors in use have no approved request.',
              { missing, inUse: inventory.data?.in_use ?? 0 },
            )}
          </Typography>
        )}
      </Box>
      {create.isSuccess && (
        <Alert severity="success" onClose={() => create.reset()} data-testid="connector-inventory-created">
          {t(
            'pages.admin.connectorRequests.inventory.created',
            'Created {{created}} request(s); {{skipped}} connector(s) already had an open or approved request. Another connector approver must decide them.',
            { created: create.data.created, skipped: create.data.skipped },
          )}
        </Alert>
      )}
      {create.isError && <Alert severity="error">{describeDecisionError(create.error)}</Alert>}
    </Box>
  );
}

/** Approve (reason optional) or reject (reason required) one request. */
function DecisionDialog({ pending, onClose }: { readonly pending: Pending; readonly onClose: () => void }): ReactNode {
  const [reason, setReason] = useState('');
  const decide = useDecideConnectorRequest();
  const { row, approve } = pending;
  const confirm = (): void =>
    decide.mutate({ id: row.id, approve, reason: reason.trim() }, { onSuccess: onClose });

  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>
        {approve
          ? t('pages.admin.connectorRequests.approveTitle', 'Approve connector request #{{id}}?', { id: row.id })
          : t('pages.admin.connectorRequests.rejectTitle', 'Reject connector request #{{id}}?', { id: row.id })}
      </DialogTitle>
      <DialogContent>
        <TextField
          fullWidth
          multiline
          minRows={2}
          margin="normal"
          required={!approve}
          label={t('pages.admin.connectorRequests.reason', 'Reason')}
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          slotProps={{ htmlInput: { maxLength: 1000, 'data-testid': 'connector-request-reason' } }}
        />
        {decide.isError && <Alert severity="error">{describeDecisionError(decide.error)}</Alert>}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('common.cancel', 'Cancel')}</Button>
        <Button
          variant="contained"
          color={approve ? 'primary' : 'error'}
          disabled={decide.isPending || (!approve && reason.trim() === '')}
          onClick={confirm}
          data-testid="connector-request-confirm"
        >
          {approve ? t('pages.admin.connectorRequests.approve', 'Approve') : t('pages.admin.connectorRequests.reject', 'Reject')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function RequestRow({ row, onDecide }: { readonly row: ConnectorRequest; readonly onDecide: (approve: boolean) => void }): ReactNode {
  return (
    <TableRow data-testid={`connector-request-row-${row.id}`} sx={rowSx}>
      <TableCell>
        <Typography variant="bodyMedium" sx={wrapSx}>{row.target}</Typography>
        <Typography variant="bodySmall" color="text.secondary">
          {row.target_kind} #{row.id}
        </Typography>
        {row.justification !== '' && (
          <Typography variant="bodySmall" color="text.secondary" sx={wrapSx}>{row.justification}</Typography>
        )}
      </TableCell>
      <TableCell>{requester(row)}</TableCell>
      <TableCell>{scope(row)}</TableCell>
      <TableCell>
        <Box sx={chipsSx}>
          {row.tools.map((tool) => (
            <Chip key={tool.name} size="small" label={`${tool.name} · ${tool.category}`} />
          ))}
        </Box>
      </TableCell>
      <TableCell sx={wrapSx}>{row.egress_hosts.length === 0 ? '—' : row.egress_hosts.join(', ')}</TableCell>
      <TableCell>{row.data_classes.length === 0 ? '—' : row.data_classes.join(', ')}</TableCell>
      <TableCell>
        {row.state}
        {row.decision_reason !== '' && (
          <Typography variant="bodySmall" color="text.secondary" sx={wrapSx}>{row.decision_reason}</Typography>
        )}
      </TableCell>
      <TableCell>
        {row.state === 'submitted' && (
          <Box sx={actionsSx}>
            <Button size="small" variant="contained" onClick={() => onDecide(true)} data-testid={`connector-request-approve-${row.id}`}>
              {t('pages.admin.connectorRequests.approve', 'Approve')}
            </Button>
            <Button size="small" color="error" onClick={() => onDecide(false)} data-testid={`connector-request-reject-${row.id}`}>
              {t('pages.admin.connectorRequests.reject', 'Reject')}
            </Button>
          </Box>
        )}
      </TableCell>
    </TableRow>
  );
}

const titleSx: SxProps<Theme> = { marginBottom: '0.25rem' };
const subtitleSx: SxProps<Theme> = { marginBottom: '1rem' };
const filterSx: SxProps<Theme> = { minWidth: '12rem', marginBottom: '1rem' };
const rowSx: SxProps<Theme> = { verticalAlign: 'top' };
const wrapSx: SxProps<Theme> = { overflowWrap: 'anywhere' };
const chipsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem' };
const actionsSx: SxProps<Theme> = { display: 'flex', gap: '0.5rem' };
const inventorySx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem', marginBottom: '1rem' };
const inventoryRowSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '0.75rem' };
