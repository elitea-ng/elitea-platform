/**
 * Admin › Projects › a row's "Send to cloud" dialog (ADR-0034 decision 11,
 * owner decision 2026-10-19): one project's cloud executor on/off, for any
 * project, whether or not the operator is a member of it.
 *
 * Reads and writes `/admin/project_cloud_executor/administration/{id}`
 * (gated on `projects.projects.projects.view` / `.edit`). The control shows
 * the effective state and whether it is the project's own choice or the
 * platform's default for projects; "Use the default for projects" removes
 * the project's choice. While the platform switch is off "On" is not
 * offered, but off and the default can still be set ahead of time.
 *
 * `canEdit` follows the page's other project writes (presentation only; the
 * server refuses a write without the permission).
 */
import Button from '@mui/material/Button';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import Typography from '@mui/material/Typography';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { useCallback } from 'react';

import { CloudExecutorSwitchControl, describeCloudExecutorSwitchError } from '@/features/cloud-executor-switch';
import {
  getGetAdminProjectCloudExecutorQueryKey,
  setAdminProjectCloudExecutor,
  useGetAdminProjectCloudExecutor,
} from '@/shared/api/generated/admin/admin';
import type { ProjectCloudExecutorSwitch } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import type { AdminProjectRow } from './api/adminProjectsApi';

export interface AdminProjectCloudExecutorDialogProps {
  /** The project to show, or `null` when the dialog is closed. */
  readonly project: AdminProjectRow | null;
  readonly canEdit: boolean;
  readonly onClose: () => void;
}

export function AdminProjectCloudExecutorDialog({ project, canEdit, onClose }: AdminProjectCloudExecutorDialogProps) {
  return (
    <Dialog open={project !== null} onClose={onClose} maxWidth="sm" fullWidth>
      <DialogTitle>{t('features.cloudExecutorSwitch.admin.title', 'Send to cloud (cloud executor)')}</DialogTitle>
      <DialogContent>
        {/* Keyed per project: a second project never shows the first one's save error. */}
        {project ? <ProjectSwitch key={project.id} project={project} canEdit={canEdit} /> : null}
      </DialogContent>
      <DialogActions>
        <Button variant="elitea" color="secondary" onClick={onClose}>
          {t('features.cloudExecutorSwitch.admin.close', 'Close')}
        </Button>
      </DialogActions>
    </Dialog>
  );
}

function ProjectSwitch({ project, canEdit }: { readonly project: AdminProjectRow; readonly canEdit: boolean }) {
  const projectId = String(project.id);
  const queryClient = useQueryClient();
  const query = useGetAdminProjectCloudExecutor(projectId);
  const state = query.data?.data as ProjectCloudExecutorSwitch | undefined;
  const mutation = useMutation({
    mutationFn: (enabled: boolean | null) => setAdminProjectCloudExecutor(projectId, { enabled }),
    onSuccess: (response) => {
      queryClient.setQueryData(getGetAdminProjectCloudExecutorQueryKey(projectId), response);
    },
  });
  const { mutate } = mutation;
  const onChange = useCallback((enabled: boolean | null) => mutate(enabled), [mutate]);
  const failure = mutation.error ?? query.error;

  return (
    <>
      <Typography variant="bodyMedium" color="text.secondary" sx={{ marginBottom: '0.75rem' }}>
        {project.name}
      </Typography>
      <CloudExecutorSwitchControl
        state={state}
        canEdit={canEdit}
        isSaving={mutation.isPending}
        errorMessage={failure ? describeCloudExecutorSwitchError(failure) : undefined}
        readOnlyHint={t(
          'features.cloudExecutorSwitch.admin.readOnly',
          'Changing it needs the permission to edit projects.',
        )}
        onChange={onChange}
      />
    </>
  );
}
