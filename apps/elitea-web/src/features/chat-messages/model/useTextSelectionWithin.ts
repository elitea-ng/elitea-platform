/**
 * "Has the reader highlighted anything inside THIS element?"
 *
 * The rule itself is `../lib/canvasSelection`'s `selectionTextWithin`; this
 * hook is only the subscription, and the events it subscribes to are the
 * point:
 *
 *  * `selectionchange` on the document is the modern signal and the only one
 *    that fires for a keyboard selection (Shift+Arrow) and for a
 *    double/triple click that never produces a second `mouseup`.
 *  * `mouseup` and `keyup` are kept BESIDE it rather than instead of it.
 *    `selectionchange` is dispatched by the browser's own selection
 *    machinery, and a test environment that implements `Selection` without
 *    dispatching that event would leave this hook permanently empty — a
 *    subscription that never fires is indistinguishable from a feature that
 *    was never wired, which is the failure shape this repository keeps
 *    meeting.
 *
 * Listeners sit on the DOCUMENT, not on the element: a drag that starts inside
 * the answer and ends outside it releases the mouse somewhere else entirely,
 * and a listener bound to the element would never see the release.
 */
import { useCallback, useEffect, useState } from 'react';
import type { RefObject } from 'react';

import { selectionTextWithin } from '../lib/canvasSelection';

export interface TextSelectionWithin {
  /** The highlighted text, or `undefined` when nothing inside the element is selected. */
  readonly selectedText: string | undefined;
  /** Drops the remembered selection — used once the selection has been acted on. */
  readonly clearSelection: () => void;
}

export function useTextSelectionWithin(containerRef: RefObject<HTMLElement | null>): TextSelectionWithin {
  const [selectedText, setSelectedText] = useState<string | undefined>(undefined);

  useEffect(() => {
    const read = (): void => {
      setSelectedText(selectionTextWithin(containerRef.current, document.getSelection()));
    };
    document.addEventListener('selectionchange', read);
    document.addEventListener('mouseup', read);
    document.addEventListener('keyup', read);
    return () => {
      document.removeEventListener('selectionchange', read);
      document.removeEventListener('mouseup', read);
      document.removeEventListener('keyup', read);
    };
  }, [containerRef]);

  const clearSelection = useCallback(() => {
    setSelectedText(undefined);
  }, []);

  return { selectedText, clearSelection };
}
