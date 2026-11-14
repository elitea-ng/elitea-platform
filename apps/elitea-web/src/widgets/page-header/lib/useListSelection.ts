import { useCallback, useMemo, useState } from 'react';

import type { EntitySelection } from '@/shared/ui/EntityCardList';

/**
 * What a list page keeps for multi-select (ADR-0051): the ids picked on the
 * rows currently listed. A row that leaves the list (deleted, filtered out,
 * another page of results) drops out of the selection with it, so the
 * selection section never counts or acts on a row the person cannot see.
 */
interface ListSelection {
  /** What the cards and the table read: `<EntitySelectionProvider value={…}>`. */
  readonly selection: EntitySelection;
  /** The selected ids, in the order the list shows them. */
  readonly ids: readonly string[];
  readonly clear: () => void;
}

export function useListSelection(visibleIds: readonly string[]): ListSelection {
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());

  const ids = useMemo(() => visibleIds.filter((id) => picked.has(id)), [visibleIds, picked]);
  const selected = useMemo<ReadonlySet<string>>(() => new Set(ids), [ids]);

  const toggle = useCallback(
    (id: string) => {
      setPicked((current) => {
        const next = new Set(current);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        return next;
      });
    },
    [],
  );

  const toggleAll = useCallback((all: readonly string[]) => {
    setPicked((current) => {
      const everyOneSelected = all.length > 0 && all.every((id) => current.has(id));
      const next = new Set(current);
      for (const id of all) {
        if (everyOneSelected) next.delete(id);
        else next.add(id);
      }
      return next;
    });
  }, []);

  const clear = useCallback(() => {
    setPicked(new Set());
  }, []);

  const selection = useMemo<EntitySelection>(() => ({ selected, toggle, toggleAll }), [selected, toggle, toggleAll]);
  return { selection, ids, clear };
}
