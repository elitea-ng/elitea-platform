import { act, fireEvent, renderHook } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import {
  GridTable,
  GridTableBody,
  GridTableHeader,
  GridTableRow,
  SortComparators,
  responsiveColumns,
  useRowSelection,
  useTableSort,
  type GridTableColumn,
} from '.';

interface Token {
  readonly id: string;
  readonly name: string;
  readonly expires: string | null;
}

const ROWS: readonly Token[] = [
  { id: 'a', name: 'beta', expires: '2027-01-01' },
  { id: 'b', name: 'Alpha', expires: null },
  { id: 'c', name: 'gamma', expires: '2026-12-01' },
];

const COLUMNS: readonly GridTableColumn<Token>[] = [
  { field: 'name', label: 'Name', sortable: true, width: '2fr' },
  { field: 'expires', label: 'Expires', sortable: true },
  { field: 'actions', label: 'Actions', width: '5rem', align: 'right', render: (row) => <button type="button">Delete {row.name}</button> },
];

function Table({ rows = ROWS, onSort = vi.fn() }: { rows?: readonly Token[]; onSort?: (field: string) => void }) {
  const { gridTemplateColumns } = responsiveColumns(COLUMNS, 1200, false);
  return (
    <GridTable
      isEmpty={rows.length === 0}
      emptyMessage="No tokens"
    >
      <GridTableHeader
        columns={COLUMNS}
        gridTemplateColumns={gridTemplateColumns}
        sort={{ field: 'name', direction: 'desc' }}
        onSort={onSort}
      />
      <GridTableBody>
        {rows.map((row) => (
          <GridTableRow
            key={row.id}
            row={row}
            columns={COLUMNS}
            gridTemplateColumns={gridTemplateColumns}
          />
        ))}
      </GridTableBody>
    </GridTable>
  );
}

describe('GridTable', () => {
  it('renders a table a screen reader reads: column headers, rows and cells', () => {
    const { getAllByRole, getByRole } = renderWithTheme(<Table />);
    expect(getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual(['Name', 'Expires', 'Actions']);
    expect(getAllByRole('row')).toHaveLength(4);
    expect(getByRole('button', { name: 'Delete beta' })).toBeInTheDocument();
  });

  it('shows "-" for an empty value and the empty message for no rows', () => {
    const { getAllByRole, unmount } = renderWithTheme(<Table />);
    expect(getAllByRole('cell').map((cell) => cell.textContent)).toContain('-');
    unmount();
    const empty = renderWithTheme(<Table rows={[]} />);
    expect(empty.getByText('No tokens')).toBeInTheDocument();
    expect(empty.queryByRole('table')).toBeNull();
  });

  it('marks the sorted column with aria-sort and sorts by a button', () => {
    const onSort = vi.fn();
    const { getByRole } = renderWithTheme(<Table onSort={onSort} />);
    expect(getByRole('columnheader', { name: 'Name' })).toHaveAttribute('aria-sort', 'descending');
    expect(getByRole('columnheader', { name: 'Expires' })).toHaveAttribute('aria-sort', 'none');
    expect(getByRole('columnheader', { name: 'Actions' }).querySelector('button')).toBeNull();
    fireEvent.click(getByRole('button', { name: 'Expires' }));
    expect(onSort).toHaveBeenCalledWith('expires');
  });

  it('pads a cell on the spacing scale', () => {
    const { getAllByRole } = renderWithTheme(<Table />);
    const cell = getComputedStyle(getAllByRole('cell')[0] as HTMLElement);
    // jsdom keeps the theme's value: steps 1 and 2 (8 and 16 px in comfortable
    // density, 6 and 12 in compact); the theme writes step 1 without calc().
    expect([cell.paddingTop, cell.paddingRight]).toEqual(['var(--el-spacing)', 'calc(2 * var(--el-spacing))']);
  });
});

describe('useTableSort', () => {
  it('sorts text without case, flips on the same column, and starts another column ascending', () => {
    const { result } = renderHook(() => useTableSort<Token>({ comparators: { expires: SortComparators.date } }));
    expect(result.current.sortRows(ROWS).map((row) => row.name)).toEqual(['Alpha', 'beta', 'gamma']);
    act(() => {
      result.current.handleSort('name');
    });
    expect(result.current.sort).toEqual({ field: 'name', direction: 'desc' });
    expect(result.current.sortRows(ROWS).map((row) => row.name)).toEqual(['gamma', 'beta', 'Alpha']);
    act(() => {
      result.current.handleSort('expires');
    });
    // A token that never expires reads as time 0, so it comes first when ascending.
    expect(result.current.sortRows(ROWS).map((row) => row.id)).toEqual(['b', 'c', 'a']);
  });

  it('puts an empty value last when ascending and first when descending', () => {
    const rows = [{ v: null }, { v: 2 }, { v: 1 }];
    const { result } = renderHook(() => useTableSort<{ v: number | null }>({ defaultField: 'v' }));
    expect(result.current.sortRows(rows).map((row) => row.v)).toEqual([1, 2, null]);
    act(() => {
      result.current.handleSort('v');
    });
    expect(result.current.sortRows(rows).map((row) => row.v)).toEqual([null, 2, 1]);
  });

  it('compares numbers given as text', () => {
    expect(SortComparators.number('10', 9)).toBeGreaterThan(0);
    expect(SortComparators.number('x', 0)).toBe(0);
    expect(SortComparators.date('not a date', '')).toBe(0);
  });
});

describe('useRowSelection', () => {
  it('selects all rows of the page, keeps other pages, and reports a partial selection', () => {
    const getRowId = (row: Token) => row.id;
    const { result, rerender } = renderHook(({ rows }) => useRowSelection({ rows, getRowId, initialSelected: ['z'] }), {
      initialProps: { rows: ROWS },
    });
    act(() => {
      result.current.toggleRow('a');
    });
    expect(result.current.selectAll('Select all')).toMatchObject({ checked: false, indeterminate: true });
    act(() => {
      result.current.toggleAll();
    });
    expect(result.current.selectedIds).toEqual(['z', 'a', 'b', 'c']);
    expect(result.current.selectAll('Select all').checked).toBe(true);
    act(() => {
      result.current.toggleAll();
    });
    expect(result.current.selectedIds).toEqual(['z']);
    rerender({ rows: [] });
    expect(result.current.selectAll('Select all').checked).toBe(false);
    act(() => {
      result.current.clearSelection();
    });
    expect(result.current.isSelected('z')).toBe(false);
  });
});

describe('responsiveColumns', () => {
  it('hides a column below its width and builds the grid template', () => {
    const columns: GridTableColumn<Token>[] = [
      { field: 'name', label: 'Name', width: '2fr' },
      { field: 'expires', label: 'Expires', hideBelow: 900 },
    ];
    expect(responsiveColumns(columns, 1200, true).gridTemplateColumns).toBe('3rem 2fr 1fr');
    expect(responsiveColumns(columns, 800, false)).toMatchObject({ gridTemplateColumns: '2fr' });
  });
});
