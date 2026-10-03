/**
 * The way out of a budget refusal (#6732).
 *
 * A refused turn showed the budget message and nothing to act on. Main now
 * reports the refusing scope as its own public code; this link sends the
 * reader to Settings > Usage, labelled for whose budget ran out. A member
 * refusal opens the member view (`?scope=user`), which reads the caller's own
 * budget; the project and unscoped refusals open the project view. A plain
 * anchor, not a router link: transcripts also render outside the app router
 * (shared chats, the playback view).
 */
import type { ReactNode } from 'react';

import Link from '@mui/material/Link';

import { getConfig } from '@/shared/config';
import { t } from '@/shared/i18n';
import { normalizeBasename } from '@/shared/lib/basename';

const USAGE_PATH = '/settings/usage';

/** The public failure codes of a model budget refusal, by scope. */
const BUDGET_FAILURE_CODES = new Set(['MODEL_BUDGET_EXHAUSTED', 'PROJECT_BUDGET_EXHAUSTED', 'MEMBER_BUDGET_EXHAUSTED']);

/** @public Whether a failure code is a model budget refusal. */
export function isBudgetFailureCode(code: string | undefined): boolean {
  return code !== undefined && BUDGET_FAILURE_CODES.has(code);
}

/** @public `/settings/usage` under the configured app base. */
export function usageSettingsHref(basePath: string): string {
  return `${normalizeBasename(basePath)}${USAGE_PATH}`;
}

/** @public The Usage view for a budget refusal code: the member view for a member refusal. */
export function budgetUsageHref(basePath: string, code: string | undefined): string {
  const href = usageSettingsHref(basePath);
  return code === 'MEMBER_BUDGET_EXHAUSTED' ? `${href}?scope=user` : href;
}

function resolveUsageHref(code: string | undefined): string {
  if (import.meta.env.DEV) return budgetUsageHref('', code);
  const result = getConfig();
  return budgetUsageHref(result.status === 'ok' ? result.config.vite_base_uri : '', code);
}

export interface BudgetUsageLinkProps {
  readonly code: string | undefined;
}

export function BudgetUsageLink({ code }: BudgetUsageLinkProps): ReactNode {
  if (!isBudgetFailureCode(code)) return null;
  const label = code === 'MEMBER_BUDGET_EXHAUSTED'
    ? t('chatMessages.error.budgetUsageMember', 'View my usage')
    : t('chatMessages.error.budgetUsageProject', 'View project usage');
  return (
    <Link href={resolveUsageHref(code)} variant="bodyMedium" data-testid="budget-usage-link" sx={{ display: 'inline-block', mt: 0.5 }}>
      {label}
    </Link>
  );
}
