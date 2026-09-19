import type { ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import LinkIcon from '@mui/icons-material/Link';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { SxProps, Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import { ClockIcon } from '@/shared/ui/icons/clock-icon';

import { PipelineScheduleModal } from './PipelineScheduleModal';
import { PipelineWebhookModal } from './PipelineWebhookModal';
import type { TriggerListEntry } from './triggerTypeSelector.lib';
import { useTriggerSurface } from './useTriggerSurface';

export interface TriggerTypeSelectorProps {
  readonly disabled?: boolean | undefined;
  readonly projectId?: string;
  readonly versionId?: number;
  /** The SAVED version's YAML (`version_details.instructions`) -- see `computeHasInteractiveElements`'s own doc comment for why this is not the editor's live working copy. */
  readonly versionInstructions?: string;
  readonly onNotifySuccess?: (message: string) => void;
  readonly onNotifyError?: (message: string) => void;
}

const containerSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem', width: '100%', padding: '0.5rem 1rem', boxSizing: 'border-box' };
const selectWrapperSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const selectSx: SxProps<Theme> = { flex: 1, marginBottom: '0' };
const iconStyle: SxProps<Theme> = { width: '1rem', height: '1rem' };
const listSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.25rem' };
const rowTextSx: SxProps<Theme> = { flex: 1, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', color: 'text.secondary' };
function triggerButtonSx(theme: Theme) {
  return { padding: '0.25rem', color: theme.vars.palette.icon.fill.secondary, '&:hover': { color: theme.vars.palette.primary.main } };
}

interface TriggerActionButtonProps {
  readonly tooltip: string;
  readonly onClick: () => void;
  readonly disabled: boolean;
  readonly icon: ReactNode;
  readonly testId?: string;
}

function TriggerActionButton({ tooltip, onClick, disabled, icon, testId }: TriggerActionButtonProps): ReactNode {
  return (
    <Tooltip
      title={tooltip}
      placement="top"
    >
      <IconButton
        sx={triggerButtonSx}
        onClick={onClick}
        disabled={disabled}
        data-testid={testId}
        aria-label={tooltip}
      >
        {icon}
      </IconButton>
    </Tooltip>
  );
}

interface TriggerRowListProps {
  readonly entries: readonly TriggerListEntry[];
  readonly disabled: boolean;
  readonly onEdit: (kind: TriggerListEntry['kind']) => void;
  readonly onDelete: (kind: TriggerListEntry['kind']) => void;
}

/**
 * What this pipeline version actually has configured — one row per kind,
 * because the two Go facilities are independent and both can be on at once
 * (see {@link TriggerTypeSelector}'s own doc comment). Split out of that
 * component to keep it under the §3.5 complexity budget (12).
 */
function TriggerRowList({ entries, disabled, onEdit, onDelete }: TriggerRowListProps): ReactNode {
  if (entries.length === 0) return null;
  return (
    <Box
      sx={listSx}
      data-testid="pipeline-trigger-list"
    >
      {entries.map(entry => (
        <Box
          key={entry.kind}
          sx={rowSx}
          data-testid={`pipeline-trigger-row-${entry.kind}`}
        >
          <Typography
            variant="bodySmall"
            sx={rowTextSx}
          >
            {`${entry.label}: ${entry.detail}`}
          </Typography>
          <TriggerActionButton
            tooltip={entry.kind === 'schedule'
              ? t('pipelines.triggerTypeSelector.editSchedule', 'Edit schedule')
              : t('pipelines.triggerTypeSelector.editWebhook', 'Edit webhook settings')}
            onClick={() => onEdit(entry.kind)}
            disabled={disabled}
            testId={`pipeline-trigger-edit-${entry.kind}`}
            icon={entry.kind === 'schedule' ? <ClockIcon style={{ width: '1rem', height: '1rem' }} /> : <LinkIcon sx={iconStyle} />}
          />
          <TriggerActionButton
            tooltip={t('pipelines.triggerTypeSelector.deleteTrigger', 'Delete trigger')}
            onClick={() => onDelete(entry.kind)}
            disabled={disabled}
            testId={`pipeline-trigger-delete-${entry.kind}`}
            icon={<DeleteOutlineIcon sx={iconStyle} />}
          />
        </Box>
      ))}
    </Box>
  );
}

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/pipelines/flow-editor/ui/
 * settings/TriggerTypeSelector.jsx` (unit A2h), then REDESIGNED onto the Go
 * backend by #899.
 *
 * WHAT #899 CHANGED, AND WHY IT IS NOT THE BASELINE'S SHAPE. The baseline
 * wrote one `PipelineTrigger` row carrying a `type` discriminator
 * (chat_message | schedule | webhook) and a schedule/webhook jsonb blob, over
 * a pylon route that no longer exists. `internal/api/v2/pipelinetriggers`
 * serves two INDEPENDENT facilities instead -- `/pipeline_schedules` and
 * `/pipeline_triggers` -- with no trigger-type column at all and with both
 * able to exist on one pipeline version. So:
 *
 *  - the dropdown still offers Chat Message / Schedule / Webhook, because
 *    that is the question a person is answering, but it is now a CHOICE OF
 *    WHAT TO ADD rather than a stored field. Its displayed value is derived
 *    (`currentTriggerKind`), and "Chat Message" is the absence of both
 *    facilities -- which is exactly what it means to the user.
 *  - whatever is configured is LISTED beneath it, one row per kind, each
 *    with its own edit and delete action, because both can be on at once and
 *    a single-valued control cannot say so.
 *  - the Webhook modal shows the endpoint URL and the credential the backend
 *    hands back, with reveal/rotate/revoke -- there is no GitHub/GitLab/
 *    Custom signature mode in the Go inbound route (one bearer secret,
 *    constant-time compared), so those options are gone rather than wired to
 *    nothing.
 *
 * OTHER DEVIATIONS FROM BASELINE:
 *  1. `useFormikContext()` -> explicit `versionId`/`versionInstructions`
 *     props, falling back to `FlowEditorContext` (which the editor now
 *     carries them on) so every node component does not have to thread a
 *     `triggerProps` object it has no data for -- the #899 "zero callers
 *     ever passed triggerProps" gap.
 *  2. `useSelectedProject()` -> explicit `projectId` prop, same fallback.
 *  3. RTK Query -> `../../api/usePipelineTriggers.ts` over the generated
 *     client.
 *  4. `useToast()` -> `onNotifySuccess`/`onNotifyError` callback props.
 */
export function TriggerTypeSelector(props: TriggerTypeSelectorProps): ReactNode {
  const { disabled = false } = props;
  const surface = useTriggerSurface(props);
  const controlsDisabled = disabled || surface.isLoading;

  return (
    <Box sx={containerSx}>
      <InfoLabelWithTooltip
        label={t('pipelines.triggerTypeSelector.label', 'Trigger')}
        tooltip={surface.tooltip}
        variant="labelSmall"
        iconSize={14}
      />

      {/*
        `nopan nodrag` — React Flow's escape hatch, and it is the THIRD thing
        that made this surface unreachable (#899), independent of the
        capability flag and of the never-supplied scope. The canvas's drag
        layer handles the mouse-down on the node, so without it a click on
        this dropdown focuses the combobox and never opens its menu: measured
        on the journeys stack, the accessibility tree showed the combobox with
        no `listbox` anywhere. Keyboard (focus + Enter) still worked, which is
        why unit tests saw a working control and only an e2e click caught it.
        `SimpleLLMInputItem.tsx` records the same finding for its own pair of
        selects, including why the class sits on a WRAPPER (`hasSelector` in
        @xyflow/system walks UP from the event target, and `SingleSelect` is
        shared and already at the §3.5 12-prop budget).
      */}
      <Box
        className="nopan nodrag"
        sx={selectWrapperSx}
      >
        <SingleSelect
          sx={selectSx}
          value={surface.currentTriggerType}
          onChange={value => {
            void surface.actions.handleTriggerTypeChange(value);
          }}
          options={surface.options}
          disabled={controlsDisabled}
        />
      </Box>

      <TriggerRowList
        entries={surface.entries}
        disabled={controlsDisabled}
        onEdit={kind => (kind === 'schedule' ? surface.openScheduleModal() : surface.openWebhookModal())}
        onDelete={kind => {
          void surface.actions.handleDeleteKind(kind);
        }}
      />

      <PipelineScheduleModal
        open={surface.isScheduleModalOpen}
        onClose={surface.closeScheduleModal}
        onSubmit={cronExpression => {
          void surface.actions.handleScheduleSubmit(cronExpression);
        }}
        cron={surface.cron}
        isLoading={surface.isUpdating}
      />

      <PipelineWebhookModal
        open={surface.isWebhookModalOpen}
        onClose={surface.closeWebhookModal}
        webhookUrl={surface.webhookUrl}
        secretValue={surface.revealedSecret}
        isLoading={surface.isUpdating}
        onReveal={() => {
          void surface.actions.handleRevealWebhook();
        }}
        onRotate={() => {
          void surface.actions.handleRotateWebhook();
        }}
        onRevoke={() => {
          void surface.actions.handleDeleteKind('webhook');
        }}
        onNotify={props.onNotifySuccess}
      />
    </Box>
  );
}
