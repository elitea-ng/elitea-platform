import { memo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AnalyticsKpis } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { fmtNum } from '../../lib/format';
import { KpiCard } from './KpiCard';
import type { KpiCardProps } from './KpiCard';

/**
 * The KPI row shared by `AnalyticsOverview` and the three detail screens.
 *
 * ── WHY THE TILE SET IS NOT FIXED ──
 *
 * It used to be six hardcoded tiles, and all six read a REQUIRED field: the
 * spec declared `unique_users`, `total_project_users`, `ai_active_users`,
 * `adoption_rate`, `llm_calls`, `tool_runs`, `chat_msgs` and `agent_runs` as
 * numbers that were always present, so the row could print all of them
 * unconditionally.
 *
 * The server never agreed. Six of the eight were literal zeros no query
 * produced (issue #303) and were then REMOVED from the response rather than
 * zeroed, which left the contract asserting fields no server sends — a row
 * printing `NaN` or `0` for figures nothing measures. Half of them now have a
 * real producer (gateway.llm_request_logs, shared migration 0099) and half
 * still have none, and which half is which depends on the deployment.
 *
 * So the row renders A TILE PER FIGURE THE SERVER SENT. An absent figure means
 * "nothing in this platform produces this", and the honest rendering of that is
 * no tile — not a tile reading 0, which is a measurement, and not a tile
 * reading "—", which invites the reader to wonder what broke. `AnalyticsKpis`
 * is now all-optional in the spec for exactly this reason.
 *
 * Cost is deliberately NOT here. It has one producer and one view of it
 * (/analytics_costs, which carries the scope rules that stop it double-counting
 * a user-scope row into its own project's total); `AnalyticsOverview` fetches
 * it separately and passes it in as `totalCost` rather than the server
 * publishing a second, potentially disagreeing figure on this endpoint.
 */
export interface AnalyticsKpiRowProps {
  readonly kpis: AnalyticsKpis | undefined;
  /**
   * Project spend for the same window, from /analytics_costs. Undefined when
   * that query has not resolved or the caller does not show cost (the detail
   * screens do not).
   */
  readonly totalCost?: string | undefined;
  /**
   * The billing period(s) `totalCost` covers (`fmtBilledPeriod`). The figure
   * is a whole-period accumulator, not the selected window's spend, so the
   * tile names the period instead of implying the window.
   */
  readonly totalCostPeriod?: string | undefined;
}

const rowSx: SxProps<Theme> = {
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fit, minmax(9rem, 1fr))',
  gap: (theme: Theme) => theme.spacing(2),
};

/**
 * The COST tile names the billing period its figure covers: an accumulator
 * row is a whole period, so under `Today` the figure is the month to date.
 */
function costSubtitle(period: string | undefined): string {
  if (period === undefined) return t('analytics.kpi.cost.subtitleNoPeriod', 'billing period to date, USD');
  return t('analytics.kpi.cost.subtitle', 'billed {{period}}, USD', { period });
}

/**
 * The tiles, as data.
 *
 * Written as a list rather than as seven `{cond && <KpiCard/>}` branches
 * because the branches measured a complexity of 19 against this repo's budget
 * of 12 — but the shape is also the honest one: "which tiles exist" is a
 * question about the response, and answering it in one place keeps the JSX from
 * having to re-ask it per tile.
 */
function tilesFor(
  kpis: AnalyticsKpis | undefined,
  totalCost: string | undefined,
  totalCostPeriod: string | undefined,
): readonly KpiCardProps[] {
  const tiles: KpiCardProps[] = [];
  if (kpis === undefined) return tiles;

  const { ai_active_users: active, total_project_users: members, adoption_rate: adoption } = kpis;
  if (active !== undefined) {
    tiles.push({
      label: t('analytics.kpi.aiActive.label', 'AI ACTIVE'),
      value: fmtNum(active),
      // The suffix and the badge each depend on the DENOMINATOR, not on this
      // tile's own figure, and the denominator comes from identity tables this
      // service does not own. Where they are absent the tile still reports how
      // many people used AI — it just cannot say out of how many, which is a
      // smaller and true claim.
      ...(members !== undefined
        ? { valueSuffix: t('analytics.kpi.team.suffix', 'of {{total}}', { total: fmtNum(members) }) }
        : {}),
      ...(adoption !== undefined && adoption > 0
        ? {
            badge: t('analytics.kpi.aiActive.badge', '↑{{rate}}%', { rate: adoption }),
            subtitle: t('analytics.kpi.aiActive.subtitle', '{{rate}}% adoption', { rate: adoption }),
          }
        : { subtitle: t('analytics.kpi.aiActive.subtitleNoRate', 'members who called a model') }),
    });
  }
  if (kpis.llm_calls !== undefined) {
    tiles.push({
      label: t('analytics.kpi.llmCalls.label', 'LLM CALLS'),
      value: fmtNum(kpis.llm_calls),
      // The old subtitle said `event_type = llm`, naming a column of
      // centry.audit_events — a table this service cannot write and does not
      // read. The figure comes from the gateway's own request log.
      subtitle: t('analytics.kpi.llmCalls.subtitle', 'requests served by the gateway'),
    });
  }
  if (kpis.total_tokens !== undefined) {
    tiles.push({
      label: t('analytics.kpi.tokens.label', 'TOKENS'),
      value: fmtNum(kpis.total_tokens),
      subtitle: t('analytics.kpi.tokens.subtitle', 'prompt + completion'),
    });
  }
  if (totalCost !== undefined) {
    tiles.push({
      label: t('analytics.kpi.cost.label', 'COST'),
      value: totalCost,
      subtitle: costSubtitle(totalCostPeriod),
    });
  }
  // The three below have no producer today, so these branches never fire. They
  // stay because each is one query away from being real, and a tile that
  // appears the day its figure does is a smaller change than one that has to be
  // rediscovered.
  if (kpis.tool_runs !== undefined) {
    tiles.push({ label: t('analytics.kpi.toolRuns.label', 'TOOL RUNS'), value: fmtNum(kpis.tool_runs) });
  }
  if (kpis.chat_msgs !== undefined) {
    tiles.push({ label: t('analytics.kpi.chatMsg.label', 'CHAT MSG'), value: fmtNum(kpis.chat_msgs) });
  }
  if (kpis.agent_runs !== undefined) {
    tiles.push({ label: t('analytics.kpi.agentRuns.label', 'AGENT RUNS'), value: fmtNum(kpis.agent_runs) });
  }
  return tiles;
}

function AnalyticsKpiRowImpl({ kpis, totalCost, totalCostPeriod }: AnalyticsKpiRowProps): ReactNode {
  return (
    // The testid is the @visual suite's landmark for "the analytics query has
    // resolved and the tab content is painted". Before it existed, the visual
    // spec waited on `getByRole('main')` — which is satisfied by the loading
    // spinner — and the committed baseline was a picture of that spinner
    // (run 31345403013, issue #159).
    <Box sx={rowSx} data-testid="analytics-kpi-row">
      {tilesFor(kpis, totalCost, totalCostPeriod).map((tile) => (
        <KpiCard
          key={tile.label}
          {...tile}
        />
      ))}
    </Box>
  );
}

export const AnalyticsKpiRow = memo(AnalyticsKpiRowImpl);
