import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';

import { useUpdateIndexScheduleMutation } from '../../api/indexesApi';
import { toDisplayString } from '../../lib/helpers/displayString.local';
import { convertToolkitSchema } from '../../lib/helpers/toolkitSchemaConversion.local';
import type { ToolkitTypeSchema } from '../../lib/helpers/toolkitSchemaConversion.local';
import {
  EditViewTabsEnum,
  IndexCronDefault,
  IndexStatuses,
  IndexViewsEnum,
  IndexesToolsEnum,
  PERMISSIONS,
  RUNNABLE_INDEX_STATUSES,
} from '../../lib/constants/indexDetails.constants';
import { useSelectedProjectId } from '../../lib/hooks/useSelectedProjectId';
import { useIndexesStore } from '../../model/indexesStore';
import type { IndexRow, ScheduleEntry } from '../../model/indexesStore';
import type { CredentialsFieldDescriptor, CredentialsSelectSlotProps } from './IndexScheduleModal';
import { IndexScheduleModal } from './IndexScheduleModal';
import type { IndexConfigSaveActions } from './IndexActionsParts';
import { CreateModeActions, EditModeActions, IndexingInProgressActions } from './IndexActionsParts';

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/ui/
 * IndexDetails/IndexActions.jsx` (unit A4a) — the header action bar
 * (Cancel/Index, Reindex/Delete, and the schedule-enable switch + gear).
 *
 * DISCLOSED DI, three independent gaps, same class of decision as
 * `IndexScheduleModal.tsx`'s and `IndexConfig.tsx`'s own DI (all cite the
 * identical constraint: the real primitive is either layer-forbidden or
 * not yet landed):
 *  - `useGetCurrentToolkitSchemas` (`features/toolkits/lib/hooks`, an A4b
 *    sibling within the same slice, not yet landed) -> injected
 *    `useToolkitSchemas` hook prop.
 *  - `currentUserId`/`userPermissions`/`currentProjectName`
 *    (`useSelector(state => state.user)` / `state.settings.project.name` in
 *    the baseline) -> caller-supplied props. This app has no session/
 *    project store yet anywhere (the identical gap
 *    `features/credentials/ui/CredentialsSelect.tsx`'s own doc comment
 *    already discloses: "There is no session/project store yet anywhere in
 *    this app ... caller-supplied instead of read from Redux").
 *  - `convertToolkitSchema` (baseline: `common/toolkitSchemaUtils.js`, a
 *    ~140-line pure function with zero external dependencies, used only to
 *    locate the toolkit type's `credentials`-section schema property) is
 *    ported in full below as a local, non-exported helper — see its own
 *    comment for why a local copy rather than an injected/imported one.
 */

export interface UseToolkitSchemasResult {
  readonly toolkitSchemas: Record<string, ToolkitTypeSchema> | undefined;
  readonly isFetching: boolean;
}

export interface EditToolDetail {
  readonly type?: string;
  readonly schema?: ToolkitTypeSchema;
}

export interface IndexActionsProps {
  readonly activeView: string;
  readonly index: IndexRow | null | undefined;
  readonly view: string;
  readonly toolkitId: string;
  readonly onDiscard: () => void;
  readonly isValidForm?: boolean | undefined;
  readonly indexData: () => void;
  readonly isIndexingData?: boolean | undefined;
  readonly isRunningTool?: boolean | undefined;
  readonly handleDeleteIndex: () => void;
  readonly isIndexDeleting?: boolean | undefined;
  readonly selectedIndexTools: readonly string[];
  readonly onCancelIndexing: () => void;
  readonly isStoppingIndexing?: boolean | undefined;
  readonly editToolDetail?: EditToolDetail | null | undefined;
  readonly useToolkitSchemas: (params: { isMCP: boolean }) => UseToolkitSchemasResult;
  readonly currentUserId?: string | number | undefined;
  readonly userPermissions?: readonly string[] | undefined;
  readonly currentProjectName?: string | undefined;
  readonly isPrivateProject?: boolean | undefined;
  readonly renderCredentialsSelect?: ((props: CredentialsSelectSlotProps) => ReactNode) | undefined;
  /** The Save / Save & Reindex pair (ELITEA-2880…2887). Absent when the caller has no configuration form to save — then only "Reindex" is offered, which is the pre-split behaviour. */
  readonly configSave?: IndexConfigSaveActions | undefined;
}

/**
 * `scheduleData`'s resolution logic, extracted to a standalone (module-
 * level, not nested-in-`useMemo`) function for the same complexity-budget
 * reason as the sub-components above — oxlint's `complexity` check counts
 * branches inside a `useMemo`/`useCallback` callback toward the ENCLOSING
 * component function, not the callback's own scope (confirmed empirically:
 * `IndexActions`'s reported complexity only dropped from 25 to 14 after the
 * JSX sub-components were extracted, not further just from these callbacks
 * already being separate arrow-function expressions). Byte-identical logic
 * to the baseline inline version.
 */
function resolveScheduleData(toolkitScheduler: Readonly<Record<string, ScheduleEntry>>, indexName: string, currentUserId: string | number | undefined): ScheduleEntry {
  const entryByUser = toolkitScheduler[indexName];
  const schedules = entryByUser?.['schedules'] as Record<string | number, ScheduleEntry> | undefined;
  // No `?? entryByUser` fallback here — matches the baseline exactly
  // (`IndexActions.jsx:71-75`): only the per-user and `-1`-default schedules
  // are real candidates; anything else falls straight to the hardcoded
  // default below. The raw `entryByUser` bucket has shape `{schedules:
  // {...}}`, not a `ScheduleEntry` — treating it as one when neither
  // per-user nor `-1` exists would read `.cron`/`.enabled`/`.credentials`
  // off a value that never carries them, silently diverging from the
  // baseline's always-safe default in that case.
  const schedule = schedules?.[currentUserId ?? -1] ?? schedules?.[-1];
  return schedule ?? { cron: IndexCronDefault, enabled: false, credentials: null };
}

/** `credentialsData`'s resolution logic — same extraction reason as `resolveScheduleData` above. */
function resolveCredentialsData(toolkitSchema: ToolkitTypeSchema): CredentialsFieldDescriptor | null {
  const entry = Object.entries(toolkitSchema.properties ?? {}).find(([, v]) => v.section?.includes('credentials'));
  if (entry === undefined) return null;
  // The KEY travels with the property: the served property carries no
  // `description`, so the modal derives its picker label from the key instead
  // (#308 — see `resolveCredentialsLabel` in `IndexScheduleModal.tsx`).
  return { ...(entry[1] as CredentialsFieldDescriptor), propertyKey: entry[0] };
}

/** `schedulingTooltipMessage`'s computation — same extraction reason as `resolveScheduleData` above (this one is the single largest branch contributor). */
interface SchedulingTooltipMessageParams {
  readonly userPermissions: readonly string[] | undefined;
  readonly currentProjectName: string | undefined;
  readonly scheduleEnabled: boolean | undefined;
  readonly isReindexDisabled: boolean;
  readonly indexState: unknown;
  readonly scheduleCredentials: unknown;
  readonly credentialsData: CredentialsFieldDescriptor | null;
}

function computeSchedulingTooltipMessage(params: SchedulingTooltipMessageParams): string | null {
  const { userPermissions, currentProjectName, scheduleEnabled, isReindexDisabled, indexState, scheduleCredentials, credentialsData } = params;
  const noPermissions = Array.isArray(userPermissions) && !userPermissions.includes(PERMISSIONS.index.schedule);
  if (noPermissions) return `Insufficient permissions to perform this action on ${currentProjectName ?? ''} project`;
  if (scheduleEnabled) return null;
  if (isReindexDisabled) return 'Go to "Configuration" tab to configure scheduling';
  if (!RUNNABLE_INDEX_STATUSES.includes(String(indexState)) && indexState !== IndexStatuses.progress) {
    return 'Index state is not valid';
  }
  if (!scheduleCredentials && credentialsData) return 'Set credentials to enable scheduling';
  return null;
}

/**
 * Every derived flag `IndexActions` reads from `index`/`view`/
 * `selectedHistoryItem`/etc, extracted to a standalone function for the
 * same complexity-budget reason as the rest of this file's extractions
 * (each optional-chain access and `||`/`&&`/`??` inside this function's OWN
 * scope no longer counts toward `IndexActions`'s complexity total).
 */
interface IndexActionsDerivedFlags {
  readonly indexName: string;
  readonly indexCouldBeStopped: boolean;
  readonly progressInvalidIndex: boolean;
  readonly isEditMode: boolean;
  readonly isActionsDisabled: boolean;
  readonly isRemovingDisabled: boolean;
  readonly isReindexDisabled: boolean;
}

function deriveIndexActionsFlags(params: {
  readonly index: IndexRow | null | undefined;
  readonly view: string;
  readonly activeView: string;
  readonly selectedHistoryItem: unknown;
  readonly selectedIndexTools: readonly string[];
  readonly isRunningTool: boolean | undefined;
  readonly isIndexDeleting: boolean | undefined;
}): IndexActionsDerivedFlags {
  const { index, view, activeView, selectedHistoryItem, selectedIndexTools, isRunningTool, isIndexDeleting } = params;
  const metadata = index?.metadata;
  return {
    indexName: toDisplayString(metadata?.['collection']),
    indexCouldBeStopped: Boolean(metadata?.['task_id']),
    progressInvalidIndex: Boolean(index?.['stale']) && metadata?.['state'] === IndexStatuses.progress,
    isEditMode: view === IndexViewsEnum.edit,
    isActionsDisabled: Boolean(isRunningTool) || Boolean(isIndexDeleting),
    isRemovingDisabled: !selectedIndexTools.includes(IndexesToolsEnum.removeIndex),
    isReindexDisabled: Boolean(selectedHistoryItem) || activeView === EditViewTabsEnum.run,
  };
}

export function IndexActions(props: IndexActionsProps): ReactNode {
  const {
    activeView,
    index,
    view,
    toolkitId,
    onDiscard,
    isValidForm,
    indexData,
    isIndexingData,
    isRunningTool,
    handleDeleteIndex,
    isIndexDeleting,
    selectedIndexTools,
    onCancelIndexing,
    isStoppingIndexing,
    editToolDetail,
    useToolkitSchemas,
    currentUserId,
    userPermissions,
    currentProjectName,
    isPrivateProject,
    renderCredentialsSelect,
    configSave,
  } = props;

  const projectId = useSelectedProjectId();
  const toolkitScheduler = useIndexesStore((state) => state.toolkitScheduler);
  const selectedHistoryItem = useIndexesStore((state) => state.selectedHistoryItem);

  const { indexName, indexCouldBeStopped, progressInvalidIndex, isEditMode, isActionsDisabled, isRemovingDisabled, isReindexDisabled } =
    deriveIndexActionsFlags({ index, view, activeView, selectedHistoryItem, selectedIndexTools, isRunningTool, isIndexDeleting });

  const updateIndexScheduleMutation = useUpdateIndexScheduleMutation();

  const [scheduleModal, setScheduleModal] = useState(false);

  const scheduleData: ScheduleEntry = useMemo(
    () => resolveScheduleData(toolkitScheduler, indexName, currentUserId),
    [toolkitScheduler, indexName, currentUserId],
  );

  const { toolkitSchemas, isFetching: toolkitSchemaFetching } = useToolkitSchemas({ isMCP: false });

  const toolkitType = editToolDetail?.type ?? '';

  const toolkitSchema = useMemo(
    () => editToolDetail?.schema ?? convertToolkitSchema(toolkitSchemas?.[toolkitType]),
    [editToolDetail?.schema, toolkitSchemas, toolkitType],
  );

  const credentialsData: CredentialsFieldDescriptor | null = useMemo(() => resolveCredentialsData(toolkitSchema), [toolkitSchema]);

  const schedulingTooltipMessage = useMemo(
    () =>
      computeSchedulingTooltipMessage({
        userPermissions,
        currentProjectName,
        scheduleEnabled: scheduleData.enabled,
        isReindexDisabled,
        indexState: index?.metadata['state'],
        scheduleCredentials: scheduleData.credentials,
        credentialsData,
      }),
    [userPermissions, scheduleData.enabled, scheduleData.credentials, isReindexDisabled, index, credentialsData, currentProjectName],
  );

  const scheduleConfigMessage = schedulingTooltipMessage === 'Set credentials to enable scheduling' ? null : schedulingTooltipMessage;

  const handleChangeIndexSchedule = useCallback(
    async (data: Partial<ScheduleEntry>) => {
      // elitea_issues: #6547 — `data` is `{...scheduleData, ...}` and
      // `scheduleData` is the backend's STORED schedule record, which
      // carries whatever timezone was captured on a previous save. The
      // timezone sent with THIS update must always be the current user's
      // (at the moment of the update), so it is spread last: a stale
      // `data.timezone` must never win over the freshly resolved one.
      const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone;
      if (projectId === undefined) return;
      try {
        await updateIndexScheduleMutation.mutateAsync({ projectId, toolkitId, indexName, ...data, timezone });
      } catch {
        // The baseline surfaces this via `useToast` (`toastSuccess`/`toastError`).
        // No toast/snackbar primitive exists yet in `shared/ui` (see `features/
        // mcps/model/useMcpAuthModal.ts`'s own doc comment for the identical,
        // already-disclosed platform gap) — the mutation's own error state
        // (`updateIndexSchedule`'s returned tuple) is available to a caller
        // that wants to surface it once a toast primitive lands.
      }
    },
    [updateIndexScheduleMutation, projectId, toolkitId, indexName],
  );

  const onScheduleModalSubmit = useCallback(
    (cronExpression: string, credentials: unknown) => void handleChangeIndexSchedule({ ...scheduleData, cron: cronExpression, credentials }),
    [handleChangeIndexSchedule, scheduleData],
  );

  if (isIndexingData) {
    return (
      <IndexingInProgressActions
        indexCouldBeStopped={indexCouldBeStopped}
        progressInvalidIndex={progressInvalidIndex}
        onCancelIndexing={onCancelIndexing}
        isStoppingIndexing={Boolean(isStoppingIndexing)}
        isActionsDisabled={isActionsDisabled}
        isRemovingDisabled={isRemovingDisabled}
        onDelete={handleDeleteIndex}
      />
    );
  }

  return (
    <>
      <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', gap: '.75rem' }}>
        {isEditMode ? (
          <EditModeActions
            indexName={indexName}
            scheduleData={scheduleData}
            schedulingTooltipMessage={schedulingTooltipMessage}
            scheduleConfigMessage={scheduleConfigMessage}
            onToggleSchedule={() => void handleChangeIndexSchedule({ ...scheduleData, enabled: !scheduleData.enabled })}
            onOpenScheduleModal={() => setScheduleModal(true)}
            isReindexDisabled={isReindexDisabled}
            isActionsDisabled={isActionsDisabled}
            isRemovingDisabled={isRemovingDisabled}
            onIndexData={indexData}
            onDelete={handleDeleteIndex}
            configSave={configSave}
          />
        ) : (
          <CreateModeActions
            onDiscard={onDiscard}
            isRunningTool={Boolean(isRunningTool)}
            isValidForm={Boolean(isValidForm)}
            onIndexData={indexData}
          />
        )}
      </Box>
      <IndexScheduleModal
        cron={scheduleData.cron}
        credentials={scheduleData.credentials}
        open={scheduleModal}
        onClose={() => setScheduleModal(false)}
        onSubmit={onScheduleModalSubmit}
        credentialsData={credentialsData}
        toolkitSchemaFetching={toolkitSchemaFetching}
        isPrivateProject={isPrivateProject}
        renderCredentialsSelect={renderCredentialsSelect}
      />
    </>
  );
}
