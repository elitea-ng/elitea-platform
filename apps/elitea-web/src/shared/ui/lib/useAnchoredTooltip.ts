import { useCallback, useEffect, useRef, useState, type RefObject } from 'react';

export interface AnchoredTooltip<E extends HTMLElement> {
  /** Spread onto the MUI `Tooltip`. */
  readonly tooltipProps: {
    readonly open: boolean;
    readonly onOpen: () => void;
    readonly onClose: () => void;
    readonly disableInteractive: true;
  };
  /** Put on the element the tooltip describes. */
  readonly anchorRef: RefObject<E | null>;
  /** Call from the element's click handler when the click moves or removes it. */
  readonly close: () => void;
}

function hasKeyboardFocus(element: HTMLElement): boolean {
  try {
    return element.matches(':focus-visible');
  } catch {
    // An engine without `:focus-visible` (older test DOMs): treat as pointer focus.
    return false;
  }
}

/**
 * A tooltip for a button that moves under the pointer (a collapse toggle, a
 * panel's close button). MUI closes a tooltip on `mouseleave`, and an element
 * that moves away from a still pointer never gets one, so the tooltip stayed
 * on screen over whatever the pointer went to next. Here, any pointer move
 * that is not over the anchor closes it, unless the anchor has keyboard focus,
 * and a click closes it until the pointer has left the anchor.
 * Non-interactive, so the open tooltip never takes a click meant for the
 * control under it.
 */
export function useAnchoredTooltip<E extends HTMLElement>(): AnchoredTooltip<E> {
  const [open, setOpen] = useState(false);
  const anchorRef = useRef<E | null>(null);
  // Set by the click: MUI's enter-delay timer, started by the `mouseover` that
  // preceded the click, still fires after it, and the moved button's tooltip
  // then opened with nothing under the pointer. Ignored until the pointer has
  // been somewhere else.
  const suppressed = useRef(false);
  const close = useCallback(() => {
    suppressed.current = true;
    setOpen(false);
  }, []);
  const onOpen = useCallback(() => {
    if (!suppressed.current) setOpen(true);
  }, []);
  const onClose = useCallback(() => setOpen(false), []);

  useEffect(() => {
    const onMove = (event: PointerEvent): void => {
      const anchor = anchorRef.current;
      const overAnchor = anchor !== null && event.target instanceof Node && anchor.contains(event.target);
      if (overAnchor) return;
      suppressed.current = false;
      if (anchor !== null && hasKeyboardFocus(anchor)) return;
      setOpen(false);
    };
    document.addEventListener('pointermove', onMove, true);
    return () => document.removeEventListener('pointermove', onMove, true);
  }, []);

  return {
    tooltipProps: { open, onOpen, onClose, disableInteractive: true },
    anchorRef,
    close,
  };
}
