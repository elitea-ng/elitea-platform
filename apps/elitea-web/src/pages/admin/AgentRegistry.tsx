/**
 * Admin › Agent registry (ADR-0040 decision 1, #1497): every agent of every
 * project with its owners, risk tier and certification, filtered by tier,
 * certification state, owner and project, with CSV and JSON export for a GRC
 * tool or an auditor. Read-only: the record is edited from the agent's own
 * ⋮ › Governance dialog. Behind `configuration.governance`, like the server
 * route.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import LinearProgress from '@mui/material/LinearProgress';
import Table from '@mui/material/Table';
import TableBody from '@mui/material/TableBody';
import TableCell from '@mui/material/TableCell';
import TableHead from '@mui/material/TableHead';
import TableRow from '@mui/material/TableRow';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentRegistryRecord } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import { AgentRegistryFiltersBar } from './AgentRegistryFiltersBar';
import {
  EMPTY_REGISTRY_FILTERS,
  REGISTRY_PAGE_SIZE,
  downloadRegistryCsv,
  downloadRegistryJson,
  useAdminAgentRegistry,
  type AgentRegistryFilters,
} from './api/adminAgentRegistryApi';

function when(value: string | null): string {
  return value === null ? '—' : new Date(value).toLocaleDateString();
}

function owner(id: number | null): string {
  return id === null ? '—' : String(id);
}

export function AdminAgentRegistry(): ReactNode {
  const [filters, setFilters] = useState<AgentRegistryFilters>(EMPTY_REGISTRY_FILTERS);
  const [offset, setOffset] = useState(0);
  const [exportError, setExportError] = useState<string>();
  const [exporting, setExporting] = useState(false);
  const list = useAdminAgentRegistry(filters, offset);
  const rows = list.data?.rows ?? [];
  const total = list.data?.total ?? 0;

  const changeFilters = (next: AgentRegistryFilters): void => {
    setFilters(next);
    setOffset(0);
  };
  const runExport = (download: () => Promise<unknown>): void => {
    setExportError(undefined);
    setExporting(true);
    download()
      .catch(() => setExportError(t('pages.admin.agentRegistry.exportError', 'The export could not be downloaded.')))
      .finally(() => setExporting(false));
  };

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem' }}>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.agentRegistry.title', 'Agent registry')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.agentRegistry.subtitle',
          'Every agent across projects with its owners, risk tier and certification. Edit a record from the agent’s ⋮ › Governance.',
        )}
      </Typography>

      <AgentRegistryFiltersBar
        filters={filters}
        onChange={changeFilters}
        exporting={exporting}
        onExportCsv={() => runExport(() => downloadRegistryCsv(filters))}
        onExportJson={() =>
          runExport(async () => {
            const { truncated } = await downloadRegistryJson(filters);
            if (truncated) {
              setExportError(
                t('pages.admin.agentRegistry.truncated', 'The JSON export stopped at its row limit. Narrow the filters.'),
              );
            }
          })
        }
      />

      {list.isFetching && <LinearProgress data-testid="agent-registry-loading" />}
      {list.isError && (
        <Alert severity="error">{t('pages.admin.agentRegistry.loadError', 'Could not read the agent registry.')}</Alert>
      )}
      {exportError !== undefined && (
        <Alert severity="error" onClose={() => setExportError(undefined)}>{exportError}</Alert>
      )}

      <Table size="small" data-testid="agent-registry-table">
        <TableHead>
          <TableRow>
            <TableCell>{t('pages.admin.agentRegistry.column.project', 'Project')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.agent', 'Agent')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.tier', 'Risk tier')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.state', 'Certification')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.recertifyBy', 'Recertify by')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.owners', 'Business / technical owner')}</TableCell>
            <TableCell>{t('pages.admin.agentRegistry.column.dataClasses', 'Data classes')}</TableCell>
          </TableRow>
        </TableHead>
        <TableBody>
          {rows.length === 0 && !list.isLoading && (
            <TableRow>
              <TableCell colSpan={7}>
                {t('pages.admin.agentRegistry.empty', 'No agent matches these filters.')}
              </TableCell>
            </TableRow>
          )}
          {rows.map((row) => (
            <RegistryRow key={`${row.project_id}:${row.application_id}`} row={row} />
          ))}
        </TableBody>
      </Table>

      <Box sx={pagerSx}>
        <Typography variant="bodySmall" color="text.secondary" data-testid="agent-registry-count">
          {t('pages.admin.agentRegistry.count', '{{from}}-{{to}} of {{total}}', {
            from: total === 0 ? 0 : offset + 1,
            to: offset + rows.length,
            total,
          })}
        </Typography>
        <Button size="small" disabled={offset === 0} onClick={() => setOffset(Math.max(0, offset - REGISTRY_PAGE_SIZE))}>
          {t('pages.admin.agentRegistry.previous', 'Previous')}
        </Button>
        <Button
          size="small"
          disabled={offset + rows.length >= total}
          onClick={() => setOffset(offset + REGISTRY_PAGE_SIZE)}
        >
          {t('pages.admin.agentRegistry.next', 'Next')}
        </Button>
      </Box>
    </DrawerPage>
  );
}

function RegistryRow({ row }: { readonly row: AgentRegistryRecord }): ReactNode {
  return (
    <TableRow data-testid={`agent-registry-row-${row.project_id}-${row.application_id}`}>
      <TableCell>{row.project_id}</TableCell>
      <TableCell>
        {row.application_name}{' '}
        <Typography variant="bodySmall" component="span" color="text.secondary">
          #{row.application_id}
        </Typography>
      </TableCell>
      <TableCell>{row.risk_tier}</TableCell>
      <TableCell>{row.certification_state}</TableCell>
      <TableCell>{when(row.recertify_by)}</TableCell>
      <TableCell>
        {owner(row.business_owner_id)} / {owner(row.technical_owner_id)}
      </TableCell>
      <TableCell>{row.data_classes.length === 0 ? '—' : row.data_classes.join(', ')}</TableCell>
    </TableRow>
  );
}

const titleSx: SxProps<Theme> = { marginBottom: '0.25rem' };
const subtitleSx: SxProps<Theme> = { marginBottom: '1rem' };
const pagerSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: '0.5rem', paddingTop: '0.5rem' };
