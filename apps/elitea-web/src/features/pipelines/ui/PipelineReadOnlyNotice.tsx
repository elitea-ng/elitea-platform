import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { useSelectedProjectId } from '../lib/flow-editor/hooks/useSelectedProjectId';
import { usePermissionState } from '../lib/useHasPermission';

/**
 * A disabled editor must say so: without this, the YAML view just looks greyed
 * out. Shown only once the permission list has answered, so a still-loading
 * list does not flash it for a user who can edit.
 */
export function PipelineReadOnlyNotice({ disabled }: { readonly disabled: boolean | undefined }): ReactNode {
  const projectId = useSelectedProjectId();
  const { settled } = usePermissionState(projectId, PERMISSIONS.applications.update);
  if (!disabled || !settled) return null;
  return (
    <Alert severity="info" data-testid="pipeline-read-only-notice" sx={{ mb: 1 }}>
      {t(
        'features.pipelines.editorPanel.readOnly',
        'This pipeline is read-only. Editing needs permission to update pipelines in this project, and published pipelines cannot be edited.',
      )}
    </Alert>
  );
}
