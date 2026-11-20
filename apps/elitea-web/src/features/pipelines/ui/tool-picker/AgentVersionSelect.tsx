import { memo, useCallback, useMemo, type ReactNode } from 'react';
import { format } from 'date-fns';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { LATEST_VERSION_NAME } from '@/entities/version';
import { useGetApplication } from '@/shared/api/generated/applications/applications';
import { unwrapBody } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { SingleSelect, type SingleSelectOption } from '@/shared/ui/SingleSelect';

import { useSelectedProjectId } from '../../lib/flow-editor/hooks/useSelectedProjectId';
import type { PickerToolEntry } from '../../lib/tool-picker/toolPicker.types';
import { useIsVersionLocked } from '../../lib/tool-picker/useIsVersionLocked';

interface ApplicationVersionEntry {
  readonly id?: number | string | undefined;
  readonly name?: string | undefined;
  readonly created_at?: string | undefined;
  readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
}

interface ApplicationDetailBody {
  readonly versions?: readonly ApplicationVersionEntry[] | undefined;
}

export interface AgentVersionSelectProps {
  readonly tool?: PickerToolEntry | null | undefined;
  readonly tooltip?: ReactNode | undefined;
  readonly versionStatus?: string | undefined;
  readonly projectId?: string | undefined;
  readonly disabled?: boolean | undefined;
  readonly onVersionChange?: (
    applicationId: number,
    versionDetails: {
      readonly id: number;
      readonly icon_meta?: Readonly<Record<string, unknown>> | undefined;
    },
  ) => void;
  readonly 'data-testid'?: string | undefined;
}

function formatVersionLabel(version: ApplicationVersionEntry): string {
  if (version.name === LATEST_VERSION_NAME) return LATEST_VERSION_NAME;
  const name = version.name || t('pipelines.toolPicker.unnamedVersion', 'Unnamed version');
  const createdAt = version.created_at ? new Date(version.created_at) : null;
  return createdAt && !Number.isNaN(createdAt.getTime())
    ? `${name} – ${format(createdAt, 'dd.MM.yyyy')}`
    : name;
}

function sortValidVersions(
  versions: readonly ApplicationVersionEntry[] | undefined,
): ApplicationVersionEntry[] {
  return [...(versions ?? [])]
    .filter((version) => Number(version.id) > 0)
    .sort((a, b) => {
      if (a.name === LATEST_VERSION_NAME) return -1;
      if (b.name === LATEST_VERSION_NAME) return 1;
      return new Date(b.created_at ?? 0).getTime() - new Date(a.created_at ?? 0).getTime();
    });
}

const rootSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: theme.spacing(0.5),
  width: '100%',
});

function buildVersionOptions(entries: readonly ApplicationVersionEntry[]): SingleSelectOption[] {
  return entries.map((version) => ({
    value: String(Number(version.id)),
    label: formatVersionLabel(version),
  }));
}

function resolveMissingVersionError(
  versionId: number | undefined,
  options: readonly SingleSelectOption[],
  stringVersionId: string,
): string | undefined {
  const isMissing =
    Boolean(versionId) && options.length > 0 && !options.some((opt) => opt.value === stringVersionId);
  return isMissing
    ? t('pipelines.toolPicker.missingVersion', 'This version no longer exists. Select another one.')
    : undefined;
}

function resolveOptionalSelectProps(
  tooltip: ReactNode | undefined,
  versionLabel: string,
  missingVersionError: string | undefined,
): { readonly label?: string; readonly error?: string } {
  return {
    ...(tooltip ? {} : { label: versionLabel }),
    ...(missingVersionError ? { error: missingVersionError } : {}),
  };
}

function resolveVersionIds(tool: PickerToolEntry | null | undefined): {
  readonly applicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly stringVersionId: string;
} {
  const settings = tool?.settings;
  const applicationId = settings?.application_id;
  const versionId = settings?.application_version_id;
  return {
    applicationId,
    versionId,
    stringVersionId: versionId !== undefined ? String(versionId) : '',
  };
}

/** Version of the agent or pipeline an Agent node runs. Changing it is applied on Save. */
export const AgentVersionSelect = memo(function AgentVersionSelect({
  tool,
  tooltip,
  versionStatus,
  projectId: projectIdOverride,
  disabled = false,
  onVersionChange,
  'data-testid': testId,
}: AgentVersionSelectProps): ReactNode {
  const routeProjectId = useSelectedProjectId();
  const projectId = projectIdOverride ?? routeProjectId ?? '';
  const isVersionLocked = useIsVersionLocked(versionStatus);
  const { applicationId, versionId, stringVersionId } = resolveVersionIds(tool);

  const query = useGetApplication(projectId, applicationId ?? 0, {
    query: { enabled: Boolean(projectId && applicationId) },
  });

  const application = unwrapBody(query.data) as ApplicationDetailBody | undefined;
  const versionEntries = useMemo(() => sortValidVersions(application?.versions), [application?.versions]);
  const options = useMemo(() => buildVersionOptions(versionEntries), [versionEntries]);
  const missingVersionError = resolveMissingVersionError(versionId, options, stringVersionId);

  const handleChange = useCallback(
    (newVersionValue: string) => {
      if (!applicationId) return;
      const numericId = Number(newVersionValue);
      const matched = versionEntries.find((entry) => Number(entry.id) === numericId);
      if (matched) {
        onVersionChange?.(applicationId, { id: numericId, icon_meta: matched.icon_meta });
      }
    },
    [applicationId, onVersionChange, versionEntries],
  );

  const versionLabel = t('pipelines.toolPicker.versionLabel', 'Version');
  const selectDisabled = disabled || isVersionLocked || !applicationId || query.isFetching;
  const selectValue = options.some((opt) => opt.value === stringVersionId) ? stringVersionId : '';
  const optionalSelectProps = resolveOptionalSelectProps(tooltip, versionLabel, missingVersionError);

  return (
    <Box sx={rootSx} className="nopan nodrag nowheel" data-testid={testId}>
      {tooltip ? <InfoLabelWithTooltip label={versionLabel} tooltip={tooltip} /> : null}
      <SingleSelect
        value={selectValue}
        options={options}
        onChange={handleChange}
        disabled={selectDisabled}
        {...optionalSelectProps}
      />
    </Box>
  );
});
