/**
 * Admin › Permission trimming (ADR-0044 decision 5, #1501): across projects,
 * every agent's selected tools, attached toolkits and credential use grants,
 * and the project and platform egress allowlist entries, against their use
 * in the step records and egress summaries of the window. An unused item is
 * a removal candidate; Remove performs it through the item's own mutation
 * path and audits it. The weekly `main.permission_trimming` routine notifies
 * agent owners of new candidates. Behind `credentials.inventory.read`, like
 * the server route; Remove needs `credentials.inventory.manage`.
 */
import { useState, type ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { credentialReviewErrorMessage, PermissionTrimmingTable, trimmingItemKey } from '@/features/agent-lifecycle';
import type { PermissionTrimmingItem } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseCheckbox } from '@/shared/ui/BaseCheckbox';
import { DrawerPage } from '@/shared/ui/settings/DrawerPage';

import {
  EMPTY_TRIMMING_FILTERS,
  useAdminPermissionTrimming,
  useRemoveAdminTrimmingItem,
  type AdminTrimmingFilters,
} from './api/adminCredentialReviewApi';

function agentHref(item: PermissionTrimmingItem): string | undefined {
  return item.application_id > 0 ? `/agents/all/${item.application_id}?project_id=${item.project_id}` : undefined;
}

export function AdminPermissionTrimming(): ReactNode {
  const [filters, setFilters] = useState<AdminTrimmingFilters>(EMPTY_TRIMMING_FILTERS);
  const [removingKey, setRemovingKey] = useState<string>();
  const report = useAdminPermissionTrimming(filters);
  const remove = useRemoveAdminTrimmingItem();

  const onRemove = (item: PermissionTrimmingItem): void => {
    setRemovingKey(trimmingItemKey(item));
    remove.mutate(
      {
        project_id: item.project_id,
        application_id: item.application_id,
        kind: item.kind,
        key: item.key,
      },
      { onSettled: () => setRemovingKey(undefined) },
    );
  };

  return (
    <DrawerPage sx={{ padding: '1rem 1.5rem' }}>
      <Typography variant="headingLarge" component="h1" sx={titleSx}>
        {t('pages.admin.permissionTrimming.title', 'Permission trimming')}
      </Typography>
      <Typography variant="bodyMedium" color="text.secondary" sx={subtitleSx}>
        {t(
          'pages.admin.permissionTrimming.subtitle',
          'Granted tools, toolkits, credential grants and egress hosts that agents did not use in the window. Removing one is audited.',
        )}
      </Typography>

      <Box sx={toolbarSx} data-testid="permission-trimming-filters">
        <TextField
          size="small"
          label={t('pages.admin.permissionTrimming.filter.project', 'Project id')}
          value={filters.projectId}
          onChange={(event) => setFilters({ ...filters, projectId: event.target.value })}
          slotProps={{
            htmlInput: {
              inputMode: 'numeric',
              'data-testid': 'permission-trimming-filter-project',
            },
          }}
          sx={idSx}
        />
        <TextField
          size="small"
          label={t('pages.admin.permissionTrimming.filter.window', 'Window (days)')}
          placeholder={String(report.data?.window_days ?? 30)}
          value={filters.windowDays}
          onChange={(event) => setFilters({ ...filters, windowDays: event.target.value })}
          slotProps={{
            htmlInput: {
              inputMode: 'numeric',
              'data-testid': 'permission-trimming-filter-window',
            },
          }}
          sx={idSx}
        />
        <FormControlLabel
          control={
            <BaseCheckbox
              size="small"
              checked={filters.candidatesOnly}
              onChange={(event) => setFilters({ ...filters, candidatesOnly: event.target.checked })}
            />
          }
          label={t('pages.admin.permissionTrimming.filter.candidatesOnly', 'Removal candidates only')}
        />
      </Box>

      {report.isFetching && <LinearProgress data-testid="permission-trimming-loading" />}
      {report.isError && (
        <Alert severity="error">
          {t('pages.admin.permissionTrimming.loadError', 'Could not read the permission trimming report.')}
        </Alert>
      )}
      {remove.error !== null && (
        <Alert severity="error" onClose={() => remove.reset()}>
          {credentialReviewErrorMessage(remove.error)}
        </Alert>
      )}

      {report.data !== undefined && (
        <PermissionTrimmingTable
          items={report.data.items}
          showAgent
          removingKey={removingKey}
          onRemove={onRemove}
          agentHref={agentHref}
        />
      )}
    </DrawerPage>
  );
}

const titleSx: SxProps<Theme> = { marginBottom: '0.25rem' };
const subtitleSx: SxProps<Theme> = { marginBottom: '1rem' };
const toolbarSx: SxProps<Theme> = {
  display: 'flex',
  flexWrap: 'wrap',
  alignItems: 'center',
  gap: '0.75rem',
  marginBottom: '0.75rem',
};
const idSx: SxProps<Theme> = { width: '9rem' };
