import { type ReactNode, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useGetApplication, useGetPipelineInboundTrigger, useGetPipelineSchedule } from '@/shared/api/generated/applications/applications';
import type { ApplicationDetail } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { CopyToClipboardButton } from '@/shared/ui/CopyToClipboardButton';

import { useSelectedProjectId } from '../api/useSelectedProjectId';

import { StyledShowContextModal } from './StyledShowContextModal';

const TRIGGER_TYPE_LABELS: Readonly<Record<string, string>> = {
  chat_message: 'Chat Message',
  schedule: 'Schedule',
  webhook: 'Webhook',
};

const WEBHOOK_TYPE_LABELS: Readonly<Record<string, string>> = {
  github: 'GitHub',
  gitlab: 'GitLab',
  custom: 'Custom',
};

const LAST_RUN_FORMAT_OPTIONS: Readonly<Intl.DateTimeFormatOptions> = { dateStyle: 'short', timeStyle: 'short' };

/**
 * `entities/pipeline`'s `PipelineTrigger.schedule` is a confirmed opaque
 * jsonb passthrough; `cron`/`timezone`/`last_run`/`webhook_type` (baseline:
 * `ApplicationInformation.jsx:100-134`) live inside it — read defensively,
 * same `isRecord`+`typeof` guard `useAppDetail.ts`'s `readMetaString` uses.
 */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null;
}

function readScheduleString(schedule: unknown, key: string): string | undefined {
  if (!isRecord(schedule)) return undefined;
  const value = schedule[key];
  return typeof value === 'string' && value !== '' ? value : undefined;
}

/** Pulled out of JSX so the `dateStyle`/`timeStyle` literals aren't re-scanned every render by `i18next/no-literal-string`'s `jsx-only` mode (which inspects string literals inside any JSX-embedded expression, not just JSX attributes/text). */
function formatLastRun(lastRun: string, timezone: string | undefined): string {
  return new Intl.DateTimeFormat(undefined, { ...LAST_RUN_FORMAT_OPTIONS, timeZone: timezone }).format(new Date(lastRun));
}

interface ScheduleTriggerDetails {
  readonly cron: string | undefined;
  readonly timezone: string | undefined;
  readonly lastRun: string | undefined;
  readonly webhookType: string | undefined;
}

function useScheduleTriggerDetails(schedule: unknown): ScheduleTriggerDetails {
  return useMemo(
    () => ({
      cron: readScheduleString(schedule, 'cron'),
      timezone: readScheduleString(schedule, 'timezone'),
      lastRun: readScheduleString(schedule, 'last_run'),
      webhookType: readScheduleString(schedule, 'webhook_type'),
    }),
    [schedule],
  );
}

interface ForkedApplicationParams {
  readonly isForked: boolean;
  readonly forkedProjectId: string | undefined;
  readonly forkedApplicationId: string | undefined;
}
interface ForkedApplicationName {
  readonly name: string | undefined;
  /** The query's raw `EliteaApiError`, so `ForkedFromRow` can tell a 403 apart from "still-loading". */
  readonly error: unknown;
}

/** Duck-typed against `EliteaApiError`, same pattern `useSharepointCheckConnection.hooks.ts`'s `isEliteaApiErrorLike` establishes. */
function isForbiddenError(error: unknown): boolean {
  if (typeof error !== 'object' || error === null || !('failure' in error)) return false;
  return (error as { readonly failure?: { readonly status?: number } }).failure?.status === 403;
}

/** Split out of `ApplicationInformation` purely to keep its own complexity under the §3.5 budget (12) — the `enabled` guard's three-way `&&` was itself a meaningful chunk of that function's branch count. */
function useForkedApplicationName({ isForked, forkedProjectId, forkedApplicationId }: ForkedApplicationParams): ForkedApplicationName {
  const forkQuery = useGetApplication(forkedProjectId ?? '', Number(forkedApplicationId ?? 0), {
    query: { enabled: isForked && forkedProjectId !== undefined && forkedApplicationId !== undefined },
  });
  return {
    name: (forkQuery.data?.data as ApplicationDetail | undefined)?.name,
    error: forkQuery.error,
  };
}

interface PipelineTriggerParams {
  readonly isPipeline: boolean;
  readonly projectId: string | undefined;
  readonly versionId: string | undefined;
}

/**
 * Split out for the same reason as `useForkedApplicationName` above.
 *
 * NOTE(#899): was the hand-written client for pylon's deleted
 * `/elitea_core/pipeline_trigger/...` route, which answered one row carrying a
 * `type` discriminator and a schedule/webhook blob. The Go backend has no
 * trigger-type column — it serves a cron schedule and an inbound trigger
 * INDEPENDENTLY — so the type shown here is derived the same way the editor's
 * own selector derives it, and the `schedule` blob is assembled from the
 * schedule read rather than passed through. `timezone` and `webhook_type` are
 * absent because nothing stores them any more: the schedule fires on the
 * platform's clock, and the inbound route verifies ONE bearer credential
 * rather than a per-provider signature header.
 */
function usePipelineTriggerType({ isPipeline, projectId, versionId }: PipelineTriggerParams): {
  readonly type: string | null | undefined;
  readonly schedule: unknown;
} {
  const enabled = isPipeline && projectId !== undefined && versionId !== undefined;
  const project = Number(projectId ?? 0);
  const version = Number(versionId ?? 0);
  const scheduleQuery = useGetPipelineSchedule(project, version, { query: { enabled, retry: false } });
  const triggerQuery = useGetPipelineInboundTrigger(project, version, { query: { enabled, retry: false } });
  const schedule = scheduleQuery.data?.status === 200 ? scheduleQuery.data.data : undefined;
  const webhook = triggerQuery.data?.status === 200 ? triggerQuery.data.data : undefined;
  if (schedule?.configured === true) {
    return { type: 'schedule', schedule: { cron: schedule.cron, last_run: schedule.last_run } };
  }
  if (webhook?.configured === true && webhook.revoked_at === undefined) {
    return { type: 'webhook', schedule: {} };
  }
  return { type: undefined, schedule: undefined };
}

/** The `type === 'schedule'`-only rows, split out to keep `PipelineTriggerRows` under the §3.5 complexity budget (12). */
function ScheduleRows({ schedule }: { readonly schedule: ScheduleTriggerDetails }): ReactNode {
  return (
    <>
      {schedule.cron !== undefined && (
        <CopyToClipboardButton
          label={t('agents.applicationInformation.scheduleLabel', 'Schedule:')}
          value={schedule.cron}
          tooltip={t('agents.applicationInformation.copyCronTooltip', 'Copy cron expression')}
        />
      )}
      {schedule.timezone !== undefined && (
        <Box sx={pipelineLinkSx}>
          <Typography variant="bodyMedium">{t('agents.applicationInformation.timezoneLabel', 'Timezone:')}</Typography>
          <Typography variant="bodyMedium">{schedule.timezone}</Typography>
        </Box>
      )}
      {schedule.lastRun !== undefined && (
        <Box sx={pipelineLinkSx}>
          <Typography variant="bodyMedium">{t('agents.applicationInformation.lastRunLabel', 'Last run:')}</Typography>
          <Typography variant="bodyMedium">{formatLastRun(schedule.lastRun, schedule.timezone)}</Typography>
        </Box>
      )}
    </>
  );
}

interface PipelineTriggerRowsProps {
  readonly triggerType: string | null | undefined;
  readonly schedule: ScheduleTriggerDetails;
}

/** The trigger-type/schedule/webhook-type rows — split out of `ApplicationInformation` purely to stay under the §3.5 complexity budget (12); no behaviour change from having them inline. */
function PipelineTriggerRows({ triggerType, schedule }: PipelineTriggerRowsProps): ReactNode {
  if (triggerType === undefined || triggerType === null) return null;
  return (
    <>
      <Box sx={pipelineLinkSx}>
        <Typography variant="bodyMedium">{t('agents.applicationInformation.triggerLabel', 'Trigger:')}</Typography>
        <Typography variant="bodyMedium">{TRIGGER_TYPE_LABELS[triggerType] ?? triggerType}</Typography>
      </Box>
      {triggerType === 'schedule' && <ScheduleRows schedule={schedule} />}
      {triggerType === 'webhook' && schedule.webhookType !== undefined && (
        <Box sx={pipelineLinkSx}>
          <Typography variant="bodyMedium">{t('agents.applicationInformation.webhookTypeLabel', 'Webhook type:')}</Typography>
          <Typography variant="bodyMedium">{WEBHOOK_TYPE_LABELS[schedule.webhookType] ?? schedule.webhookType}</Typography>
        </Box>
      )}
    </>
  );
}

interface ForkedFromRowProps {
  readonly originalApplicationName: string | undefined;
  readonly error: unknown;
}

/** Baseline (`ApplicationInformation.jsx:138-149`): `LabelLinkWithToolTip`, `disabled={error?.status === 403}` — ported as a plain `Tooltip` + dimmed `Typography` (see module doc comment for the dropped `href`). */
function ForkedFromRow({ originalApplicationName, error }: ForkedFromRowProps): ReactNode {
  const isForbidden = isForbiddenError(error);
  return (
    <Box sx={pipelineLinkSx}>
      <Typography variant="bodyMedium">{t('agents.applicationInformation.forkedFromLabel', 'Forked from:')}</Typography>
      <Tooltip
        title={
          isForbidden
            ? t('agents.applicationInformation.forkedFromForbiddenTooltip', 'You do not have permission to see the original agent')
            : t('agents.applicationInformation.forkedFromTooltip', 'Go to original agent')
        }
      >
        <Typography
          variant="bodyMedium"
          sx={isForbidden ? forkedFromDisabledSx : undefined}
          aria-disabled={isForbidden}
        >
          {originalApplicationName ?? t('agents.applicationInformation.originalAgentFallback', 'Original agent')}
        </Typography>
      </Tooltip>
    </Box>
  );
}

function PipelineShowRow({ onShow }: { readonly onShow: () => void }): ReactNode {
  return (
    <Box sx={pipelineLinkSx}>
      <Typography variant="bodyMedium">{t('agents.applicationInformation.pipelineLabel', 'Pipeline:')}</Typography>
      <Typography
        sx={showLinkSx}
        variant="bodyMedium"
        onClick={onShow}
      >
        {t('agents.applicationInformation.showLink', 'Show')}
      </Typography>
    </Box>
  );
}

interface ApplicationInformationRowsProps {
  readonly idLabel: string;
  readonly id: string | undefined;
  readonly versionId: string | undefined;
  readonly isPipeline: boolean;
  readonly triggerType: string | null | undefined;
  readonly schedule: ScheduleTriggerDetails;
  readonly isForked: boolean;
  readonly originalApplicationName: string | undefined;
  readonly forkedApplicationError: unknown;
  readonly showPipeline: boolean;
  readonly onShowPipeline: () => void;
}

/** The whole "Information" accordion content — split out of `ApplicationInformation` purely to stay under the §3.5 complexity budget (12); that function keeps only data-fetching/state, this one only renders. */
function ApplicationInformationRows({
  idLabel,
  id,
  versionId,
  isPipeline,
  triggerType,
  schedule,
  isForked,
  originalApplicationName,
  forkedApplicationError,
  showPipeline,
  onShowPipeline,
}: ApplicationInformationRowsProps): ReactNode {
  return (
    <Box sx={contentContainerSx}>
      <CopyToClipboardButton
        label={idLabel}
        value={id ?? ''}
        tooltip={t('agents.applicationInformation.copyIdTooltip', 'Copy ID')}
        data-testid="copy-id"
      />
      {versionId !== undefined && (
        <CopyToClipboardButton
          label={t('agents.applicationInformation.versionIdLabel', 'Version ID:')}
          value={versionId}
          tooltip={t('agents.applicationInformation.copyVersionIdTooltip', 'Copy version ID')}
        />
      )}
      {isPipeline && (
        <PipelineTriggerRows triggerType={triggerType} schedule={schedule} />
      )}
      {isForked && <ForkedFromRow originalApplicationName={originalApplicationName} error={forkedApplicationError} />}
      {showPipeline && <PipelineShowRow onShow={onShowPipeline} />}
    </Box>
  );
}

/** @public */
export interface ApplicationInformationProps {
  id: string | undefined;
  versionId: string | undefined;
  isPipeline: boolean;
  isForked?: boolean;
  forkedProjectId?: string | undefined;
  forkedApplicationId?: string | undefined;
  pipelineInstructions?: string;
  showPipeline?: boolean;
  style?: SxProps<Theme>;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/
 * Applications/ApplicationInformation.jsx`.
 *
 * DISCLOSED DEVIATIONS:
 *  - No ambient form context — every value the baseline read off
 *    `useFormikContext().values` is now an explicit prop (this app has no
 *    Formik dependency; see `../model/types.ts`'s module doc comment,
 *    followed by every sibling `features/agents/ui/*` component).
 *  - "Forked from" renders as non-navigable text (no `href`) but keeps the
 *    baseline's permission-aware tooltip/disabled-state (`LabelLinkWithToolTip`'s
 *    `disabled={error?.status === 403}`) via `useForkedApplicationName`'s
 *    surfaced query error + `ForkedFromRow`'s dim/`aria-disabled`/tooltip-copy
 *    swap. Only `href` is dropped: baseline's `buildForkedEntityHref` builds
 *    an old-app-router URL (`common/utils.jsx:997-1012`) this app's real
 *    router does not use verbatim, and guessing the still-in-flux
 *    cross-project URL shape would invent a route contract, not port one.
 *  - Uses the real, already-landed `StyledShowContextModal`
 *    (`./StyledShowContextModal.tsx`, a sibling A1 sub-unit's port of the
 *    baseline's own modal) for the "Show pipeline" view instead of a
 *    hand-rolled one — that component already discloses its own real
 *    mermaid-rendering gap (no `mermaid` package anywhere in this app).
 *  - `useSelectedProject()`/`useForkedFromApplicationDetailsQuery` replaced
 *    with this slice's own `useSelectedProjectId` + the real generated
 *    `useGetApplication`.
 *  - Split into `ApplicationInformationRows`/`PipelineTriggerRows`/
 *    `ScheduleRows`/`ForkedFromRow`/`PipelineShowRow` purely to stay under
 *    the §3.5 cyclomatic-complexity budget (12) — the baseline's single
 *    render function branches on 7+ independent conditions, same "split to
 *    shrink complexity, not to change behaviour" move `InputBase.tsx`/
 *    `BasicAccordion.tsx` already document for this codebase.
 *  - Wraps content in `BasicAccordion` (`data-testid="agent-information-section"`,
 *    title `'Information'`), matching baseline `ApplicationInformation.jsx:191-199`;
 *    `style` now targets `slotSx.root` instead of the inner content box.
 */
export function ApplicationInformation({
  id,
  versionId,
  isPipeline,
  isForked = false,
  forkedProjectId,
  forkedApplicationId,
  pipelineInstructions = '',
  showPipeline = false,
  style,
}: ApplicationInformationProps): ReactNode {
  const projectId = useSelectedProjectId();

  const forkedApplication = useForkedApplicationName({ isForked, forkedProjectId, forkedApplicationId });
  const trigger = usePipelineTriggerType({ isPipeline, projectId, versionId });
  const scheduleDetails = useScheduleTriggerDetails(trigger.schedule);

  const [showPipelineModal, setShowPipelineModal] = useState(false);
  const onShowPipelineModal = useCallback(() => setShowPipelineModal(true), []);
  const onClosePipelineModal = useCallback(() => setShowPipelineModal(false), []);

  const idLabel = isPipeline
    ? t('agents.applicationInformation.pipelineIdLabel', 'Pipeline ID:')
    : t('agents.applicationInformation.agentIdLabel', 'Agent ID:');

  const informationContent = (
    <ApplicationInformationRows
      idLabel={idLabel}
      id={id}
      versionId={versionId}
      isPipeline={isPipeline}
      triggerType={trigger.type}
      schedule={scheduleDetails}
      isForked={isForked}
      originalApplicationName={forkedApplication.name}
      forkedApplicationError={forkedApplication.error}
      showPipeline={showPipeline}
      onShowPipeline={onShowPipelineModal}
    />
  );

  return (
    <>
      <BasicAccordion
        data-testid="agent-information-section"
        showMode="left"
        slotSx={{ accordion: accordionSx, ...(style !== undefined ? { root: style } : {}) }}
        items={[{ title: t('agents.applicationInformation.title', 'Information'), content: informationContent }]}
      />
      {showPipeline && (
        <StyledShowContextModal
          context={pipelineInstructions}
          open={showPipelineModal}
          onClose={onClosePipelineModal}
          contextLabel={t('agents.applicationInformation.pipelineModalTitle', 'Pipeline')}
          renderContextAsMermaid
        />
      )}
    </>
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({ background: theme.vars.palette.background.tabPanel });

const contentContainerSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  alignItems: 'flex-start',
  gap: '0.5rem',
  paddingBottom: '1.5rem',
};

const pipelineLinkSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'row',
  alignItems: 'center',
  gap: '0.75rem',
};

const showLinkSx: SxProps<Theme> = {
  cursor: 'pointer',
  color: (theme: Theme) => theme.vars.palette.text.button.showMore,
};

const forkedFromDisabledSx: SxProps<Theme> = { color: 'text.disabled', cursor: 'not-allowed' };
