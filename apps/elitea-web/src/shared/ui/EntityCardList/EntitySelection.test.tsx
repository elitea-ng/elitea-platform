import { useState } from 'react';

import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { EntityCard } from './EntityCard';
import { EntityListTable } from './EntityListTable';
import type { EntityListItem } from './model';
import { EntitySelectionProvider, type EntitySelection } from './selection';

const ITEMS: readonly EntityListItem[] = [
  { id: '1', name: 'Alpha', badge: 'Draft', createdAt: '2026-03-14T10:00:00Z' },
  { id: '2', name: 'Beta', badge: 'Published', createdAt: '2026-03-15T10:00:00Z' },
];

/** A page's selection state, as `useListSelection` keeps it. */
function Harness({ children, onOpen }: { readonly children: (open: () => void) => React.ReactNode; readonly onOpen?: () => void }): React.ReactNode {
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const selection: EntitySelection = {
    selected: picked,
    toggle: (id) => setPicked((current) => (current.has(id) ? new Set([...current].filter((value) => value !== id)) : new Set([...current, id]))),
    toggleAll: (ids) => setPicked((current) => (ids.every((id) => current.has(id)) ? new Set() : new Set(ids))),
  };
  return (
    <EntitySelectionProvider value={selection}>
      <div data-testid="count">{picked.size}</div>
      {children(onOpen ?? (() => undefined))}
    </EntitySelectionProvider>
  );
}

describe('EntityCard selection and badge', () => {
  it('shows the status badge as text', () => {
    const { getByTestId } = renderWithTheme(<EntityCard item={ITEMS[0] as EntityListItem} />);
    expect(getByTestId('entity-card-status')).toHaveTextContent('Draft');
  });

  it('opens the card on a plain click and selects it on ⌘-click', async () => {
    const onClick = vi.fn();
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <Harness>{() => <EntityCard item={{ ...(ITEMS[0] as EntityListItem), onClick }} />}</Harness>,
    );

    await user.click(getByTestId('entity-card-name'));
    expect(onClick).toHaveBeenCalledTimes(1);
    expect(getByTestId('count')).toHaveTextContent('0');

    await user.keyboard('{Meta>}');
    await user.click(getByTestId('entity-card-name'));
    await user.keyboard('{/Meta}');
    expect(onClick).toHaveBeenCalledTimes(1);
    expect(getByTestId('count')).toHaveTextContent('1');
    expect(getByTestId('entity-card')).toHaveAttribute('data-selected', 'true');
  });

  it('toggles a card with its checkbox, and a plain click then adds to the selection instead of opening', async () => {
    const onClick = vi.fn();
    const user = userEvent.setup();
    const { getAllByRole, getAllByTestId, getByTestId } = renderWithTheme(
      <Harness>
        {() => (
          <>
            <EntityCard item={{ ...(ITEMS[0] as EntityListItem), onClick }} />
            <EntityCard item={{ ...(ITEMS[1] as EntityListItem), onClick }} />
          </>
        )}
      </Harness>,
    );

    await user.click(getAllByRole('checkbox')[0] as HTMLElement);
    expect(getByTestId('count')).toHaveTextContent('1');

    await user.click(getAllByTestId('entity-card-name')[1] as HTMLElement);
    expect(onClick).not.toHaveBeenCalled();
    expect(getByTestId('count')).toHaveTextContent('2');
  });
});

describe('EntityListTable selection and status column', () => {
  it('draws a Status column when a row has a badge', () => {
    const { getAllByTestId } = renderWithTheme(<EntityListTable items={ITEMS} />);
    expect(getAllByTestId('entity-row-status').map((cell) => cell.textContent)).toEqual(expect.arrayContaining(['Draft', 'Published']));
  });

  it('selects a row with its checkbox and every row with the header checkbox', async () => {
    const user = userEvent.setup();
    const { getAllByTestId, getByTestId } = renderWithTheme(<Harness>{() => <EntityListTable items={ITEMS} />}</Harness>);

    await user.click(getAllByTestId('entity-row-select')[0]?.querySelector('input') as HTMLElement);
    expect(getByTestId('count')).toHaveTextContent('1');

    await user.click(getByTestId('entity-list-select-all').querySelector('input') as HTMLElement);
    expect(getByTestId('count')).toHaveTextContent('2');

    await user.click(getByTestId('entity-list-select-all').querySelector('input') as HTMLElement);
    expect(getByTestId('count')).toHaveTextContent('0');
  });
});
