/**
 * The composer's usage chip (#1227), beside the context meter: the latest
 * turn's tokens and their cached share, the thread's total, and a cost only
 * when the model catalogue names the model's price (`listConfigurationModels`
 * `input_cost_per_1m_tokens` …, served to a caller who may see amounts, the
 * gateway's own price list). Without a price no currency figure appears:
 * "≈ N tokens". The tooltip adds the sub-agents' share and, when the member
 * may see amounts, their budget for the period (`getMemberBudget`, read once
 * the tooltip opens). Governance stays the gateway's: this only shows.
 * Loaded lazily by `ThreadComposer`.
 */
import { useState } from 'react';

import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { composer, type WorkspaceTurn } from '@/features/workspace';
import type { SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetMemberBudget } from '@/shared/api/generated/budgets/budgets';
import { useListConfigurationModels } from '@/shared/api/generated/configurations/configurations';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import type { UsageCounts } from '@/shared/desktop/turnUsage';
import { t } from '@/shared/i18n';
import { formatMoney } from '@/shared/lib/money';

export type TurnUsage = NonNullable<WorkspaceTurn['view']['usage']>;

/** A model's price per 1M tokens, as the catalogue serves it. */
export interface ModelPrice {
  input: number;
  output: number;
  cachedInput: number;
  currency: string;
}

/** What the member's budget says, when they may see its amounts. */
export interface BudgetLine {
  limit: number | null;
  percentUsed: number | null;
  currency: string;
}

const compact = (value: number): string => new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value);

const finite = (value: unknown): number | undefined => (typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : undefined);

/** `name`'s price in the catalogue body; `undefined` unless all three prices and the currency are there. */
export function modelPrice(body: unknown, name: string | undefined): ModelPrice | undefined {
  if (name === undefined) return undefined;
  const items = (unwrapBody(body) as { items?: unknown } | undefined)?.items;
  const row = (Array.isArray(items) ? (items as Record<string, unknown>[]) : []).find((item) => item['name'] === name);
  if (row === undefined) return undefined;
  const input = finite(row['input_cost_per_1m_tokens']);
  const output = finite(row['output_cost_per_1m_tokens']);
  const cachedInput = finite(row['cached_input_cost_per_1m_tokens']);
  const currency = row['currency'];
  if (input === undefined || output === undefined || cachedInput === undefined || typeof currency !== 'string' || currency === '') return undefined;
  return { input, output, cachedInput, currency };
}

/** The member budget's line, only when its amounts may be shown. */
export function budgetLine(body: unknown): BudgetLine | undefined {
  const budget = unwrapBody(body) as Record<string, unknown> | undefined;
  if (budget?.['can_see_amounts'] !== true || typeof budget['currency'] !== 'string') return undefined;
  return { limit: finite(budget['effective_limit']) ?? null, percentUsed: finite(budget['percent_used']) ?? null, currency: budget['currency'] };
}

/** What `usage` cost at `price`: cached input at its own price, the rest of the prompt at the input price. */
export function costOf(usage: UsageCounts, price: ModelPrice): number {
  const fresh = Math.max(0, usage.prompt - usage.cached);
  return (fresh * price.input + usage.cached * price.cachedInput + usage.completion * price.output) / 1_000_000;
}

const tokensOf = (usage: UsageCounts): number => usage.prompt + usage.completion;

export interface UsageText {
  label: string;
  detail: string;
  /** Share of the turn's prompt read from the provider's cache, 0 to 100. */
  cachedPercent: number;
}

/** A thread's total and, when every turn's model has a price in one currency, its cost. */
export interface ThreadUsage {
  counts: UsageCounts;
  cost?: { amount: number; currency: string };
}

/** The thread's turns summed; each turn costed at its own model's price. */
export function threadUsage(turns: readonly TurnUsage[], priceOf: (model: string | undefined) => ModelPrice | undefined): ThreadUsage | undefined {
  const counts = composer.sumUsage(turns);
  if (counts === undefined) return undefined;
  const prices = turns.map((turn) => priceOf(turn.model));
  const currency = prices[0]?.currency;
  if (currency === undefined || prices.some((price) => price?.currency !== currency)) return { counts };
  const amount = turns.reduce((sum, turn, index) => sum + costOf(turn, prices[index] as ModelPrice), 0);
  return { counts, cost: { amount, currency } };
}

/** What the chip says: the label for the turn, the tooltip for the rest. */
export function describeUsage(turn: TurnUsage, thread: ThreadUsage | undefined, price: ModelPrice | undefined, budget: BudgetLine | undefined): UsageText {
  const cachedPercent = turn.prompt > 0 ? Math.round((turn.cached / turn.prompt) * 100) : 0;
  const tokens = compact(tokensOf(turn));
  const label =
    price === undefined
      ? t('workspace.usage.label', '≈ {{tokens}} tokens', { tokens })
      : t('workspace.usage.labelCost', '{{cost}} · {{tokens}} tokens', { cost: formatMoney(costOf(turn, price), price.currency), tokens });
  const lines = [
    t('workspace.usage.turn', 'This turn: {{prompt}} in ({{cached}}% cached), {{completion}} out', { prompt: compact(turn.prompt), cached: cachedPercent, completion: compact(turn.completion) }),
  ];
  if (turn.reasoning > 0) lines.push(t('workspace.usage.reasoning', '{{reasoning}} of the output was reasoning', { reasoning: compact(turn.reasoning) }));
  if (turn.subagents !== undefined) lines.push(t('workspace.usage.subagents', 'Sub-agents: {{tokens}} tokens of it', { tokens: compact(tokensOf(turn.subagents)) }));
  if (!turn.final) lines.push(t('workspace.usage.running', 'Still running: the agent’s own calls so far'));
  if (thread !== undefined) {
    const threadTokens = compact(tokensOf(thread.counts));
    lines.push(
      thread.cost === undefined
        ? t('workspace.usage.thread', 'This thread: {{tokens}} tokens', { tokens: threadTokens })
        : t('workspace.usage.threadCost', 'This thread: {{tokens}} tokens, {{cost}}', { tokens: threadTokens, cost: formatMoney(thread.cost.amount, thread.cost.currency) }),
    );
  }
  if (budget !== undefined && budget.limit !== null) {
    lines.push(
      t('workspace.usage.budget', 'Your budget: {{percent}}% of {{limit}} used this period', { percent: Math.round(budget.percentUsed ?? 0), limit: formatMoney(budget.limit, budget.currency) }),
    );
  }
  return { label, detail: lines.join(' · '), cachedPercent };
}

export interface UsageChipProps {
  /** The thread's turns that reported usage, oldest first: the last is the one shown. */
  turns: readonly TurnUsage[];
  projectId: number;
}

export default function UsageChip({ turns, projectId }: UsageChipProps): React.JSX.Element | null {
  const [open, setOpen] = useState(false);
  // The model chip's read (same key): already cached when it is shown.
  const catalogue = useListConfigurationModels(String(projectId), { section: 'llm', include_shared: true }, { query: { staleTime: 60_000 } });
  const author = useGetCurrentAuthor({ query: { enabled: open } });
  const userId = (unwrapBody(author.data) as SocialAuthorProfile | undefined)?.id;
  const member = useGetMemberBudget(String(projectId), Number(userId), { query: { enabled: open && userId !== undefined, staleTime: 60_000 } });
  const usage = turns[turns.length - 1];
  if (usage === undefined) return null;
  const priceOf = (model: string | undefined) => modelPrice(catalogue.data, model);
  return <UsageChipView text={describeUsage(usage, threadUsage(turns, priceOf), priceOf(usage.model), budgetLine(member.data))} onOpen={() => setOpen(true)} />;
}

/** The chip itself, from what it says. */
export function UsageChipView({ text, onOpen }: { text: UsageText; onOpen?: () => void }): React.JSX.Element {
  const { label, detail, cachedPercent } = text;
  return (
    <Tooltip title={detail} {...(onOpen === undefined ? {} : { onOpen })}>
      <Box
        component="span"
        data-testid="usage-chip"
        data-cached-percent={cachedPercent}
        aria-label={t('workspace.usage.aria', 'Tokens used: {{label}}', { label })}
        sx={{ display: 'inline-flex', alignItems: 'center', color: 'text.secondary', flexShrink: 0 }}
      >
        <Typography component="span" variant="labelSmall" aria-hidden>
          {label}
        </Typography>
      </Box>
    </Tooltip>
  );
}
