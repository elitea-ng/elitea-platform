import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { useIndexesStore } from '../../model/indexesStore';

import { IndexHistory } from './IndexHistory';
import type { IndexHistoryItem } from './IndexHistory';

beforeEach(() => {
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
});

const history: IndexHistoryItem[] = [
  { state: 'completed', updated_on: 1_700_000_000, conversation_id: 'c1' },
  { state: 'failed', updated_on: 1_700_100_000, conversation_id: 'c2' },
  { state: 'partly_indexed', updated_on: 1_700_050_000, conversation_id: 'c3' },
];

describe('IndexHistory', () => {
  /* elitea_issues: #6391 — Run History must auto-select the most recent run when it opens.
   * The backend's `history` array is contract-ordered oldest-first / newest-last (see
   * `services/elitea-main/internal/application/indexmeta/list_history_bound_test.go`'s
   * "the view shows the newest run first" — bound keeps the trailing/newest entries), so the
   * newest run is always the LAST element. This fixture uses non-monotonic timestamps
   * deliberately: the assertion is about which array position is selected, matching that
   * contract, not about re-deriving "latest" from timestamps client-side. */
  it('selects the last history item on mount and clears selection on unmount', () => {
    const { unmount } = renderWithTheme(<IndexHistory history={history} />);
    expect(useIndexesStore.getState().selectedHistoryItem).toEqual(history[2]);
    unmount();
    expect(useIndexesStore.getState().selectedHistoryItem).toBeNull();
  });

  it('renders every history row with its label and formatted date', () => {
    const { getByText } = renderWithTheme(<IndexHistory history={history} />);
    expect(getByText('Reindexed')).toBeInTheDocument();
    expect(getByText('Failed')).toBeInTheDocument();
    expect(getByText('Partially Indexed')).toBeInTheDocument();
  });

  it('sorts by date descending by default, and toggles direction on repeated click', async () => {
    const user = userEvent.setup();
    const { getByText, getAllByText } = renderWithTheme(<IndexHistory history={history} />);

    // Default: date desc -> Failed (1_700_100_000) first, then Partially Indexed, then Reindexed.
    const rowsDesc = getAllByText(/Reindexed|Failed|Partially Indexed/);
    expect(rowsDesc[0]).toHaveTextContent('Failed');

    await user.click(getByText('Date'));
    const rowsAsc = getAllByText(/Reindexed|Failed|Partially Indexed/);
    expect(rowsAsc[0]).toHaveTextContent('Reindexed');
  });

  it('clicking a row selects it', async () => {
    const user = userEvent.setup();
    const { getByText } = renderWithTheme(<IndexHistory history={history} />);
    await user.click(getByText('Failed'));
    expect(useIndexesStore.getState().selectedHistoryItem).toEqual(history[1]);
  });
});

/**
 * The report pane. Its data is already on every history row (the index_meta
 * `metadata` map is copied verbatim out of PgVector), so this is a render of
 * data the tab was previously throwing away.
 */
describe('IndexHistory indexing report', () => {
  it('shows the selected run\'s outcome breakdown, and follows the selection', async () => {
    const user = userEvent.setup();
    const runs: IndexHistoryItem[] = [
      { state: 'created', updated_on: 1_700_000_000, conversation_id: 'c1', indexed: 5 },
      {
        state: 'partly_indexed',
        updated_on: 1_700_100_000,
        conversation_id: 'c2',
        indexed: 9,
        skipped: { documents_skipped: { filtered_count: 2, filtered: ['x.md'] } },
      },
    ];
    const { getByText, getByTestId, queryByText } = renderWithTheme(<IndexHistory history={runs} />);
    // Mounted selection is the LAST row.
    expect(getByTestId('indexing-report-summary')).toBeInTheDocument();
    expect(getByText(/2 documents skipped/)).toBeInTheDocument();

    // Click by the row's EVENT label — the date column is formatted in the
    // runner's local timezone, so asserting on it would be machine-dependent.
    await user.click(getByText('Created'));
    expect(queryByText(/2 documents skipped/)).toBeNull();
    expect(getByText(/5 documents indexed/)).toBeInTheDocument();
  });

  it('surfaces the run error exactly once — the legacy path folds it into the report already', () => {
    const runs: IndexHistoryItem[] = [
      { state: 'failed', updated_on: 1_700_000_000, conversation_id: 'c1', indexed: 0, error: 'the source went away' },
    ];
    const { getAllByText } = renderWithTheme(<IndexHistory history={runs} />);
    expect(getAllByText('the source went away')).toHaveLength(1);
  });

  it('shows a stored error above a canonical report that lists none of its own', () => {
    const runs: IndexHistoryItem[] = [
      {
        state: 'partly_indexed',
        updated_on: 1_700_000_000,
        conversation_id: 'c1',
        error: 'the source went away',
        report: { status: 'partly_indexed', totals: { indexed: 1 }, categories: [{ kind: 'indexed', count: 1, groups: [] }] },
      },
    ];
    const { getAllByText } = renderWithTheme(<IndexHistory history={runs} />);
    expect(getAllByText('the source went away')).toHaveLength(1);
  });
});
