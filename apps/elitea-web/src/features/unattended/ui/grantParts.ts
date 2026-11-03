/**
 * The parts of an unattended grant (ADR-0045 decision 1): what each one
 * answers, and what happens without the grant. The form and the summary both
 * read this list.
 */
import { t } from '@/shared/i18n';

import type { ChainField } from '../model/grant';

export type SwitchPart = 'continue_step_limit' | 'continue_tool_call_cap' | 'continue_output_limit' | 'hook_asks' | 'remote_confirmations';

/** Where a part applies. */
type Scope = 'any' | 'desktop' | 'cloud';

interface PartInfo {
  key: SwitchPart;
  scope: Scope;
  label: () => string;
  help: () => string;
}

const PARTS: readonly PartInfo[] = [
  {
    key: 'continue_step_limit',
    scope: 'any',
    label: () => t('unattended.part.stepLimit', 'Continue at the step limit'),
    help: () => t('unattended.part.stepLimitHelp', 'Answers “Continue?” at the step limit. Without the grant: the run asks; on timeout, it stops.'),
  },
  {
    key: 'continue_tool_call_cap',
    scope: 'desktop',
    label: () => t('unattended.part.toolCallCap', 'Continue after 512 tool calls'),
    help: () => t('unattended.part.toolCallCapHelp', 'Answers the local cap of 512 tool calls. Without the grant: the run stops and you click Continue.'),
  },
  {
    key: 'continue_output_limit',
    scope: 'cloud',
    label: () => t('unattended.part.outputLimit', 'Continue at the output limit'),
    help: () => t('unattended.part.outputLimitHelp', 'Answers the output limit. Without the grant: the run stops and you click Continue.'),
  },
  {
    key: 'hook_asks',
    scope: 'desktop',
    label: () => t('unattended.part.hookAsks', 'Allow hook questions'),
    help: () => t('unattended.part.hookAsksHelp', 'Answers the question of a PreToolUse hook with Allow. Without the grant: the run asks; on timeout, it declines.'),
  },
  {
    key: 'remote_confirmations',
    scope: 'desktop',
    label: () => t('unattended.part.remote', 'Approve remote tool confirmations'),
    help: () => t('unattended.part.remoteHelp', 'Answers a confirmation of a remote toolkit with Approve. Without the grant: the run asks; on timeout, it declines.'),
  },
];

/** Which parts show: `desktop` shows the desktop-only parts, `cloud` the cloud-only ones. */
export interface PartsShown {
  desktop: boolean;
  cloud: boolean;
}

export function visibleParts({ desktop, cloud }: PartsShown): readonly PartInfo[] {
  return PARTS.filter((part) => part.scope === 'any' || (part.scope === 'desktop' ? desktop : cloud));
}

export const sensitiveLabel = (): string => t('unattended.part.sensitive', 'Sensitive tools');
export const sensitiveHelp = (): string => t('unattended.part.sensitiveHelp', 'Runs the sensitive tools that you select without a pause. Without the grant: the run pauses for your approval.');

interface ChainInfo {
  field: ChainField;
  label: () => string;
  help: () => string;
  error: () => string;
}

export const CHAIN_PARTS: readonly ChainInfo[] = [
  {
    field: 'max_continuations',
    label: () => t('unattended.chain.continuations', 'Most automatic continuations'),
    help: () => t('unattended.chain.continuationsHelp', 'From 0 to 200. Default: 20.'),
    error: () => t('unattended.chain.continuationsError', 'Type a whole number from 0 to 200.'),
  },
  {
    field: 'max_spend',
    label: () => t('unattended.chain.spend', 'Most spend (USD)'),
    help: () => t('unattended.chain.spendHelp', 'More than 0, with at most 4 decimals. Default: the spend cap of one run × 10.'),
    error: () => t('unattended.chain.spendError', 'Type an amount more than 0, with at most 4 decimals.'),
  },
  {
    field: 'deadline_hours',
    label: () => t('unattended.chain.deadline', 'Deadline (hours)'),
    help: () => t('unattended.chain.deadlineHelp', 'From 1 second to 7 days (168 hours). Default: 12 hours.'),
    error: () => t('unattended.chain.deadlineError', 'Type a time from 1 second to 168 hours.'),
  },
];
