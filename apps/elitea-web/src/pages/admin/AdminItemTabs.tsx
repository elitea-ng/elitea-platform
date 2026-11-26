/**
 * The tab strips of a console item that spans more than one page (ADR-0059
 * decision 6). Each page keeps its own path, and the strip above it links
 * the item's pages, so the one navigation item reaches all of them:
 *
 *  - "Schedules and tasks" (R3.2, issue 2532): `/schedules` (the recurring
 *    routines) and `/tasks` (what is running now);
 *  - Usage (R3.5a, issue 2535): `/models/usage` and the gateway's Status,
 *    `/models/status`, which decision 6 gives no item of its own;
 *  - "Budgets and alerts" (R3.5a): `/budgets` and `/budgets/alerts`.
 *
 * `./AdminConsoleOutlet` mounts a strip under the six-area navigation only.
 * With the `admin_console_areas` switch off, each page looks as it did.
 */
import type { ReactNode } from 'react';

import { Link } from '@tanstack/react-router';

import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

import { t } from '@/shared/i18n';

interface ItemTab {
  /** The route id the tab opens. */
  readonly path: string;
  /** The tab's test id suffix. */
  readonly key: string;
  readonly label: () => string;
}

export interface ItemTabStrip {
  /** The test id stem: `admin-<id>-tabs`, `admin-<id>-tab-<key>`. */
  readonly id: string;
  readonly label: () => string;
  readonly tabs: readonly ItemTab[];
}

const ITEM_TAB_STRIPS: readonly ItemTabStrip[] = [
  {
    id: 'schedules-tasks',
    label: () => t('pages.admin.console.item.schedules-tasks', 'Schedules and tasks'),
    tabs: [
      { path: '/schedules', key: 'schedules', label: () => t('pages.admin.schedulesTasks.tab.schedules', 'Schedules') },
      { path: '/tasks', key: 'tasks', label: () => t('pages.admin.nav.tasks', 'Tasks') },
    ],
  },
  {
    id: 'usage',
    label: () => t('pages.admin.console.item.usage', 'Usage'),
    tabs: [
      { path: '/models/usage', key: 'usage', label: () => t('pages.admin.llmProxy.tab.usage', 'Usage') },
      { path: '/models/status', key: 'status', label: () => t('pages.admin.llmProxy.tab.status', 'Status') },
    ],
  },
  {
    id: 'budgets-alerts',
    label: () => t('pages.admin.console.item.budgets-alerts', 'Budgets and alerts'),
    tabs: [
      { path: '/budgets', key: 'budgets', label: () => t('pages.admin.nav.budgets', 'Budgets') },
      { path: '/budgets/alerts', key: 'alerts', label: () => t('pages.admin.llmProxy.tab.alerts', 'Budget alerts') },
    ],
  },
];

/** The strip of the matched leaf route, when its item spans pages. */
export function itemTabStripFor(leaf: string | undefined): ItemTabStrip | undefined {
  return ITEM_TAB_STRIPS.find((strip) => strip.tabs.some((tab) => tab.path === leaf));
}

export interface AdminItemTabsProps {
  readonly strip: ItemTabStrip;
  /** The matched leaf route: the selected tab. */
  readonly current: string;
}

const tabSx = { textTransform: 'none', minHeight: '2.5rem' } as const;

export function AdminItemTabs({ strip, current }: AdminItemTabsProps): ReactNode {
  return (
    <Tabs
      value={current}
      aria-label={strip.label()}
      data-testid={`admin-${strip.id}-tabs`}
      sx={{ minHeight: '2.5rem', flexShrink: 0, paddingInline: '1.5rem' }}
    >
      {strip.tabs.map((tab) => (
        <Tab
          key={tab.path}
          component={Link}
          to={tab.path}
          value={tab.path}
          label={tab.label()}
          data-testid={`admin-${strip.id}-tab-${tab.key}`}
          sx={tabSx}
        />
      ))}
    </Tabs>
  );
}
