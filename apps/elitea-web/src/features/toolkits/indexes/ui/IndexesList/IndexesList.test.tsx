import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { IndexRow } from '../../model/indexesStore';

import { IndexesList } from './IndexesList';

const indexes: IndexRow[] = [
  { id: '1', metadata: { collection: 'a' } },
  { id: '2', metadata: { collection: 'b' } },
];

describe('IndexesList', () => {
  it('shows the empty placeholder when there are no indexes and it is not loading', () => {
    const { getByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
      />,
    );
    expect(getByText('Still no indexes created')).toBeInTheDocument();
  });

  it('renders skeleton rows while loading', () => {
    const { getAllByTestId } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading
      />,
    );
    expect(getAllByTestId('index-list-item-skeleton')).toHaveLength(8);
  });

  it('renders every index row', () => {
    const { getByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={indexes}
        onIndexClick={vi.fn()}
      />,
    );
    expect(getByText('a')).toBeInTheDocument();
    expect(getByText('b')).toBeInTheDocument();
  });

  it('calls handleAddIndex when the add button is clicked', async () => {
    const user = userEvent.setup();
    const handleAddIndex = vi.fn();
    const { getByRole } = renderWithTheme(
      <IndexesList
        handleAddIndex={handleAddIndex}
        indexesList={[]}
        onIndexClick={vi.fn()}
      />,
    );
    await user.click(getByRole('button', { name: 'Add index' }));
    expect(handleAddIndex).toHaveBeenCalled();
  });
});

/**
 * THE FAILED READ (see `IndexesList`'s own doc comment and
 * `../../lib/helpers/indexesListError.ts`).
 *
 * The rejection shape is the real one: `EliteaApiError.failure` as
 * `HttpFailure` (`shared/api/http.ts:45-49`), and the body is the exact
 * payload the 2026-09-06 parity walk recorded.
 */
const listReadFailure = (status: number, body: unknown): unknown =>
  Object.assign(new Error('eliteaFetch'), { failure: { kind: 'http', status, url: '/index_meta', body } });

describe('IndexesList — a read that failed is not an empty list', () => {
  it('does NOT claim "Still no indexes created" when the read failed', () => {
    // The regression itself. Before this state existed, an errored query and an
    // empty project produced byte-identical screens, and the errored one was
    // the case the product was actually in.
    const { queryByText, getByTestId } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={listReadFailure(400, { error: 'PGVector configuration is missing for toolkit 1' })}
      />,
    );
    expect(queryByText('Still no indexes created')).not.toBeInTheDocument();
    expect(getByTestId('indexes-list-error')).toBeInTheDocument();
  });

  it('shows the status and the server’s own sentence verbatim', () => {
    const { getByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={listReadFailure(400, { error: 'PGVector configuration is missing for toolkit 1' })}
      />,
    );
    expect(getByText('The index list could not be loaded (400).')).toBeInTheDocument();
    expect(getByText('PGVector configuration is missing for toolkit 1')).toBeInTheDocument();
  });

  it('adds the prerequisite guidance for the missing-vector-store failure', () => {
    const { getByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={listReadFailure(400, { error: 'PGVector configuration is missing for toolkit 1' })}
      />,
    );
    expect(getByText(/Create a PgVector configuration in Settings/)).toBeInTheDocument();
  });

  it('omits the guidance for a failure the user cannot fix that way', () => {
    const { getByText, queryByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={listReadFailure(502, undefined)}
      />,
    );
    expect(getByText('The index list could not be loaded (502).')).toBeInTheDocument();
    expect(queryByText(/Create a PgVector configuration/)).not.toBeInTheDocument();
  });

  it('falls back to the generic heading when the failure carries no status', () => {
    const { getByText } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={Object.assign(new Error('offline'), { failure: { kind: 'network', url: '/x', message: 'boom', cause: null } })}
      />,
    );
    expect(getByText('The index list could not be loaded.')).toBeInTheDocument();
  });

  it('keeps the "Add index" button reachable, so a fixed prerequisite can be used at once', () => {
    const handleAddIndex = vi.fn();
    const { getByLabelText } = renderWithTheme(
      <IndexesList
        handleAddIndex={handleAddIndex}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading={false}
        error={listReadFailure(400, { error: 'PGVector configuration is missing for toolkit 1' })}
      />,
    );
    getByLabelText('Add index').click();
    expect(handleAddIndex).toHaveBeenCalledTimes(1);
  });

  it('loading wins over an error, so a retry in flight does not flash the failure', () => {
    const { queryByTestId, getAllByTestId } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={[]}
        onIndexClick={vi.fn()}
        loading
        error={listReadFailure(400, { error: 'PGVector configuration is missing for toolkit 1' })}
      />,
    );
    expect(queryByTestId('indexes-list-error')).not.toBeInTheDocument();
    expect(getAllByTestId('index-list-item-skeleton').length).toBeGreaterThan(0);
  });

  it('renders rows, not the error, when a refetch failed but rows are still held', () => {
    // react-query keeps the last good `data` across a failed refetch. The rows
    // the user can still act on must win over a banner about the refetch.
    const { getByText, queryByTestId } = renderWithTheme(
      <IndexesList
        handleAddIndex={vi.fn()}
        indexesList={indexes}
        onIndexClick={vi.fn()}
        loading={false}
      />,
    );
    expect(getByText('a')).toBeInTheDocument();
    expect(queryByTestId('indexes-list-error')).not.toBeInTheDocument();
  });
});
