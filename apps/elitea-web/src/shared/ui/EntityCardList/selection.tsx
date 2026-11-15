import { createContext, useContext, type MouseEvent } from 'react';

/**
 * A page-wide multi-select over the rows below it (ADR-0051, the Workshop's
 * selection section). A list page wraps its list in the provider; the cards
 * draw a checkbox and take ⌘-click (Ctrl-click on Windows and Linux), the table
 * gets a checkbox column and a select-all header. `shared/ui` knows nothing
 * about what the page does with the selection.
 */
export interface EntitySelection {
  readonly selected: ReadonlySet<string>;
  readonly toggle: (id: string) => void;
  /** Selects every id in `ids`, or clears them when all of them are selected already. */
  readonly toggleAll: (ids: readonly string[]) => void;
}

const SelectionContext = createContext<EntitySelection | undefined>(undefined);

export const EntitySelectionProvider = SelectionContext.Provider;

export function useEntitySelection(): EntitySelection | undefined {
  return useContext(SelectionContext);
}

/** ⌘-click on a card (Ctrl-click where there is no ⌘) toggles it instead of opening it. */
function isSelectionClick(event: { readonly metaKey: boolean; readonly ctrlKey: boolean }): boolean {
  return event.metaKey || event.ctrlKey;
}

/** What one row needs of the selection: whether it is on, a toggle, and the click that opens it (or selects it). */
interface SelectableRow {
  readonly selectable: boolean;
  readonly selected: boolean;
  readonly toggle: () => void;
  readonly activate: (event: MouseEvent) => void;
}

/**
 * A row's click: ⌘-click selects it, and so does a plain click while others
 * are selected, so a run of rows is picked without the modifier. With no
 * selection in scope the click only opens the row.
 */
export function useSelectableRow(id: string, open: (() => void) | undefined): SelectableRow {
  const selection = useEntitySelection();
  const toggle = (): void => selection?.toggle(id);
  const activate = (event: MouseEvent): void => {
    if (selection !== undefined && (isSelectionClick(event) || selection.selected.size > 0)) toggle();
    else open?.();
  };
  return { selectable: selection !== undefined, selected: selection?.selected.has(id) === true, toggle, activate };
}
