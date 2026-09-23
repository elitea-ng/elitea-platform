import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { NormalizedNotification } from '../api/normalize';
import { NOTIFICATION_DEFAULT_SORT } from '../lib/table';
import { NotificationsTable } from './NotificationsTable';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function row(id: string): NormalizedNotification {
  return {
    id,
    eventType: 'private_project_created',
    createdAt: '2026-01-01T00:00:00Z',
    isSeen: false,
    meta: {},
  };
}

function renderTable(rows: readonly NormalizedNotification[], selectedIds: ReadonlySet<string>) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      <QueryClientProvider client={client}>
        <NotificationsTable
          rows={rows}
          total={rows.length}
          page={1}
          pageSize={5}
          sort={NOTIFICATION_DEFAULT_SORT}
          selectedIds={selectedIds}
          personalProjectId="1"
          onSort={vi.fn()}
          onSelectAll={vi.fn()}
          onSelectRow={vi.fn()}
          onPageChange={vi.fn()}
          onPageSizeChange={vi.fn()}
        />
      </QueryClientProvider>
    </ThemeProvider>,
  );
}

/* onetest: elitea_issues: #4695 — the Select All header checkbox reflects only the CURRENT page's
 * rows against `selectedIds`, so ids left over from a different page (page 1) neither mark a fresh
 * page's header as fully-checked nor drop it straight to unchecked once one row on the new page is
 * selected — it goes to indeterminate, per NotificationsTable's `isAllSelected`/`isIndeterminate`
 * (`rows.every`/`rows.some` scoped to the rendered rows, never a raw `selectedIds.size` count). */
describe('NotificationsTable — select-all across pages (#4695)', () => {
  it('shows the header checkbox as UNCHECKED (not checked) when selectedIds only holds ids from a different page', () => {
    // "page 2" rows — none of their ids are in `selectedIds`, which carries page-1 ids only.
    renderTable([row('20'), row('21')], new Set(['1', '2']));

    const header = screen.getByRole('checkbox', { name: 'Select all' });
    expect(header).not.toBeChecked();
    expect(header).not.toHaveAttribute('data-indeterminate', 'true'); // fully unchecked, not indeterminate either
  });

  it('shows the header checkbox as INDETERMINATE, not fully unchecked, once one row on the new page is selected', () => {
    // selectedIds mixes a stale page-1 id with one row actually on this page.
    renderTable([row('20'), row('21')], new Set(['1', '20']));

    const header = screen.getByRole('checkbox', { name: 'Select all' });
    expect(header).not.toBeChecked();
    expect(header).toHaveAttribute('data-indeterminate', 'true');
  });

  it('shows the header checkbox as fully CHECKED only when every row on the CURRENT page is selected', () => {
    renderTable([row('20'), row('21')], new Set(['20', '21']));

    const header = screen.getByRole('checkbox', { name: 'Select all' });
    expect(header).toBeChecked();
  });
});
