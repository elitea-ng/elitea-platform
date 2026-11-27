import { fireEvent, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { EntityCardList } from './EntityCardList';
import { EntityRichCard } from './EntityRichCard';
import type { EntityListItem } from './model';
import { EntityRowSlotProvider, type EntityRowSlot } from './rowSlot';

const AGENT: EntityListItem = {
  id: '7',
  name: 'Dev Coordinator',
  description: 'Leads the dev team and hands work to the others.',
  subtitle: 'Coordinator',
  badge: 'Draft',
  updatedAt: '2026-11-20T10:00:00Z',
  authors: [{ id: 'a', name: 'Ann Lee' }],
  tags: [{ name: 'desktop' }, { name: 'elitea-dev' }, { name: 'ops' }, { name: 'ci' }],
};

function slot(pinned: boolean): EntityRowSlot {
  return {
    columnLabel: 'Folder',
    renderCell: () => null,
    renderCardAction: (item) => (
      <button type="button" aria-label={`Pin ${item.name}`} data-testid="slot-control" onClick={(event) => event.stopPropagation()} />
    ),
    isPinned: () => pinned,
  };
}

describe('EntityRichCard (issue 3133)', () => {
  it('leads with the name and its small status badge, then the persona, the description and a muted footer', () => {
    const { getByTestId, getAllByTestId } = renderWithTheme(<EntityRichCard item={AGENT} />);
    const title = getByTestId('entity-card-name');
    expect(title.tagName).toBe('BUTTON');
    expect(title).toHaveTextContent('Dev Coordinator');
    // The status sits beside the name, not in the footer.
    expect(title.parentElement).toContainElement(getByTestId('entity-card-status'));
    expect(getByTestId('entity-card-status')).toHaveTextContent('Draft');
    expect(getByTestId('entity-card-subtitle')).toHaveTextContent('Coordinator');
    expect(getByTestId('entity-card-description')).toHaveTextContent('Leads the dev team');
    // At most two chips, then "+N".
    expect(getAllByTestId('entity-card-tag-chip').map((chip) => chip.textContent)).toEqual(['desktop', 'elitea-dev']);
    expect(getByTestId('entity-card-tag-overflow')).toHaveTextContent('+2');
    const updated = getByTestId('entity-card-updated');
    expect(updated.tagName).toBe('TIME');
    expect(updated).toHaveAttribute('dateTime', AGENT.updatedAt);
    expect(updated.textContent).toMatch(/^Updated .+ ago$/);
  });

  it('draws a brand-tinted monogram when the agent has no avatar, and the page\'s avatar when it has one', () => {
    const { getByTestId, rerender } = renderWithTheme(<EntityRichCard item={AGENT} />);
    expect(within(getByTestId('entity-card-icon')).getByTestId('monogram')).toHaveTextContent('DC');
    rerender(<EntityRichCard item={{ ...AGENT, icon: <span data-testid="live-avatar" /> }} />);
    expect(within(getByTestId('entity-card-icon')).getByTestId('live-avatar')).toBeInTheDocument();
    expect(within(getByTestId('entity-card-icon')).queryByTestId('monogram')).not.toBeInTheDocument();
  });

  it('omits the time for the zero sentinel and an unparseable value', () => {
    const { queryByTestId, rerender } = renderWithTheme(<EntityRichCard item={{ ...AGENT, updatedAt: '0001-01-01T00:00:00Z' }} />);
    expect(queryByTestId('entity-card-updated')).not.toBeInTheDocument();
    rerender(<EntityRichCard item={{ ...AGENT, updatedAt: 'not a date' }} />);
    expect(queryByTestId('entity-card-updated')).not.toBeInTheDocument();
  });

  it('keeps a filled pin on screen only while the row is pinned', () => {
    const { queryByTestId, rerender } = renderWithTheme(
      <EntityRowSlotProvider value={slot(false)}>
        <EntityRichCard item={AGENT} />
      </EntityRowSlotProvider>,
    );
    expect(queryByTestId('entity-card-pinned')).not.toBeInTheDocument();
    rerender(
      <EntityRowSlotProvider value={slot(true)}>
        <EntityRichCard item={AGENT} />
      </EntityRowSlotProvider>,
    );
    expect(queryByTestId('entity-card-pinned')).toBeInTheDocument();
    expect(queryByTestId('entity-card-pinned')?.querySelector('title')).toHaveTextContent('Pinned');
  });

  it('puts the primary action and the row slot\'s controls in the hover and focus cluster, reachable by keyboard', async () => {
    const onChat = vi.fn();
    const onOpen = vi.fn();
    const { getByTestId, container } = renderWithTheme(
      <EntityRowSlotProvider value={slot(false)}>
        <EntityRichCard item={{ ...AGENT, onClick: onOpen, primaryAction: { label: 'Chat', onClick: onChat } }} />
      </EntityRowSlotProvider>,
    );
    const cluster = container.querySelector('[data-entity-card-actions]');
    expect(cluster).toContainElement(getByTestId('slot-control'));
    expect(cluster).toContainElement(getByTestId('entity-card-primary-action'));
    // The visible word leads the accessible name (WCAG 2.5.3), and the agent is named.
    expect(getByTestId('entity-card-primary-action')).toHaveAccessibleName('Chat Dev Coordinator');
    // Tab order: the card's button, then the cluster's controls; none of them is hidden from it.
    await userEvent.tab();
    expect(getByTestId('entity-card-name')).toHaveFocus();
    await userEvent.tab();
    expect(getByTestId('slot-control')).toHaveFocus();
    await userEvent.tab();
    expect(getByTestId('entity-card-primary-action')).toHaveFocus();
    await userEvent.keyboard('{Enter}');
    expect(onChat).toHaveBeenCalledTimes(1);
    // A click on an action never opens the card behind it.
    expect(onOpen).not.toHaveBeenCalled();
    fireEvent.click(getByTestId('entity-card-name'));
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it('draws no cluster when there is nothing to act on', () => {
    const { container } = renderWithTheme(<EntityRichCard item={AGENT} />);
    expect(container.querySelector('[data-entity-card-actions]')).toBeNull();
  });

  it('is what the grid draws for the rich variant, with taller skeletons while loading', () => {
    const { getAllByTestId, rerender } = renderWithTheme(
      <EntityCardList
        items={[AGENT, { ...AGENT, id: '8', name: 'Dev Implementer' }]}
        variant="rich"
        view="cards"
      />,
    );
    expect(getAllByTestId('entity-card').map((card) => card.getAttribute('data-variant'))).toEqual(['rich', 'rich']);
    rerender(
      <EntityCardList
        items={[]}
        isLoading
        variant="rich"
      />,
    );
    expect(getAllByTestId('entity-card-skeleton').length).toBeGreaterThan(0);
  });
});
