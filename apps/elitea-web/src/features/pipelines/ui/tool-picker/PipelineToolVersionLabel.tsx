import { memo, useMemo, type ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { useGetApplication } from '@/shared/api/generated/applications/applications';
import { unwrapBody } from '@/shared/api/unwrap';

import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import type { PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';

interface ApplicationVersionsBody {
  readonly versions?: readonly { readonly id?: number | string | undefined; readonly name?: string | undefined }[] | undefined;
}

export interface PipelineToolVersionLabelProps {
  readonly tool: PickerToolEntry;
  readonly projectId?: string | undefined;
}

/** Name of the agent or pipeline version a pipeline runs. */
export const PipelineToolVersionLabel = memo(function PipelineToolVersionLabel({
  tool,
  projectId: projectIdOverride,
}: PipelineToolVersionLabelProps): ReactNode {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId ?? '';
  const applicationId = tool.settings?.application_id;
  const versionId = tool.settings?.application_version_id;

  const query = useGetApplication(projectId, applicationId ?? 0, {
    query: { enabled: Boolean(projectId) && Boolean(applicationId) },
  });

  const application = unwrapBody(query.data) as ApplicationVersionsBody | undefined;

  const versionName = useMemo(
    () => application?.versions?.find((version) => Number(version.id) === Number(versionId))?.name,
    [application?.versions, versionId],
  );

  if (!versionName) return null;

  return (
    <Typography
      variant="bodySmall"
      color="text.primary"
      noWrap
      data-testid="pipeline-tool-card-version"
    >
      {versionName}
    </Typography>
  );
});
