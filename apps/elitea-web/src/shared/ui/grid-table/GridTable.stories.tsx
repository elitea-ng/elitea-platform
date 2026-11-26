import type { Meta, StoryObj } from '@storybook/react-vite';
import { expect, userEvent, within } from 'storybook/test';

import {
  GridTable,
  GridTableBody,
  GridTableHeader,
  GridTableRow,
  SortComparators,
  useResponsiveColumns,
  useRowSelection,
  useTableSort,
  type GridTableColumn,
} from '.';

interface Secret {
  readonly id: string;
  readonly name: string;
  readonly updated: string;
  readonly owner: string;
}

const ROWS: readonly Secret[] = [
  { id: '1', name: 'github-token', updated: '2026-11-20', owner: 'Platform team' },
  { id: '2', name: 'openai-key-for-the-evaluation-pipeline-in-staging', updated: '2026-10-02', owner: 'AI Run' },
  { id: '3', name: 'jira-bot', updated: '2026-11-25', owner: 'Delivery' },
];

const COLUMNS: readonly GridTableColumn<Secret>[] = [
  { field: 'name', label: 'Name', sortable: true, width: '2fr' },
  { field: 'owner', label: 'Owner' },
  { field: 'updated', label: 'Updated', sortable: true, width: '8rem' },
];

interface DemoProps {
  readonly rows: readonly Secret[];
  readonly selectable: boolean;
}

/** A screen's composition: the hooks own the sort and the selection, the parts draw them. */
function Demo({ rows, selectable }: DemoProps) {
  const { sort, handleSort, sortRows } = useTableSort<Secret>({ comparators: { updated: SortComparators.date } });
  const selection = useRowSelection({ rows, getRowId: (row) => row.id });
  const { visibleColumns, gridTemplateColumns } = useResponsiveColumns({ columns: COLUMNS, showCheckbox: selectable });
  return (
    <GridTable
      isEmpty={rows.length === 0}
      emptyMessage="No secrets"
    >
      <GridTableHeader
        columns={visibleColumns}
        gridTemplateColumns={gridTemplateColumns}
        sort={sort}
        onSort={handleSort}
        selectAll={selectable ? selection.selectAll('Select all secrets') : undefined}
      />
      <GridTableBody>
        {sortRows(rows).map((row) => (
          <GridTableRow
            key={row.id}
            row={row}
            columns={visibleColumns}
            gridTemplateColumns={gridTemplateColumns}
            select={
              selectable
                ? { checked: selection.isSelected(row.id), onChange: () => selection.toggleRow(row.id), label: `Select ${row.name}` }
                : undefined
            }
          />
        ))}
      </GridTableBody>
    </GridTable>
  );
}

const meta = {
  title: 'shared/ui/GridTable',
  component: Demo,
  parameters: { a11y: { test: 'error' } },
  args: { rows: ROWS, selectable: false },
} satisfies Meta<typeof Demo>;

export default meta;
type Story = StoryObj<typeof meta>;

export const Sortable: Story = {
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    const names = () => canvas.getAllByRole('row').slice(1).map((row) => row.firstElementChild?.textContent);
    await expect(names()).toEqual(['github-token', 'jira-bot', 'openai-key-for-the-evaluation-pipeline-in-staging']);
    await userEvent.click(canvas.getByRole('button', { name: 'Name' }));
    await expect(canvas.getByRole('columnheader', { name: 'Name' })).toHaveAttribute('aria-sort', 'descending');
    await expect(names()[0]).toBe('openai-key-for-the-evaluation-pipeline-in-staging');
  },
};

export const Selectable: Story = {
  args: { selectable: true },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await userEvent.click(canvas.getByRole('checkbox', { name: 'Select jira-bot' }));
    await expect(canvas.getByRole('checkbox', { name: 'Select all secrets' })).toHaveAttribute('data-indeterminate', 'true');
    await userEvent.click(canvas.getByRole('checkbox', { name: 'Select all secrets' }));
    await expect(canvas.getByRole('checkbox', { name: 'Select github-token' })).toBeChecked();
  },
};

export const Empty: Story = {
  args: { rows: [] },
  play: async ({ canvasElement }) => {
    const canvas = within(canvasElement);
    await expect(canvas.getByText('No secrets')).toBeInTheDocument();
  },
};
