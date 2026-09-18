import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { PipelineListPanel, type PipelineListRow } from './PipelineListPanel';

const BASE_PROPS = {
  emptyTitle: 'Nothing found.',
  emptyDescription: 'Try a different search.',
  errorMessage: 'Something went wrong.',
  onSelect: () => {},
  hasMore: false,
  isLoadingMore: false,
  onLoadMore: () => {},
};

const ROWS: readonly PipelineListRow[] = [
  { id: '1', name: 'My Pipeline', description: 'A helpful pipeline' },
  { id: '2', name: 'Other Pipeline', description: 'Another pipeline' },
];

describe('PipelineListPanel', () => {
  it('shows card skeletons while loading', () => {
    const { container } = renderWithTheme(
      <PipelineListPanel
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
      <PipelineListPanel
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
      <PipelineListPanel
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
      <PipelineListPanel
        {...BASE_PROPS}
        rows={ROWS}
        isLoading={false}
        isError={false}
        onSelect={onSelect}
      />,
    );
    expect(getByText('My Pipeline')).toBeInTheDocument();
    // The card face shows the NAME only (the description rides in its tooltip),
    // which is what `components/Card.jsx` renders.
    expect(getByText('Other Pipeline')).toBeInTheDocument();

    getByText('My Pipeline').click();
    expect(onSelect).toHaveBeenCalledWith('1');
  });

  it('does not render "Load more" when hasMore is false', () => {
    const { queryByText } = renderWithTheme(
      <PipelineListPanel
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
      <PipelineListPanel
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
      <PipelineListPanel
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

  // #915 — same "Forked from" wiring as `pages/agents/ui/ApplicationListPanel`.
  it('renders a "Forked from" link for a forked row and routes clicks through row.forkedFrom', () => {
    const onSelect = vi.fn();
    const onForkedFromClick = vi.fn();
    const { getByRole } = renderWithTheme(
      <PipelineListPanel
        {...BASE_PROPS}
        rows={[{ id: '2', name: 'Forked Pipeline', description: '', forkedFrom: { onClick: onForkedFromClick } }]}
        isLoading={false}
        isError={false}
        onSelect={onSelect}
      />,
    );
    getByRole('link', { name: 'Forked from' }).click();
    expect(onForkedFromClick).toHaveBeenCalledTimes(1);
    expect(onSelect).not.toHaveBeenCalled();
  });
});
