import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ApplicationListPanel, type ApplicationListRow } from './ApplicationListPanel';

const BASE_PROPS = {
  emptyTitle: 'Nothing found.',
  emptyDescription: 'Try a different search.',
  errorMessage: 'Something went wrong.',
  onSelect: () => {},
  hasMore: false,
  isLoadingMore: false,
  onLoadMore: () => {},
};

const ROWS: readonly ApplicationListRow[] = [
  { id: '1', name: 'My Agent', description: 'A helpful agent' },
  { id: '2', name: 'Other Agent', description: 'Another agent' },
];

describe('ApplicationListPanel', () => {
  it('shows card skeletons while loading', () => {
    const { container } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[]}
        isLoading
        isError={false}
      />,
    );
    // The grid shows card-shaped skeletons while the first page loads — the
    // "Loading…" text line it used to render belonged to the plain <List>.
    expect(container.querySelectorAll('[data-testid="entity-card-skeleton"]').length).toBeGreaterThan(0);
  });

  it('shows an alert with the error message on error', () => {
    const { getByRole } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[]}
        isLoading={false}
        isError
      />,
    );
    expect(getByRole('alert')).toHaveTextContent('Something went wrong.');
  });

  it('shows the empty state when there are no rows', () => {
    const { getByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[]}
        isLoading={false}
        isError={false}
      />,
    );
    expect(getByText('Nothing found.')).toBeInTheDocument();
  });

  it('renders each row and calls onSelect with its id when clicked', () => {
    const onSelect = vi.fn();
    const { getByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={ROWS}
        isLoading={false}
        isError={false}
        onSelect={onSelect}
      />,
    );
    expect(getByText('My Agent')).toBeInTheDocument();
    // The card face shows the NAME only (the description rides in its tooltip),
    // which is what `components/Card.jsx` renders.
    expect(getByText('Other Agent')).toBeInTheDocument();

    getByText('My Agent').click();
    expect(onSelect).toHaveBeenCalledWith('1');
  });

  it('does not render "Load more" when hasMore is false', () => {
    const { queryByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={ROWS}
        isLoading={false}
        isError={false}
        hasMore={false}
      />,
    );
    expect(queryByText('Load more')).not.toBeInTheDocument();
  });

  it('renders "Load more" and calls onLoadMore when hasMore is true', () => {
    const onLoadMore = vi.fn();
    const { getByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={ROWS}
        isLoading={false}
        isError={false}
        hasMore
        onLoadMore={onLoadMore}
      />,
    );
    getByText('Load more').click();
    expect(onLoadMore).toHaveBeenCalled();
  });

  it('disables "Load more" while isLoadingMore is true', () => {
    const { getByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={ROWS}
        isLoading={false}
        isError={false}
        hasMore
        isLoadingMore
      />,
    );
    expect(getByText('Load more').closest('button')).toBeDisabled();
  });
});
