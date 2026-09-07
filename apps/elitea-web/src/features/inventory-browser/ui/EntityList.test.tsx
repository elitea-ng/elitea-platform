/**
 * The entities a filter matched, and which one is open.
 *
 * The SELECTED row is the one the detail pane shows, not the one that was
 * clicked. After a filter changes the two differ — the open entity may not be
 * in the new list — and a list that highlighted the click would show nothing
 * selected while the pane still described something. The `selected` assertion
 * below is what holds that.
 *
 * The one-line description also drops its empty parts: joining the raw fields
 * renders `class · · code ·` for a node an older ingestion left half-described,
 * which reads as a damaged row rather than an incompletely recorded one.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { InventoryEntity } from '@/entities/inventory';

import { renderWithProviders } from '../__tests__/testUtils';
import { EntityList } from './EntityList';

function entity(overrides: Partial<InventoryEntity> = {}): InventoryEntity {
  return {
    id: 'code:checkout-service',
    name: 'CheckoutService',
    type: 'class',
    layer: 'application',
    sourceToolkit: 'code',
    filePath: 'src/checkout/service.py',
    ...overrides,
  };
}

function show(overrides: Partial<React.ComponentProps<typeof EntityList>> = {}) {
  const onSelect = vi.fn();
  renderWithProviders(
    <EntityList
      entities={[entity()]}
      selectedId={null}
      isPending={false}
      error={null}
      onSelect={onSelect}
      {...overrides}
    />,
  );
  return onSelect;
}

describe('EntityList', () => {
  it('says it is reading rather than showing an empty graph', () => {
    show({ isPending: true, entities: [] });
    expect(screen.getByTestId('inventory-entities-pending')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-entities-empty')).not.toBeInTheDocument();
  });

  it('shows the provider own sentence instead of the list when a read failed', () => {
    show({ error: 'No graph has been built for bucket graphs.' });
    expect(screen.getByTestId('inventory-entities-error')).toHaveTextContent(
      'No graph has been built for bucket graphs.',
    );
    expect(screen.queryByTestId('inventory-entity-list')).not.toBeInTheDocument();
  });

  it('names the FILTER as the thing to change on an empty result', () => {
    show({ entities: [] });
    expect(screen.getByTestId('inventory-entities-empty')).toHaveTextContent(/matches the current filter/);
  });

  it('describes each row without the fields the graph did not record', () => {
    show({ entities: [entity({ layer: '', filePath: '' })] });
    const row = screen.getByTestId('inventory-entity-row');
    expect(within(row).getByText('CheckoutService')).toBeInTheDocument();
    expect(within(row).getByText('class · code')).toBeInTheDocument();
  });

  it('highlights the OPEN entity, not the one that was clicked', async () => {
    const user = userEvent.setup();
    const onSelect = show({
      entities: [entity(), entity({ id: 'code:place-order', name: 'placeOrder' })],
      selectedId: 'code:checkout-service',
    });
    const rows = screen.getAllByTestId('inventory-entity-row');
    expect(rows[0]).toHaveClass('Mui-selected');
    expect(rows[1]).not.toHaveClass('Mui-selected');

    // Clicking raises the choice; it does not move the highlight on its own.
    await user.click(rows[1] as HTMLElement);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: 'code:place-order' }));
    expect(screen.getAllByTestId('inventory-entity-row')[0]).toHaveClass('Mui-selected');
  });

  it('addresses each row by its entity id', () => {
    show();
    expect(screen.getByTestId('inventory-entity-row')).toHaveAttribute(
      'data-entity-id',
      'code:checkout-service',
    );
  });
});
