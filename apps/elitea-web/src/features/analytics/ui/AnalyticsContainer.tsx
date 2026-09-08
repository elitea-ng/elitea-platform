import { useCallback, useMemo, useState } from 'react';
import type { ReactNode, SyntheticEvent } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';
import { AdapterDateFns } from '@mui/x-date-pickers/AdapterDateFns';
import { LocalizationProvider } from '@mui/x-date-pickers/LocalizationProvider';

import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';
import { BriefcaseIcon } from '@/shared/ui/icons/briefcase-icon';
import { TabGroupButton } from '@/shared/ui/TabGroupButton';

import { useProjectAnalyticsQuery, useProjectCostsQuery } from '../api/useAnalytics';
import { ANALYTICS_TAB, DATE_FILTER_PRESETS } from '../lib/constants';
import { presetToDateRange, toIsoRange } from '../model/dateRange';
import { AnalyticsTabContent } from './components/AnalyticsTabContent';
import { DateRangeField } from './components/DateRangeField';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/analytics/ui/AnalyticsContainer.jsx`
 * (ROUTE-060, `/settings/analytics`).
 *
 * `projectId`/`projectName` are INJECTED PROPS, not read internally — the
 * baseline sourced them from `useSelectedProjectId`/`useSelectedProjectName`
 * (Redux). Wave 1 has no equivalent: `entities/project`'s public API
 * (`src/entities/project/index.ts`) exports only types and pure predicates,
 * no "current project" accessor, and the one implementation that exists
 * (`getProjectStore()` in `src/routes/$projectId.$.tsx`) is R1/R3's own
 * routing-layer file — importing it from `features/` would be an upward
 * import (R-L1: `features` may not import `pages`/`processes`/`app`, and a
 * route file sits at that level architecturally). Documented as a genuine
 * Wave-1 gap in this unit's final report rather than worked around outside
 * this unit's ownership fence (`src/features/analytics/`): whoever wires
 * this component into the real `/settings/analytics` route supplies these
 * two props once a shared "current project" selector exists (ideally
 * promoted into `entities/project`).
 *
 * DROPPED vs. the baseline: the interactive-tours integration
 * (`useInteractiveTour`, `data-tour` attributes, the tab-jump `useEffect`).
 * `useInteractiveTour` lives in `app/providers` (an upward import) and
 * `ANALYTICS_TOUR_ID`/`ANALYTICS_TOUR_TARGET_IDS` in `features/
 * interactive-tours` (a sideways `features/`-to-`features/` import, R-L1) —
 * neither is reachable from this slice, and `features/interactive-tours`
 * does not exist yet in this codebase at all. Left out rather than
 * reached-around, per this programme's ownership-fence rule.
 *
 * The six-tab content area and the `From`/`To` date fields are extracted
 * into `ui/components/{AnalyticsTabContent,DateRangeField}.tsx` — besides
 * removing real duplication (the baseline's two near-identical
 * `DateTimePicker` blocks), that extraction is what brings this
 * component's own cyclomatic complexity under the `eslint(complexity)`
 * budget of 12 (it measured 19 before).
 */
export interface AnalyticsContainerProps {
  readonly projectId: string | undefined;
  readonly projectName?: string;
}

const TAB_LABELS: readonly { readonly key: string; readonly fallback: string }[] = [
  { key: 'analytics.tabs.overview', fallback: 'Overview' },
  { key: 'analytics.tabs.costs', fallback: 'Costs' },
  { key: 'analytics.tabs.tokens', fallback: 'Tokens' },
  { key: 'analytics.tabs.agents', fallback: 'Agents' },
  { key: 'analytics.tabs.tools', fallback: 'Tools' },
  { key: 'analytics.tabs.users', fallback: 'Users' },
  { key: 'analytics.tabs.health', fallback: 'Health' },
  { key: 'analytics.tabs.guide', fallback: 'Guide' },
];

const headerSx: SxProps<Theme> = {
  height: '3.8rem',
  minHeight: '3.8rem',
  display: 'flex',
  alignItems: 'center',
  gap: (theme: Theme) => theme.spacing(1.5),
  padding: (theme: Theme) => `0 ${theme.spacing(3)}`,
  boxSizing: 'border-box',
};

const projectLabelSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: theme.spacing(0.5),
  border: `1px solid ${theme.vars.palette.border.lines}`,
  padding: `${theme.spacing(0.5)} ${theme.spacing(1)}`,
  borderRadius: theme.vars.shape.radiusMd,
});

const filterBarSx = (theme: Theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  flexWrap: 'wrap',
  gap: theme.spacing(1.5),
  padding: `${theme.spacing(2)} ${theme.spacing(3)}`,
  borderTop: `1px solid ${theme.vars.palette.border.table}`,
  background: theme.vars.palette.background.tabPanel,
});

const dateFieldsRowSx: SxProps<Theme> = { display: 'flex', gap: (theme: Theme) => theme.spacing(1), alignItems: 'center' };

const tabsContainerSx = (theme: Theme) => ({
  padding: `0 ${theme.spacing(3)}`,
  borderBottom: `1px solid ${theme.vars.palette.border.table}`,
  background: theme.vars.palette.background.tabPanel,
});

const shellSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', flex: 1, overflow: 'hidden' };

const contentAreaSx: SxProps<Theme> = {
  flex: 1,
  overflow: 'auto',
  padding: (theme: Theme) => theme.spacing(3),
  position: 'relative',
};

/**
 * Two fraction digits, not the accumulator's full NUMERIC precision. The stored
 * figure is exact to the nano-USD (a real total is `2.00000001`), and a KPI tile
 * is not the place a reader needs the ninth decimal — /analytics_costs' own
 * response carries it for anyone who does.
 */
const currencyFormat = new Intl.NumberFormat(undefined, {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 2,
});

export function AnalyticsContainer({ projectId, projectName }: AnalyticsContainerProps): ReactNode {
  const [selectedPreset, setSelectedPreset] = useState<string>('1');
  const [dateFrom, setDateFrom] = useState<Date>(() => presetToDateRange(1, new Date()).from);
  const [dateTo, setDateTo] = useState<Date>(() => new Date());
  const [fromOpen, setFromOpen] = useState(false);
  const [toOpen, setToOpen] = useState(false);
  const [activeTab, setActiveTab] = useState<number>(ANALYTICS_TAB.overview);
  const [pendingUserId, setPendingUserId] = useState<string | null>(null);

  const range = useMemo(() => toIsoRange({ from: dateFrom, to: dateTo }), [dateFrom, dateTo]);

  // Overview and Health both render from the same `projectAnalytics` fetch.
  const needsOverview = activeTab === ANALYTICS_TAB.overview || activeTab === ANALYTICS_TAB.health;
  const { data, isFetching, isError, error } = useProjectAnalyticsQuery(projectId, range, needsOverview);

  // Cost is a SEPARATE endpoint on purpose. `gateway.llm_budget_accumulators`
  // has one reader (/analytics_costs) and that reader carries the rule the
  // figure is wrong without — it sums PROJECT-scope rows only, because the
  // gateway also bills a user scope for the same request and a user-scope row
  // is a subset of its project's spend, not an addition to it. A second view of
  // the same dollars computed by /analytics could disagree with this one, so
  // /analytics does not publish cost at all.
  //
  // This endpoint had NO caller in this app until now: it was missing from
  // endpoints.manifest.json and unreferenced outside generated code, while the
  // Analytics page showed a cost KPI of 0 from the usage endpoint's hardcoded
  // literal.
  //
  // Its failure is deliberately NOT surfaced. It is one tile of a dashboard the
  // rest of which renders fine, and taking down the whole Overview because the
  // money read failed would trade a missing tile for a blank screen — the cost
  // tile is simply absent, which is the same thing it does for a figure the
  // deployment cannot produce.
  const costs = useProjectCostsQuery(projectId, range, needsOverview);
  const totalCost = useMemo(() => {
    const kpis = costs.data?.kpis;
    // GATED ON `spend_available`, not on the presence of `total_cost`.
    //
    // /analytics_costs ALWAYS emits `total_cost` — it is `0.00000000` when the
    // write-back path has persisted nothing — and publishes `spend_available`
    // for exactly this reason, in its own schema's words: so that "no spend
    // yet" stays distinguishable from "no data". Reading only the number
    // renders a COST / $0.00 / billed spend tile for a project whose spend has
    // never been measured, which is the fabricated-zero claim the rest of this
    // screen goes to some length to avoid: the KPI row omits a tile rather than
    // print a zero for an unmeasured figure, and this one would have done the
    // opposite.
    if (kpis === undefined || !kpis.spend_available) return undefined;
    return currencyFormat.format(kpis.total_cost);
  }, [costs.data]);

  const handlePresetChange = useCallback((value: string) => {
    const preset = DATE_FILTER_PRESETS.find((candidate) => candidate.value === value);
    if (preset === undefined) return;
    setSelectedPreset(value);
    const nextRange = presetToDateRange(preset.days, new Date());
    setDateFrom(nextRange.from);
    setDateTo(nextRange.to);
  }, []);

  const handleTabChange = useCallback((_event: SyntheticEvent, newTab: number) => {
    setPendingUserId(null);
    setActiveTab(newTab);
  }, []);

  const handleOverviewUserClick = useCallback((userId: string) => {
    setPendingUserId(userId);
    setActiveTab(ANALYTICS_TAB.users);
  }, []);

  const handleBackToOverview = useCallback(() => {
    setPendingUserId(null);
    setActiveTab(ANALYTICS_TAB.overview);
  }, []);

  return (
    <LocalizationProvider dateAdapter={AdapterDateFns}>
      <Box sx={headerSx}>
        <Typography
          variant="headingSmall"
          color="text.secondary"
        >
          {t('analytics.header.title', 'Analytics')}
        </Typography>
        {projectName !== undefined && projectName !== '' && (
          <Box sx={projectLabelSx}>
            <BriefcaseIcon />
            <Typography variant="bodySmall">
              {t('analytics.header.projectLabel', 'Project: {{name}}', { name: projectName })}
            </Typography>
          </Box>
        )}
      </Box>
      <Box sx={filterBarSx}>
        <TabGroupButton
          items={DATE_FILTER_PRESETS.map((preset) => ({
            value: preset.value,
            label: t(`analytics.datePreset.${preset.value}`, preset.label),
          }))}
          value={selectedPreset}
          onChange={handlePresetChange}
        />
        <Box sx={dateFieldsRowSx}>
          <DateRangeField
            label={t('analytics.dateRange.from', 'From:')}
            value={dateFrom}
            onChange={setDateFrom}
            open={fromOpen}
            onOpen={() => setFromOpen(true)}
            onClose={() => setFromOpen(false)}
            maxDateTime={dateTo}
          />
          <DateRangeField
            label={t('analytics.dateRange.to', 'To:')}
            value={dateTo}
            onChange={setDateTo}
            open={toOpen}
            onOpen={() => setToOpen(true)}
            onClose={() => setToOpen(false)}
            minDateTime={dateFrom}
          />
        </Box>
      </Box>
      <Box sx={shellSx}>
        <Box sx={tabsContainerSx}>
          <BaseTabs
            value={activeTab}
            onChange={handleTabChange}
          >
            {TAB_LABELS.map((tab) => (
              <BaseTab
                key={tab.key}
                label={t(tab.key, tab.fallback)}
              />
            ))}
          </BaseTabs>
        </Box>
        <Box sx={contentAreaSx}>
          <AnalyticsTabContent
            activeTab={activeTab}
            needsOverview={needsOverview}
            isFetching={isFetching}
            isError={isError}
            error={error}
            totalCost={totalCost}
            data={data}
            projectId={projectId}
            dateFrom={range.dateFrom}
            dateTo={range.dateTo}
            pendingUserId={pendingUserId}
            onUserClick={handleOverviewUserClick}
            onBackToSource={handleBackToOverview}
          />
        </Box>
      </Box>
    </LocalizationProvider>
  );
}
