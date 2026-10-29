import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentCertificationState, AgentRiskTier } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { AgentRegistryFilters } from './api/adminAgentRegistryApi';

const TIERS: readonly AgentRiskTier[] = ['low', 'medium', 'high', 'critical'];
const STATES: readonly AgentCertificationState[] = ['uncertified', 'certified', 'due', 'lapsed', 'suspended'];

/** The Agent registry page's filters and its two export buttons. */
export interface AgentRegistryFiltersBarProps {
  readonly filters: AgentRegistryFilters;
  readonly onChange: (next: AgentRegistryFilters) => void;
  readonly exporting: boolean;
  readonly onExportCsv: () => void;
  readonly onExportJson: () => void;
}

export function AgentRegistryFiltersBar({
  filters,
  onChange,
  exporting,
  onExportCsv,
  onExportJson,
}: AgentRegistryFiltersBarProps): ReactNode {
  const any = t('pages.admin.agentRegistry.any', 'Any');
  return (
    <Box sx={toolbarSx} data-testid="agent-registry-filters">
      <TextField
        select
        size="small"
        label={t('pages.admin.agentRegistry.filter.tier', 'Risk tier')}
        value={filters.tier}
        onChange={(event) => onChange({ ...filters, tier: event.target.value as AgentRiskTier | '' })}
        sx={selectSx}
        slotProps={{ htmlInput: { 'data-testid': 'agent-registry-filter-tier' } }}
      >
        <MenuItem value="">{any}</MenuItem>
        {TIERS.map((tier) => (
          <MenuItem key={tier} value={tier}>
            {tier}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        select
        size="small"
        label={t('pages.admin.agentRegistry.filter.state', 'Certification')}
        value={filters.state}
        onChange={(event) => onChange({ ...filters, state: event.target.value as AgentCertificationState | '' })}
        sx={selectSx}
      >
        <MenuItem value="">{any}</MenuItem>
        {STATES.map((state) => (
          <MenuItem key={state} value={state}>
            {state}
          </MenuItem>
        ))}
      </TextField>
      <TextField
        size="small"
        label={t('pages.admin.agentRegistry.filter.owner', 'Owner (user id)')}
        value={filters.ownerId}
        onChange={(event) => onChange({ ...filters, ownerId: event.target.value })}
        slotProps={{ htmlInput: { inputMode: 'numeric', 'data-testid': 'agent-registry-filter-owner' } }}
        sx={idSx}
      />
      <TextField
        size="small"
        label={t('pages.admin.agentRegistry.filter.project', 'Project id')}
        value={filters.projectId}
        onChange={(event) => onChange({ ...filters, projectId: event.target.value })}
        slotProps={{ htmlInput: { inputMode: 'numeric', 'data-testid': 'agent-registry-filter-project' } }}
        sx={idSx}
      />
      <Box sx={{ flex: 1 }} />
      <Button variant="outlined" disabled={exporting} onClick={onExportCsv} data-testid="agent-registry-export-csv">
        {t('pages.admin.agentRegistry.exportCsv', 'Export CSV')}
      </Button>
      <Button variant="outlined" disabled={exporting} onClick={onExportJson} data-testid="agent-registry-export-json">
        {t('pages.admin.agentRegistry.exportJson', 'Export JSON')}
      </Button>
    </Box>
  );
}

const toolbarSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.75rem', marginBottom: '1rem' };
const selectSx: SxProps<Theme> = { minWidth: '10rem' };
const idSx: SxProps<Theme> = { width: '10rem' };
