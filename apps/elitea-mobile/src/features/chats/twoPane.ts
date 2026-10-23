import { useCallback, useState } from 'react';
import { useWindowDimensions, type LayoutChangeEvent } from 'react-native';

/**
 * Regular width (issue #15): at and above this width the Chats tab shows the list and the open
 * chat side by side; below it, the phone stack (the list, a chat pushed on top).
 *
 * UIKit's horizontal size class cannot be read from JS, so this is a width. 700 pt keeps every
 * iPad's full screen and 2/3 Split View two-pane (the narrowest, iPad mini portrait, is 744),
 * and every 1/3 and most 1/2 splits on the phone stack. Erring narrow is safe: the phone stack
 * works at any width, and below ~700 pt the list and the chat would both be cramped.
 */
export const TWO_PANE_MIN_WIDTH = 700;

export function isTwoPane(width: number): boolean {
  return width >= TWO_PANE_MIN_WIDTH;
}

/** The list column: about a third of the width, 320–400 pt (the chat keeps at least 380). */
export function listPaneWidth(width: number): number {
  return Math.round(Math.min(400, Math.max(320, width * 0.36)));
}

/**
 * The width the screen actually has. The window's width until the screen has been laid out,
 * then its own (an iPad tab sidebar, Stage Manager): `measured` says which.
 */
export function useScreenWidth(): { width: number; measured: boolean; onLayout: (event: LayoutChangeEvent) => void } {
  const window = useWindowDimensions();
  const [own, setOwn] = useState<number | null>(null);
  const onLayout = useCallback((event: LayoutChangeEvent) => setOwn(Math.round(event.nativeEvent.layout.width)), []);
  return { width: own ?? window.width, measured: own !== null, onLayout };
}
