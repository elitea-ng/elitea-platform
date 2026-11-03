/**
 * The texts of the "Run to completion" control (ADR-0045 decision 1): the
 * parts that also answer for the person (off by default), what each one
 * answers and what happens without it, and the chain limits.
 */
import { t } from '@/shared/i18n';

import type { ChainDraft, ChainField } from '../model/grant';

type OptInPart = 'hook_asks' | 'remote_confirmations';

interface OptInInfo {
  key: OptInPart;
  label: () => string;
  help: () => string;
}

/** The desktop parts that answer for the person. */
export const DESKTOP_OPT_INS: readonly OptInInfo[] = [
  {
    key: 'hook_asks',
    label: () => t('unattended.optIn.hookAsks', 'Hook asks'),
    help: () => t('unattended.optIn.hookAsksHelp', 'Answers the question of a PreToolUse hook with Allow. When it is off, the run waits for you.'),
  },
  {
    key: 'remote_confirmations',
    label: () => t('unattended.optIn.remote', 'Remote confirmations'),
    help: () => t('unattended.optIn.remoteHelp', 'Answers a confirmation of a remote toolkit with Approve. When it is off, the run waits for you.'),
  },
];

export const sensitiveLabel = (): string => t('unattended.part.sensitive', 'Sensitive tools');
export const sensitiveHelp = (): string => t('unattended.optIn.sensitiveHelp', 'Runs the sensitive tools that you select without a pause. The other sensitive tools pause for your approval.');

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
    help: () => t('unattended.chain.continuationsHelp', 'A whole number from 0. The platform sets the maximum. Default: 20.'),
    error: () => t('unattended.chain.continuationsError', 'Type a whole number from 0.'),
  },
  {
    field: 'max_spend',
    label: () => t('unattended.chain.spend', 'Most spend (USD)'),
    help: () => t('unattended.chain.spendHelp', 'More than 0, with at most 4 decimals. The platform sets the maximum. Default: the spend cap of one run × 10.'),
    error: () => t('unattended.chain.spendError', 'Type an amount more than 0, with at most 4 decimals.'),
  },
  {
    field: 'deadline_hours',
    label: () => t('unattended.chain.deadline', 'Deadline (hours)'),
    help: () => t('unattended.chain.deadlineHelp', 'At least 1 second. The platform sets the maximum. Default: 12 hours.'),
    error: () => t('unattended.chain.deadlineError', 'Type a time of at least 1 second (0.0003 hours).'),
  },
];

/** The chain limits in one line: the typed values, else the platform defaults. */
export function chainLine(chain: ChainDraft): string {
  const count = chain.max_continuations.trim();
  const spend = chain.max_spend.trim();
  const hours = chain.deadline_hours.trim();
  return [
    t('unattended.chain.lineContinuations', 'up to {{number}} continuations', { number: count === '' ? '20' : count }),
    spend === '' ? t('unattended.chain.lineSpendDefault', 'the spend cap of one run × 10') : t('unattended.chain.lineSpend', '${{spend}}', { spend }),
    t('unattended.chain.lineDeadline', '{{hours}} h', { hours: hours === '' ? '12' : hours }),
  ].join(', ');
}
