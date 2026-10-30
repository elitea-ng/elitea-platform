import type { ReactNode } from 'react';
import { useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { PermissionTrimmingItem } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { credentialReviewErrorMessage, useAgentPermissionTrimming, useRemoveAgentPermission } from '../api/trimmingApi';
import { PermissionTrimmingTable, trimmingItemKey } from './PermissionTrimmingTable';

/**
 * The Permission trimming tab (ADR-0044 decision 5, #1501): what this agent
 * is granted that its runs did not use in the window. Each candidate has a
 * one-click Remove, audited server-side; the weekly
 * `main.permission_trimming` routine notifies the agent's owners of new
 * candidates. Used items are listed too, so the reader sees the whole grant.
 */
export interface GovernanceTrimmingTabProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
}

export function GovernanceTrimmingTab({ projectId, applicationId }: GovernanceTrimmingTabProps): ReactNode {
  const report = useAgentPermissionTrimming(projectId, applicationId, true);
  const remove = useRemoveAgentPermission(projectId);
  const [removingKey, setRemovingKey] = useState<string>();
  if (projectId === undefined || applicationId === undefined) {
    return (
      <Typography variant="bodyMedium">
        {t('features.agentLifecycle.governance.notSaved', 'Save the agent first; its registry record starts with it.')}
      </Typography>
    );
  }
  if (report.isPending) return <CircularProgress size="1.5rem" />;
  if (report.isError) return <Alert severity="error">{credentialReviewErrorMessage(report.error)}</Alert>;
  const onRemove = (item: PermissionTrimmingItem): void => {
    setRemovingKey(trimmingItemKey(item));
    remove.mutate(
      { application_id: item.application_id, kind: item.kind, key: item.key },
      { onSettled: () => setRemovingKey(undefined) },
    );
  };
  return (
    <Box sx={stackSx} data-testid="governance-trimming">
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'features.agentLifecycle.trimming.intro',
          'Permissions granted to this agent against their use in the last {{days}} days. An unused one is a removal candidate; removing it edits the draft version.',
          { days: report.data.window_days },
        )}
      </Typography>
      {remove.error !== null && <Alert severity="error">{credentialReviewErrorMessage(remove.error)}</Alert>}
      <PermissionTrimmingTable
        items={report.data.items}
        showAgent={false}
        removingKey={removingKey}
        onRemove={onRemove}
      />
    </Box>
  );
}

const stackSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
};
