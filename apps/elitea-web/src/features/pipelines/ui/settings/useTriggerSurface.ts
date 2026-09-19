import { useContext, useMemo, useState } from 'react';

import { hasBackendCapability } from '@/shared/config';
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

import { usePipelineTriggers, type UsePipelineTriggersResult } from '../../api/usePipelineTriggers';
import { PipelineTriggerScopeContext, type PipelineTriggerScope } from '../../lib/flow-editor/flowEditorContext';
import {
  DEFAULT_PIPELINE_CRON,
  TRIGGER_OPTIONS,
  TRIGGER_TYPES,
  type ConfiguredTriggerKinds,
  type TriggerActions,
  type TriggerListEntry,
  buildTriggerList,
  buildTriggerTooltip,
  computeHasInteractiveElements,
  currentTriggerKind,
  useAutoResetTriggerOnInteractive,
  useTriggerActions,
} from './triggerTypeSelector.lib';

/**
 * Everything `TriggerTypeSelector` renders, resolved in one place.
 *
 * Split out of that component purely for the §3.5 cyclomatic-complexity
 * budget (12): the surface is a derivation of two independent reads plus two
 * modal states, and inlining it put the component at 20. No behaviour of its
 * own lives here that is not described by `TriggerTypeSelector`'s own doc
 * comment.
 */
export interface TriggerSurfaceInput {
  readonly projectId?: string | undefined;
  readonly versionId?: number | undefined;
  readonly versionInstructions?: string | undefined;
  readonly onNotifySuccess?: ((message: string) => void) | undefined;
  readonly onNotifyError?: ((message: string) => void) | undefined;
}

export interface TriggerSurface {
  readonly options: SingleSelectOption[];
  readonly currentTriggerType: string;
  readonly entries: readonly TriggerListEntry[];
  readonly tooltip: string;
  /** True while either read or any write is in flight — the whole surface is disabled then. */
  readonly isLoading: boolean;
  readonly isUpdating: boolean;
  readonly actions: TriggerActions;
  readonly cron: string;
  readonly webhookUrl: string | undefined;
  readonly revealedSecret: string | undefined;
  readonly isScheduleModalOpen: boolean;
  readonly isWebhookModalOpen: boolean;
  readonly openScheduleModal: () => void;
  readonly closeScheduleModal: () => void;
  readonly openWebhookModal: () => void;
  readonly closeWebhookModal: () => void;
}

/** The dropdown offers only Chat Message for a graph that needs a person mid-run, and for a build that does not serve the two facilities at all. */
function triggerOptions(hasInteractiveElements: boolean): SingleSelectOption[] {
  const chatMessageOnly = hasInteractiveElements || !hasBackendCapability('pipelineTriggers');
  return chatMessageOnly ? TRIGGER_OPTIONS.filter(option => option.value === TRIGGER_TYPES.chat_message) : TRIGGER_OPTIONS;
}

/** An explicit prop wins over the editor's ambient scope; either may be absent. */
function resolveScope(input: TriggerSurfaceInput, ambient: PipelineTriggerScope | undefined): PipelineTriggerScope {
  return {
    projectId: input.projectId ?? ambient?.projectId,
    versionId: input.versionId ?? ambient?.versionId,
    versionInstructions: input.versionInstructions ?? ambient?.versionInstructions,
  };
}

/** A REVOKED trigger is kept as evidence and refused by the inbound path, so it is not a configured webhook here either. */
function configuredKinds(triggers: UsePipelineTriggersResult): ConfiguredTriggerKinds {
  return {
    hasSchedule: triggers.schedule?.configured === true,
    hasWebhook: triggers.webhook?.configured === true && triggers.webhook.revoked_at === undefined,
  };
}

export function useTriggerSurface(input: TriggerSurfaceInput): TriggerSurface {
  const ambient = useContext(PipelineTriggerScopeContext);
  const { projectId, versionId, versionInstructions } = resolveScope(input, ambient);
  const { onNotifySuccess, onNotifyError } = input;

  const [isScheduleModalOpen, setIsScheduleModalOpen] = useState(false);
  const [isWebhookModalOpen, setIsWebhookModalOpen] = useState(false);
  const [isUpdating, setIsUpdating] = useState(false);
  const [revealedSecret, setRevealedSecret] = useState<string | undefined>(undefined);

  const hasInteractiveElements = useMemo(() => computeHasInteractiveElements(versionInstructions), [versionInstructions]);
  const triggers = usePipelineTriggers(projectId, versionId);
  const kinds = configuredKinds(triggers);

  const actions = useTriggerActions({
    triggers,
    kinds,
    setIsUpdating,
    setIsScheduleModalOpen,
    setIsWebhookModalOpen,
    setRevealedSecret,
    onNotifySuccess,
    onNotifyError,
  });

  useAutoResetTriggerOnInteractive({ hasInteractiveElements, kinds, removeAll: actions.removeAll, onNotifySuccess, onNotifyError });

  return {
    options: triggerOptions(hasInteractiveElements),
    currentTriggerType: currentTriggerKind(kinds),
    entries: buildTriggerList({ hasSchedule: kinds.hasSchedule, cron: triggers.schedule?.cron, hasWebhook: kinds.hasWebhook, webhookUrl: triggers.webhook?.url }),
    tooltip: buildTriggerTooltip(hasInteractiveElements),
    isLoading: triggers.isFetching || isUpdating,
    isUpdating,
    actions,
    cron: triggers.schedule?.cron ?? DEFAULT_PIPELINE_CRON,
    webhookUrl: triggers.webhook?.url,
    revealedSecret,
    isScheduleModalOpen,
    isWebhookModalOpen,
    openScheduleModal: () => setIsScheduleModalOpen(true),
    closeScheduleModal: () => setIsScheduleModalOpen(false),
    openWebhookModal: () => setIsWebhookModalOpen(true),
    closeWebhookModal: () => setIsWebhookModalOpen(false),
  };
}
