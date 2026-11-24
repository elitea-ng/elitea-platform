/**
 * "Schedules and tasks" is one console item (ADR-0059 decision 6, R3.2, issue
 * 2532) over two pages: `/schedules` (the recurring routines) and `/tasks`
 * (what is running now). Both paths keep working; this strip, above either
 * page, links the two, so the one item reaches both.
 *
 * `./AdminConsoleOutlet` mounts it under the six-area navigation only. With
 * the `admin_console_areas` switch off, the two pages keep their own
 * navigation items and look as they did.
 */
import type { ReactNode } from 'react';

import { Link } from '@tanstack/react-router';

import Tab from '@mui/material/Tab';
import Tabs from '@mui/material/Tabs';

import { t } from '@/shared/i18n';

export interface AdminSchedulesTasksTabsProps {
  readonly current: '/schedules' | '/tasks';
}

const tabSx = { textTransform: 'none', minHeight: '2.5rem' } as const;

export function AdminSchedulesTasksTabs({ current }: AdminSchedulesTasksTabsProps): ReactNode {
  return (
    <Tabs
      value={current}
      aria-label={t('pages.admin.console.item.schedules-tasks', 'Schedules and tasks')}
      data-testid="admin-schedules-tasks-tabs"
      sx={{ minHeight: '2.5rem', flexShrink: 0, paddingInline: '1.5rem' }}
    >
      <Tab
        component={Link}
        to="/schedules"
        value="/schedules"
        label={t('pages.admin.schedulesTasks.tab.schedules', 'Schedules')}
        data-testid="admin-schedules-tasks-tab-schedules"
        sx={tabSx}
      />
      <Tab
        component={Link}
        to="/tasks"
        value="/tasks"
        label={t('pages.admin.nav.tasks', 'Tasks')}
        data-testid="admin-schedules-tasks-tab-tasks"
        sx={tabSx}
      />
    </Tabs>
  );
}
