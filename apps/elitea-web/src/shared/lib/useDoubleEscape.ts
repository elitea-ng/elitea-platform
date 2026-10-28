/**
 * Esc Esc: two presses of Escape within {@link DOUBLE_ESCAPE_MS} call `onDouble` (Chat and a Local work thread stop
 * the running turn with it). A single press does nothing here.
 *
 * Escape belongs to whatever is open first. A press that something handled (`defaultPrevented`: the composer's "/"
 * and "@" menus) or that reaches the page while a menu, a dialog or a list box is open does not count, and starts
 * the pair again: the press that closed a menu is never the first of a pair. MUI's menus and dialogs stop Escape
 * before it reaches the window, so the press that closes one is not seen at all.
 *
 * Listens on the window only while `active` (a turn runs), so focus may be anywhere on the page.
 */
import { useEffect, useRef } from 'react';

export const DOUBLE_ESCAPE_MS = 500;

/** Something open owns Escape: a menu, a modal dialog, a list box (a picker, an autocomplete). */
const OPEN_POPUP = '[role="menu"], [role="listbox"], [role="dialog"][aria-modal="true"]';

export function useDoubleEscape(active: boolean, onDouble: () => void): void {
  const latest = useRef(onDouble);
  useEffect(() => {
    latest.current = onDouble;
  });
  useEffect(() => {
    if (!active) return undefined;
    let first: number | null = null;
    const onKeyDown = (event: KeyboardEvent): void => {
      if (event.key !== 'Escape' || event.repeat || event.isComposing) return;
      if (event.defaultPrevented || document.querySelector(OPEN_POPUP) !== null) {
        first = null;
        return;
      }
      const now = Date.now();
      if (first !== null && now - first <= DOUBLE_ESCAPE_MS) {
        first = null;
        latest.current();
        return;
      }
      first = now;
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [active]);
}
