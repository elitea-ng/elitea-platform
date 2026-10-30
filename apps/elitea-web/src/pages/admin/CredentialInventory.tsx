/**
 * Admin › Credential inventory (ADR-0044 decision 5, #1501): every credential
 * an agent can use — personal access tokens, toolkit secrets, MCP OAuth
 * tokens, provider OAuth grants, ADR-0039 credential references and use
 * grants — with owner, project, scope, dates, last use, the agents that use
 * it and the stale flag of the Credential Review policy. Never a secret
 * value. Revoke is offered for the two kinds the platform issues (personal
 * access tokens and use grants) and is audited; the others are issued
 * elsewhere and are flagged only. Behind `credentials.inventory.read`, like
 * the server route; Revoke needs `credentials.inventory.manage`.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Chip from '@mui/material/Chip';
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

import type { CredentialInventoryCredential, CredentialKind } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import {
  EMPTY_INVENTORY_FILTERS,
  INVENTORY_PAGE_SIZE,
  useCredentialInventory,
  type InventoryFilters,
} from './api/adminCredentialReviewApi';
import { CredentialRevokeDialog } from './CredentialRevokeDialog';

const KINDS: readonly CredentialKind[] = [
  'pat',
  'toolkit_secret',
  'mcp_oauth_token',
  'provider_oauth_grant',
  'credential_reference',
  'credential_grant',
];

function credentialKindLabel(kind: CredentialKind): string {
  switch (kind) {
    case 'pat':
      return t('pages.admin.credentialInventory.kind.pat', 'Personal access token');
    case 'toolkit_secret':
      return t('pages.admin.credentialInventory.kind.toolkitSecret', 'Toolkit secret');
    case 'mcp_oauth_token':
      return t('pages.admin.credentialInventory.kind.mcpOauthToken', 'MCP OAuth token');
    case 'provider_oauth_grant':
      return t('pages.admin.credentialInventory.kind.providerOauthGrant', 'Provider OAuth grant');
    case 'credential_reference':
      return t('pages.admin.credentialInventory.kind.credentialReference', 'Credential reference');
    case 'credential_grant':
      return t('pages.admin.credentialInventory.kind.credentialGrant', 'Credential use grant');
  }
}

function when(value: string | null | undefined): string {
  return value === null || value === undefined ? '—' : new Date(value).toLocaleDateString();
}

/** The owning agent in the main app; the project is chosen there. */
function agentHref(projectId: number, applicationId: number): string {
  return `/agents/all/${applicationId}?project_id=${projectId}`;
}

export function AdminCredentialInventory(): ReactNode {
  const [filters, setFilters] = useState<InventoryFilters>(EMPTY_INVENTORY_FILTERS);
  const [offset, setOffset] = useState(0);
  const [revoking, setRevoking] = useState<CredentialInventoryCredential>();
  const list = useCredentialInventory(filters, offset);
  const rows = list.data?.rows ?? [];
  const total = list.data?.total ?? 0;

  const changeFilters = (next: InventoryFilters): void => {
    setFilters(next);
    setOffset(0);
  };

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem' }}>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.credentialInventory.title', 'Credential inventory')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {list.data === undefined
          ? t(
              'pages.admin.credentialInventory.subtitleLoading',
              'Every credential an agent can use, with its owner, last use and the agents that use it.',
            )
          : t(
              'pages.admin.credentialInventory.subtitle',
              'Every credential an agent can use, with its owner, last use and the agents that use it. Stale: unused for {{days}} days (policy: {{action}}).',
              {
                days: list.data.stale_after_days,
                action: list.data.stale_action,
              },
            )}
      </Typography>

      <Box sx={toolbarSx} data-testid="credential-inventory-filters">
        <TextField
          select
          size="small"
          label={t('pages.admin.credentialInventory.filter.kind', 'Kind')}
          value={filters.kind}
          onChange={(event) =>
            changeFilters({
              ...filters,
              kind: event.target.value as CredentialKind | '',
            })
          }
          sx={selectSx}
          slotProps={{
            htmlInput: { 'data-testid': 'credential-inventory-filter-kind' },
          }}
        >
          <MenuItem value="">{t('pages.admin.credentialInventory.any', 'Any')}</MenuItem>
          {KINDS.map((kind) => (
            <MenuItem key={kind} value={kind}>
              {credentialKindLabel(kind)}
            </MenuItem>
          ))}
        </TextField>
        <TextField
          size="small"
          label={t('pages.admin.credentialInventory.filter.project', 'Project id')}
          value={filters.projectId}
          onChange={(event) => changeFilters({ ...filters, projectId: event.target.value })}
          slotProps={{
            htmlInput: {
              inputMode: 'numeric',
              'data-testid': 'credential-inventory-filter-project',
            },
          }}
          sx={idSx}
        />
        <TextField
          select
          size="small"
          label={t('pages.admin.credentialInventory.filter.stale', 'Stale')}
          value={filters.stale}
          onChange={(event) =>
            changeFilters({
              ...filters,
              stale: event.target.value as InventoryFilters['stale'],
            })
          }
          sx={selectSx}
          slotProps={{
            htmlInput: { 'data-testid': 'credential-inventory-filter-stale' },
          }}
        >
          <MenuItem value="any">{t('pages.admin.credentialInventory.any', 'Any')}</MenuItem>
          <MenuItem value="stale">{t('pages.admin.credentialInventory.staleOnly', 'Stale only')}</MenuItem>
          <MenuItem value="fresh">{t('pages.admin.credentialInventory.freshOnly', 'In use')}</MenuItem>
        </TextField>
      </Box>

      {list.isFetching && <LinearProgress data-testid="credential-inventory-loading" />}
      {list.isError && (
        <Alert severity="error">
          {t('pages.admin.credentialInventory.loadError', 'Could not read the credential inventory.')}
        </Alert>
      )}

      <Table size="small" data-testid="credential-inventory-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.credentialInventory.column.kind', 'Kind')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.name', 'Credential')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.owner', 'Owner')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.project', 'Project')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.scope', 'Scope')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.created', 'Created')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.expires', 'Expires')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.lastUsed', 'Last used')}</TableCell>
            <TableCell>{t('pages.admin.credentialInventory.column.agents', 'Agents')}</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.length === 0 && !list.isLoading && (
            <TableRow>
              <TableCell colSpan={10}>
                {t('pages.admin.credentialInventory.empty', 'No credential matches these filters.')}
              </TableCell>
            </TableRow>
          )}
          {rows.map((row) => (
            <InventoryRow key={`${row.kind}:${row.credential_id}`} row={row} onRevoke={setRevoking} />
          ))}
        </TableBody>
      </Table>

      <Box sx={pagerSx}>
        <Typography variant="bodySmall" color="text.secondary" data-testid="credential-inventory-count">
          {t('pages.admin.credentialInventory.count', '{{from}}-{{to}} of {{total}}', {
            from: total === 0 ? 0 : offset + 1,
            to: offset + rows.length,
            total,
          })}
        </Typography>
        <Button
          size="small"
          disabled={offset === 0}
          onClick={() => setOffset(Math.max(0, offset - INVENTORY_PAGE_SIZE))}
        >
          {t('pages.admin.credentialInventory.previous', 'Previous')}
        </Button>
        <Button
          size="small"
          disabled={offset + rows.length >= total}
          onClick={() => setOffset(offset + INVENTORY_PAGE_SIZE)}
        >
          {t('pages.admin.credentialInventory.next', 'Next')}
        </Button>
      </Box>
      <CredentialRevokeDialog row={revoking} onClose={() => setRevoking(undefined)} />
    </DrawerPage>
  );
}

function InventoryRow({
  row,
  onRevoke,
}: {
  readonly row: CredentialInventoryCredential;
  readonly onRevoke: (row: CredentialInventoryCredential) => void;
}): ReactNode {
  return (
    <TableRow data-testid={`credential-inventory-row-${row.kind}-${row.credential_id}`}>
      <TableCell>{credentialKindLabel(row.kind)}</TableCell>
      <TableCell>
        {row.name}
        {row.stale && (
          <Chip
            size="small"
            color="warning"
            label={t('pages.admin.credentialInventory.stale', 'Stale')}
            sx={chipSx}
            data-testid="credential-inventory-stale"
          />
        )}
        {row.state !== '' && row.state !== 'active' && (
          <Typography variant="bodySmall" component="div" color="text.secondary">
            {row.state}
          </Typography>
        )}
      </TableCell>
      <TableCell>{row.owner_id ?? '—'}</TableCell>
      <TableCell>{row.project_id ?? '—'}</TableCell>
      <TableCell>{row.scope === '' ? '—' : row.scope}</TableCell>
      <TableCell>{when(row.created_at)}</TableCell>
      <TableCell>{when(row.expires_at)}</TableCell>
      <TableCell>{when(row.last_used_at)}</TableCell>
      <TableCell>
        {row.agents.length === 0
          ? '—'
          : row.agents.map((agent, index) => (
              <span key={`${agent.project_id}:${agent.application_id}`}>
                {index > 0 && ', '}
                <a href={agentHref(agent.project_id, agent.application_id)}>{agent.name}</a>
              </span>
            ))}
      </TableCell>
      <TableCell align="right">
        {row.revocable ? (
          <Button
            size="small"
            color="error"
            onClick={() => onRevoke(row)}
            data-testid={`credential-inventory-revoke-${row.kind}-${row.credential_id}`}
          >
            {t('pages.admin.credentialInventory.revoke', 'Revoke')}
          </Button>
        ) : (
          <Typography variant="bodySmall" color="text.secondary">
            {t('pages.admin.credentialInventory.flagOnly', 'Flag only')}
          </Typography>
        )}
      </TableCell>
    </TableRow>
  );
}

const titleSx: SxProps<Theme> = { marginBottom: '0.25rem' };
const subtitleSx: SxProps<Theme> = { marginBottom: '1rem' };
const toolbarSx: SxProps<Theme> = {
  display: 'flex',
  flexWrap: 'wrap',
  gap: '0.75rem',
  marginBottom: '0.75rem',
};
const selectSx: SxProps<Theme> = { minWidth: '12rem' };
const idSx: SxProps<Theme> = { width: '9rem' };
const chipSx: SxProps<Theme> = { marginLeft: '0.5rem' };
const pagerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-end',
  gap: '0.5rem',
  paddingTop: '0.5rem',
};
