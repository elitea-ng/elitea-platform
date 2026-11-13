import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { useSelectedProjectId } from '../lib/flow-editor/hooks/useSelectedProjectId';
import { usePermissionState } from '../lib/useHasPermission';

/**
 * A disabled editor must say so, and say the true reason: a published pipeline
 * is read-only for everyone, whatever they may edit; any other disabled editor
 * means the caller lacks the update permission. The permission answer is only
 * waited for in the second case, so a still-loading list does not flash it for
 * a user who can edit, and a published pipeline never waits on it.
 */
export function PipelineReadOnlyNotice({ disabled, isPublished }: { readonly disabled: boolean | undefined; readonly isPublished: boolean }): ReactNode {
  const projectId = useSelectedProjectId();
  const { settled } = usePermissionState(projectId, PERMISSIONS.applications.update);
  if (!disabled || (!isPublished && !settled)) return null;
  return (
    <Alert severity="info" data-testid="pipeline-read-only-notice" data-cause={isPublished ? 'published' : 'permission'} sx={{ mb: 1 }}>
      {isPublished
        ? t('features.pipelines.editorPanel.readOnlyPublished', 'This pipeline is published and cannot be edited.')
        : t('features.pipelines.editorPanel.readOnlyPermission', 'This pipeline is read-only: you do not have permission to update pipelines in this project.')}
    </Alert>
  );
}
