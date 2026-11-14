import { createContext, useContext, type ReactNode } from 'react';

import type { EntityListItem } from './model';

/**
 * A page-wide slot into every row of the lists below it: the card's top-right
 * corner and one extra table column. A list page wraps its list in the
 * provider and the cards and the table pick the slot up, so a feature that
 * touches every row (the project folders of issue 2048) does not have to be
 * threaded through each page's own row mapping. `shared/ui` knows nothing
 * about what the slot draws.
 */
export interface EntityRowSlot {
  /** The extra table column's header. */
  readonly columnLabel: string;
  /** The extra table column's cell for one row. */
  readonly renderCell: (item: EntityListItem) => ReactNode;
  /** The card's top-right control for one card. */
  readonly renderCardAction: (item: EntityListItem) => ReactNode;
}

const RowSlotContext = createContext<EntityRowSlot | undefined>(undefined);

export const EntityRowSlotProvider = RowSlotContext.Provider;

export function useEntityRowSlot(): EntityRowSlot | undefined {
  return useContext(RowSlotContext);
}
