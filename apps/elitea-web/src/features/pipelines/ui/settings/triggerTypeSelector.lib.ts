import { useCallback, useEffect, useRef } from 'react';

import { load } from 'js-yaml';

import type { PipelineInboundTriggerModeRequest } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';
import { buildErrorMessage } from '@/shared/lib/http-error';
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

import type { UsePipelineTriggersResult } from '../../api/usePipelineTriggers';
import { FlowEditorConstants } from '../../lib/flow-editor/constants';
import { pipelineErrorMessage } from '../../lib/hooks/pipelineErrorMessage';

/**
 * Split out of `TriggerTypeSelector.tsx` -- constants, pure functions, and
 * the data-mutating custom hooks it composes -- purely to keep that file
 * under the §3.5 400-line budget. See `TriggerTypeSelector.tsx`'s own doc
 * comment for the full provenance and for #899's redesign of this surface.
 */

/** Node types requiring user interaction and thus only supporting the Chat Message trigger (baseline: `INTERACTIVE_NODE_TYPES`). */
const INTERACTIVE_NODE_TYPES: readonly string[] = [
  FlowEditorConstants.PipelineNodeTypes.Hitl,
  FlowEditorConstants.PipelineNodeTypes.Printer,
];

export const TRIGGER_TYPES = {
  chat_message: 'chat_message',
  schedule: 'schedule',
  webhook: 'webhook',
} as const;

export const TRIGGER_OPTIONS: SingleSelectOption[] = [
  { label: 'Chat Message', value: TRIGGER_TYPES.chat_message },
  { label: 'Schedule', value: TRIGGER_TYPES.schedule },
  { label: 'Webhook', value: TRIGGER_TYPES.webhook },
];

/** The cron the Schedule modal opens on when this pipeline has none yet (baseline: `PipelineScheduleModal`'s own default). */
export const DEFAULT_PIPELINE_CRON = '0 0 * * 6';

interface ParsedPipelineYaml {
  readonly nodes?: readonly { readonly type?: string }[];
  readonly interrupt_before?: readonly unknown[];
  readonly interrupt_after?: readonly unknown[];
}

/** `TriggerTypeSelector.jsx:60-79`'s `hasInteractiveElements` computation. */
export function computeHasInteractiveElements(versionInstructions: string | undefined): boolean {
  if (!versionInstructions) return false;
  let parsed: ParsedPipelineYaml | undefined;
  try {
    parsed = load(versionInstructions) as ParsedPipelineYaml | undefined;
  } catch {
    return false;
  }
  if (!parsed) return false;

  const hasInteractiveNodes = (parsed.nodes ?? []).some(node => node.type !== undefined && INTERACTIVE_NODE_TYPES.includes(node.type));
  const hasInterrupts = (parsed.interrupt_before?.length ?? 0) > 0 || (parsed.interrupt_after?.length ?? 0) > 0;
  return hasInteractiveNodes || hasInterrupts;
}

export function buildTriggerTooltip(hasInteractiveElements: boolean): string {
  const base = t(
    'pipelines.triggerTypeSelector.tooltipBase',
    'Choose how this pipeline is triggered.\n• Chat Message (default) requires user input.\n• Schedule runs automatically based on a cron expression.\n• Webhook allows external systems to trigger the pipeline via HTTP POST.',
  );
  if (!hasInteractiveElements) return base;
  return `${base}\n\n${t(
    'pipelines.triggerTypeSelector.tooltipInteractiveNote',
    'Note: This pipeline contains HITL, Printer nodes, or interrupts that require user interaction. Only Chat Message trigger is available.',
  )}`;
}

/**
 * **Confirmed regression fix (cluster A2-settings-panels, findings 3 & 4):**
 * every trigger-mutation `catch` below used to report a fixed generic message
 * with no reference to the caught error, discarding the backend's own error
 * text the old app surfaced (`TriggerTypeSelector.jsx`'s
 * `toastError(error?.data?.error || '...')`).
 *
 * The Go backend's error envelope for these operations is the flat
 * `{"error": "message"}` shape (`internal/api/v2/pipelinetriggers/handler.go`'s
 * `writeError`), and `EliteaApiError.message` alone (`mutator.ts`'s
 * `describeFailure`) does NOT carry that text, only `status`/`url` --
 * `shared/lib/http-error.ts`'s `buildErrorMessage` does the `data.error`
 * dispatch. Adapting `EliteaApiError.failure` into the RTK-Query-shaped input
 * it expects is the same pattern `features/chat-conversation-list/lib/
 * errorMessage.ts` and `features/notifications/lib/errorMessage.ts` use;
 * duplicated rather than imported (`no-sideways-features`/R-L3).
 */
function pipelineTriggerErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const built = buildErrorMessage({ status: error.failure.status, originalStatus: error.failure.status, data: error.failure.body });
    if (typeof built === 'string' && built) return built;
  }
  return error instanceof Error || typeof error === 'string' ? pipelineErrorMessage(error) || fallback : fallback;
}

/** Which unattended entry points this pipeline version currently has. */
export interface ConfiguredTriggerKinds {
  readonly hasSchedule: boolean;
  readonly hasWebhook: boolean;
}

/**
 * What the selector shows as the CURRENT trigger.
 *
 * The backend has no trigger-type column: a schedule row and a trigger token
 * are independent and a pipeline may hold both. "Chat Message" is therefore
 * not a stored state at all -- it is the absence of both, which is exactly
 * what it means for the user (somebody has to type something to start this).
 * When both exist the schedule is shown, because it is the one that starts
 * runs on its own; the webhook is still listed beside it.
 */
export function currentTriggerKind(kinds: ConfiguredTriggerKinds): string {
  if (kinds.hasSchedule) return TRIGGER_TYPES.schedule;
  if (kinds.hasWebhook) return TRIGGER_TYPES.webhook;
  return TRIGGER_TYPES.chat_message;
}

/** A trigger row the selector lists beneath the dropdown. */
export interface TriggerListEntry {
  readonly kind: 'schedule' | 'webhook';
  readonly label: string;
  readonly detail: string;
}

export function buildTriggerList(args: {
  readonly hasSchedule: boolean;
  readonly cron: string | undefined;
  readonly hasWebhook: boolean;
  readonly webhookUrl: string | undefined;
}): readonly TriggerListEntry[] {
  const entries: TriggerListEntry[] = [];
  if (args.hasSchedule) {
    entries.push({ kind: 'schedule', label: t('pipelines.triggerTypeSelector.scheduleRow', 'Schedule'), detail: args.cron ?? '' });
  }
  if (args.hasWebhook) {
    entries.push({ kind: 'webhook', label: t('pipelines.triggerTypeSelector.webhookRow', 'Webhook'), detail: args.webhookUrl ?? '' });
  }
  return entries;
}

/**
 * The auto-reset-to-Chat-Message effect (baseline:
 * `TriggerTypeSelector.jsx:112-143`). "Reset to Chat Message" now means
 * REMOVING whichever unattended entry points exist, since that is what
 * Chat Message is (see {@link currentTriggerKind}).
 */
export function useAutoResetTriggerOnInteractive(args: {
  readonly hasInteractiveElements: boolean;
  readonly kinds: ConfiguredTriggerKinds;
  readonly removeAll: () => Promise<void>;
  readonly onNotifySuccess: ((message: string) => void) | undefined;
  readonly onNotifyError: ((message: string) => void) | undefined;
}): void {
  const { hasInteractiveElements, kinds, removeAll, onNotifySuccess, onNotifyError } = args;
  const prevHasInteractiveRef = useRef(hasInteractiveElements);

  useEffect(() => {
    const becameInteractive = !prevHasInteractiveRef.current && hasInteractiveElements;
    const hasIncompatibleTrigger = kinds.hasSchedule || kinds.hasWebhook;

    if (becameInteractive && hasIncompatibleTrigger) {
      removeAll()
        .then(() => onNotifySuccess?.(t('pipelines.triggerTypeSelector.resetToChatMessage', 'Trigger reset to Chat Message (pipeline now contains interactive elements)')))
        .catch(() => onNotifyError?.(t('pipelines.triggerTypeSelector.resetFailed', 'Failed to reset trigger')));
    }

    prevHasInteractiveRef.current = hasInteractiveElements;
    // baseline's own deps array (`TriggerTypeSelector.jsx:135-143`).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hasInteractiveElements, kinds.hasSchedule, kinds.hasWebhook]);
}

export interface TriggerActions {
  readonly handleTriggerTypeChange: (newType: string) => Promise<void>;
  readonly handleScheduleSubmit: (cronExpression: string) => Promise<void>;
  readonly handleDeleteKind: (kind: 'schedule' | 'webhook') => Promise<void>;
  /**
   * Rotate (or create) the inbound trigger. The MODE travels with it because
   * the backend writes it on this route and nowhere else (#970): a rotate
   * without one moves a signing trigger back to the bearer mode.
   */
  readonly handleRotateWebhook: (mode?: PipelineInboundTriggerModeRequest['type']) => Promise<void>;
  readonly handleRevealWebhook: () => Promise<void>;
  readonly removeAll: () => Promise<void>;
}

interface TriggerActionsArgs {
  readonly triggers: UsePipelineTriggersResult;
  readonly kinds: ConfiguredTriggerKinds;
  readonly setIsUpdating: (value: boolean) => void;
  readonly setIsScheduleModalOpen: (value: boolean) => void;
  readonly setIsWebhookModalOpen: (value: boolean) => void;
  readonly setRevealedSecret: (value: string | undefined) => void;
  readonly onNotifySuccess: ((message: string) => void) | undefined;
  readonly onNotifyError: ((message: string) => void) | undefined;
}

/** Every trigger-mutating handler (baseline: `TriggerTypeSelector.jsx:154-271`, re-pointed at the two Go facilities). */
export function useTriggerActions(args: TriggerActionsArgs): TriggerActions {
  const { triggers, kinds, setIsUpdating, setIsScheduleModalOpen, setIsWebhookModalOpen, setRevealedSecret, onNotifySuccess, onNotifyError } = args;

  const run = useCallback(
    async (action: () => Promise<void>, fallback: string): Promise<void> => {
      try {
        setIsUpdating(true);
        await action();
      } catch (error) {
        onNotifyError?.(pipelineTriggerErrorMessage(error, fallback));
      } finally {
        setIsUpdating(false);
      }
    },
    [setIsUpdating, onNotifyError],
  );

  const handleScheduleSubmit = useCallback(
    (cronExpression: string) => run(async () => {
      await triggers.saveSchedule(cronExpression);
      onNotifySuccess?.(t('pipelines.triggerTypeSelector.scheduleConfigured', 'Schedule configured successfully'));
    }, t('pipelines.triggerTypeSelector.scheduleConfigureFailed', 'Failed to configure schedule')),
    [run, triggers, onNotifySuccess],
  );

  const handleRotateWebhook = useCallback(
    (mode?: PipelineInboundTriggerModeRequest['type']) => run(async () => {
      const rotated = await triggers.rotateWebhook(mode === undefined ? undefined : { type: mode });
      setRevealedSecret(rotated.secret);
      setIsWebhookModalOpen(true);
      onNotifySuccess?.(t('pipelines.triggerTypeSelector.webhookConfigured', 'Webhook configured successfully'));
    }, t('pipelines.triggerTypeSelector.webhookConfigureFailed', 'Failed to configure webhook')),
    [run, triggers, setRevealedSecret, setIsWebhookModalOpen, onNotifySuccess],
  );

  const handleRevealWebhook = useCallback(
    () => run(async () => {
      const revealed = await triggers.revealWebhook();
      setRevealedSecret(revealed.secret);
    }, t('pipelines.triggerTypeSelector.revealFailed', 'Failed to reveal the webhook secret — rotate it to get a new one')),
    [run, triggers, setRevealedSecret],
  );

  const handleDeleteKind = useCallback(
    (kind: 'schedule' | 'webhook') => run(async () => {
      if (kind === 'schedule') {
        await triggers.removeSchedule();
        onNotifySuccess?.(t('pipelines.triggerTypeSelector.scheduleRemoved', 'Schedule removed'));
        return;
      }
      await triggers.removeWebhook();
      setRevealedSecret(undefined);
      setIsWebhookModalOpen(false);
      onNotifySuccess?.(t('pipelines.triggerTypeSelector.webhookRemoved', 'Webhook revoked'));
    }, t('pipelines.triggerTypeSelector.removeFailed', 'Failed to remove the trigger')),
    [run, triggers, setRevealedSecret, setIsWebhookModalOpen, onNotifySuccess],
  );

  const removeAll = useCallback(async (): Promise<void> => {
    if (kinds.hasSchedule) await triggers.removeSchedule();
    if (kinds.hasWebhook) await triggers.removeWebhook();
    setRevealedSecret(undefined);
  }, [kinds.hasSchedule, kinds.hasWebhook, triggers, setRevealedSecret]);

  const handleTriggerTypeChange = useCallback(
    async (newType: string): Promise<void> => {
      if (newType === TRIGGER_TYPES.schedule) {
        setIsScheduleModalOpen(true);
        return;
      }
      if (newType === TRIGGER_TYPES.webhook) {
        if (kinds.hasWebhook) {
          setIsWebhookModalOpen(true);
          return;
        }
        await handleRotateWebhook();
        return;
      }
      await run(async () => {
        await removeAll();
        onNotifySuccess?.(t('pipelines.triggerTypeSelector.updatedToChatMessage', 'Trigger updated to Chat Message'));
      }, t('pipelines.triggerTypeSelector.updateFailed', 'Failed to update trigger'));
    },
    [kinds.hasWebhook, setIsScheduleModalOpen, setIsWebhookModalOpen, handleRotateWebhook, run, removeAll, onNotifySuccess],
  );

  return { handleTriggerTypeChange, handleScheduleSubmit, handleDeleteKind, handleRotateWebhook, handleRevealWebhook, removeAll };
}
