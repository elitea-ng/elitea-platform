/**
 * A menu button's open handlers, as a native menu button behaves: the menu is
 * on screen when the mouse button goes down, not after it comes back up (the
 * press itself is 80–150ms the person would otherwise wait through). The
 * click still opens it for the keyboard (Return, Space) and assistive tech;
 * after a mouse press it finds the menu open already and changes nothing.
 * The release lands on the menu's backdrop, so it neither closes the menu nor
 * clicks whatever is under the button.
 */
import type { MouseEvent, PointerEvent } from 'react';

export interface PressToOpenProps {
  readonly onPointerDown: (event: PointerEvent<HTMLElement>) => void;
  readonly onClick: (event: MouseEvent<HTMLElement>) => void;
}

export function pressToOpen(open: (anchor: HTMLElement) => void): PressToOpenProps {
  return {
    onPointerDown: (event) => {
      if (event.pointerType !== 'mouse' || event.button !== 0) return;
      // The press's own focus would land on the button after the menu took it.
      event.preventDefault();
      open(event.currentTarget);
    },
    onClick: (event) => open(event.currentTarget),
  };
}
