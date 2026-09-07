/**
 * One entity, and the edges that reach it.
 *
 * The DIRECTION is what makes a relation readable: "calls" and "is called by"
 * are opposite facts, and the provider reports them as one relation type with a
 * direction beside it. Dropping the direction — which the first version of this
 * pane did — renders a caller and a callee identically, and nothing on screen
 * says which way round the edge goes.
 *
 * A neighbour is also a LINK, not a label. If the click stops opening the
 * entity, the pane silently becomes a table with extra steps and the graph
 * cannot be walked at all.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { InventoryEntity, InventoryNeighbour } from '@/entities/inventory';

import { renderWithProviders } from '../__tests__/testUtils';
import { EntityDetail } from './EntityDetail';

const ENTITY: InventoryEntity = {
  id: 'code:checkout-service',
  name: 'CheckoutService',
  type: 'class',
  layer: 'application',
  sourceToolkit: 'code',
  filePath: 'src/checkout/service.py',
};

const NEIGHBOURS: readonly InventoryNeighbour[] = [
  { entityId: 'code:place-order', relationType: 'defines', direction: 'outgoing' },
  { entityId: 'docs:checkout', relationType: 'documents', direction: 'incoming' },
  { entityId: 'code:cart', relationType: '', direction: '' },
];

function show(overrides: Partial<React.ComponentProps<typeof EntityDetail>> = {}) {
  const onOpenNeighbour = vi.fn();
  renderWithProviders(
    <EntityDetail
      entity={ENTITY}
      neighbours={NEIGHBOURS}
      isPending={false}
      error={null}
      onOpenNeighbour={onOpenNeighbour}
      {...overrides}
    />,
  );
  return onOpenNeighbour;
}

describe('EntityDetail', () => {
  it('says it is reading rather than claiming nothing is selected', () => {
    show({ isPending: true, entity: undefined });
    expect(screen.getByTestId('inventory-entity-pending')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-entity-none')).not.toBeInTheDocument();
  });

  it('shows the provider own sentence for a refusal', () => {
    // Nearly always `resource_not_found` for an id that is in a listing and not
    // in the loaded graph — and only the provider can say which graph it read.
    show({ error: "Entity 'code:nope' is not in this graph." });
    expect(screen.getByTestId('inventory-entity-error')).toHaveTextContent(
      "Entity 'code:nope' is not in this graph.",
    );
  });

  it('invites a selection when nothing is open', () => {
    show({ entity: undefined });
    expect(screen.getByTestId('inventory-entity-none')).toBeInTheDocument();
  });

  it('describes the entity, its id and where it lives', () => {
    show();
    expect(screen.getByText('CheckoutService')).toBeInTheDocument();
    expect(screen.getByTestId('inventory-entity-id')).toHaveTextContent('code:checkout-service');
    expect(screen.getByTestId('inventory-entity-file')).toHaveTextContent('src/checkout/service.py');
  });

  it('drops the chips and the file row the graph did not record', () => {
    show({ entity: { ...ENTITY, type: '', layer: '', sourceToolkit: '', filePath: '' } });
    expect(screen.queryByTestId('inventory-entity-file')).not.toBeInTheDocument();
    expect(screen.getByTestId('inventory-entity-id')).toBeInTheDocument();
  });

  it('reads each edge WITH its direction', () => {
    show();
    const relations = screen.getByTestId('inventory-entity-relations');
    expect(within(relations).getByText(/→ defines/)).toBeInTheDocument();
    expect(within(relations).getByText(/← documents/)).toBeInTheDocument();
    // A relation the provider did not name still reads as a relation.
    expect(within(relations).getByText(/related/)).toBeInTheDocument();
  });

  it('opens the entity a neighbour names', async () => {
    const user = userEvent.setup();
    const onOpenNeighbour = show();
    await user.click(screen.getAllByTestId('inventory-neighbour')[1] as HTMLElement);
    expect(onOpenNeighbour).toHaveBeenCalledWith('docs:checkout');
  });

  it('says so when nothing in the graph links to the entity', () => {
    show({ neighbours: [] });
    expect(screen.getByTestId('inventory-entity-no-relations')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-neighbour')).not.toBeInTheDocument();
  });
});
