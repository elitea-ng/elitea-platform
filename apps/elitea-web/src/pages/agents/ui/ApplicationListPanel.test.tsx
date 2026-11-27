import { fireEvent, within } from '@testing-library/react';
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
    const { container } = renderWithTheme(<ApplicationListPanel {...BASE_PROPS} rows={[]} isLoading isError={false} />);
    // The grid shows card-shaped skeletons while the first page loads — the
    // "Loading…" text line it used to render belonged to the plain <List>.
    expect(container.querySelectorAll('[data-testid="entity-card-skeleton"]').length).toBeGreaterThan(0);
  });

  it('shows an alert with the error message on error', () => {
    const { getByRole } = renderWithTheme(<ApplicationListPanel {...BASE_PROPS} rows={[]} isLoading={false} isError />);
    expect(getByRole('alert')).toHaveTextContent('Something went wrong.');
  });

  it('shows the empty state when there are no rows', () => {
    const { getByText } = renderWithTheme(
      <ApplicationListPanel {...BASE_PROPS} rows={[]} isLoading={false} isError={false} />,
    );
    expect(getByText('Nothing found.')).toBeInTheDocument();
  });

  it('renders each row and calls onSelect with its id when clicked', () => {
    const onSelect = vi.fn();
    const { getByText } = renderWithTheme(
      <ApplicationListPanel {...BASE_PROPS} rows={ROWS} isLoading={false} isError={false} onSelect={onSelect} />,
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
      <ApplicationListPanel {...BASE_PROPS} rows={ROWS} isLoading={false} isError={false} hasMore={false} />,
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
      <ApplicationListPanel {...BASE_PROPS} rows={ROWS} isLoading={false} isError={false} hasMore isLoadingMore />,
    );
    expect(getByText('Load more').closest('button')).toBeDisabled();
  });

  // #915, updated by the a11y correction: on a CARD the indicator is a
  // MARKER, not a control. The card root is `role="button"`, and a focusable
  // widget nested inside one is axe's `nested-interactive` (impact
  // "serious") — `agents.lifecycle.spec.ts`'s a11y check failed on exactly
  // that. The clickable copy lives in the TABLE row, whose `<tr>` carries no
  // widget role; `EntityCardList.test.tsx` owns both halves directly.
  it('renders a non-interactive "Forked from" marker for a forked row on the card view', () => {
    const onSelect = vi.fn();
    const onForkedFromClick = vi.fn();
    const { queryByRole, queryAllByText } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[{ id: '2', name: 'Forked Agent', description: '', forkedFrom: { onClick: onForkedFromClick } }]}
        isLoading={false}
        isError={false}
        onSelect={onSelect}
      />,
    );
    expect(queryAllByText('Forked from').length).toBeGreaterThan(0);
    expect(queryByRole('link', { name: 'Forked from' })).not.toBeInTheDocument();
  });

  it('renders no "Forked from" link for a row with no forkedFrom', () => {
    const { queryByRole } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[{ id: '2', name: 'Regular Agent', description: '' }]}
        isLoading={false}
        isError={false}
      />,
    );
    expect(queryByRole('link', { name: 'Forked from' })).not.toBeInTheDocument();
  });

  // #1995: the icon an author uploaded must reach the dashboard card.
  it('shows the uploaded icon on the card, and the agent glyph when there is none', () => {
    const { container } = renderWithTheme(
      <ApplicationListPanel
        {...BASE_PROPS}
        rows={[
          { id: '1', name: 'With icon', description: '', iconUrl: '/icons/1/picked.png' },
          { id: '2', name: 'Without icon', description: '' },
        ]}
        isLoading={false}
        isError={false}
      />,
    );
    const images = container.querySelectorAll('img[data-testid="application-card-icon"]');
    expect(images).toHaveLength(1);
    expect(images[0]).toHaveAttribute('src', '/icons/1/picked.png');
  });

  describe('the rich Agents card (issue 3133)', () => {
    const RICH_ROWS: readonly ApplicationListRow[] = [
      { id: '1', name: 'Dev Coordinator', description: 'Leads the team', iconUrl: '/media/coordinator.png', personaRole: 'Coordinator', updatedAt: '2026-11-20T10:00:00Z' },
      { id: '2', name: 'Dev Implementer', description: 'Writes the code' },
    ];

    it('plays the agent\'s own avatar through the live renderer, idle while the card is hovered, and a monogram without one', () => {
      const { getAllByTestId } = renderWithTheme(
        <ApplicationListPanel {...BASE_PROPS} rows={RICH_ROWS} isLoading={false} isError={false} rich={{}} />,
      );
      const [withIcon, without] = getAllByTestId('entity-card') as [HTMLElement, HTMLElement];
      const avatar = within(withIcon).getByTestId('chat-message-avatar');
      expect(avatar).toHaveAttribute('data-motion', 'still');
      fireEvent.pointerEnter(withIcon);
      expect(avatar).toHaveAttribute('data-motion', 'idle');
      fireEvent.pointerLeave(withIcon);
      expect(avatar).toHaveAttribute('data-motion', 'still');
      fireEvent.focusIn(within(withIcon).getByTestId('entity-card-name'));
      expect(avatar).toHaveAttribute('data-motion', 'idle');
      expect(within(withIcon).getByTestId('entity-card-subtitle')).toHaveTextContent('Coordinator');
      expect(within(withIcon).getByTestId('entity-card-description')).toHaveTextContent('Leads the team');
      expect(within(without).getByTestId('monogram')).toHaveTextContent('DI');
      expect(within(without).queryByTestId('chat-message-avatar')).toBeNull();
    });

    it('offers Chat on each card, and waits on the one that is starting', () => {
      const onChat = vi.fn();
      const { getAllByTestId } = renderWithTheme(
        <ApplicationListPanel {...BASE_PROPS} rows={RICH_ROWS} isLoading={false} isError={false} rich={{ onChat, chattingId: '2' }} />,
      );
      const buttons = getAllByTestId('entity-card-primary-action');
      expect(buttons[1]).toBeDisabled();
      fireEvent.click(buttons[0] as HTMLElement);
      expect(onChat).toHaveBeenCalledWith(expect.objectContaining({ id: '1', name: 'Dev Coordinator' }));
    });

    it('keeps the compact tile without the rich option', () => {
      const { getAllByTestId } = renderWithTheme(<ApplicationListPanel {...BASE_PROPS} rows={RICH_ROWS} isLoading={false} isError={false} />);
      expect(getAllByTestId('entity-card')[0]).not.toHaveAttribute('data-variant');
    });
  });
});
