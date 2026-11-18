import { useCallback, useEffect, useMemo, useState } from 'react';

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
  /** After a bulk action: when it failed on some rows, only those rows stay selected; when it failed on none, nothing changes. */
  readonly settle: (result: { readonly failed: readonly string[] }) => void;
}

export function useListSelection(visibleIds: readonly string[]): ListSelection {
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());

  const ids = useMemo(() => visibleIds.filter((id) => picked.has(id)), [visibleIds, picked]);

  // A row that left the list (deleted, filtered out) is dropped for good, so it is not selected again when it returns.
  // An empty list is a load in progress, not a list without rows: it prunes nothing.
  useEffect(() => {
    if (visibleIds.length === 0) return;
    const visible = new Set(visibleIds);
    setPicked((current) => (current.size === 0 || [...current].every((id) => visible.has(id)) ? current : new Set([...current].filter((id) => visible.has(id)))));
  }, [visibleIds]);
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

  const settle = useCallback((result: { readonly failed: readonly string[] }) => {
    if (result.failed.length > 0) setPicked(new Set(result.failed));
  }, []);

  const selection = useMemo<EntitySelection>(() => ({ selected, toggle, toggleAll }), [selected, toggle, toggleAll]);
  return { selection, ids, clear, settle };
}
