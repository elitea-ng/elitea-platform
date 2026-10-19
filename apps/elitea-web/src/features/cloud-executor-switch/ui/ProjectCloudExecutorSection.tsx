/**
 * Settings › General › "Send to cloud (cloud executor)": the project's own
 * switch, for its project admin (ADR-0034 decision 11).
 *
 * Reads and writes `/elitea_core/project_cloud_executor/prompt_lib/{id}`:
 * any member reads it, `models.project_settings.edit` writes it, which the
 * page passes in as `canEdit` (presentation only; the server gates the
 * write). While the platform has the cloud executor off the whole control is
 * disabled and says why.
 */
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { memo, useCallback } from 'react';

import type { ProjectCloudExecutorSwitch } from '@/shared/api/generated/model';
import {
  getGetProjectCloudExecutorQueryKey,
  setProjectCloudExecutor,
  useGetProjectCloudExecutor,
} from '@/shared/api/generated/settings/settings';
import { t } from '@/shared/i18n';

import { CloudExecutorSwitchControl } from './CloudExecutorSwitchControl';
import { describeCloudExecutorSwitchError } from './describeCloudExecutorSwitchError';

export interface ProjectCloudExecutorSectionProps {
  readonly projectId: string;
  readonly canEdit: boolean;
}

export const ProjectCloudExecutorSection = memo(function ProjectCloudExecutorSection({
  projectId,
  canEdit,
}: ProjectCloudExecutorSectionProps) {
  const queryClient = useQueryClient();
  const query = useGetProjectCloudExecutor(projectId, { query: { enabled: projectId !== '' } });
  const state = query.data?.data as ProjectCloudExecutorSwitch | undefined;
  const mutation = useMutation({
    mutationFn: (enabled: boolean | null) => setProjectCloudExecutor(projectId, { enabled }),
    onSuccess: (response) => {
      queryClient.setQueryData(getGetProjectCloudExecutorQueryKey(projectId), response);
    },
  });
  const { mutate } = mutation;
  const onChange = useCallback((enabled: boolean | null) => mutate(enabled), [mutate]);

  const failure = mutation.error ?? query.error;
  return (
    <CloudExecutorSwitchControl
      state={state}
      canEdit={canEdit}
      isSaving={mutation.isPending}
      errorMessage={failure ? describeCloudExecutorSwitchError(failure) : undefined}
      lockWhenPlatformOff
      readOnlyHint={t('features.cloudExecutorSwitch.readOnly.project', 'Only a project admin can change this.')}
      onChange={onChange}
    />
  );
});
