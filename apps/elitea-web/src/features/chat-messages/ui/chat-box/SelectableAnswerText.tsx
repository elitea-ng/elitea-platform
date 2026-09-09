/**
 * One text item of an answer, with the gesture that turns part of it into a
 * canvas.
 *
 * ── WHY THIS EXISTS ───────────────────────────────────────────────────────
 * The canvas half of this app could OPEN, EDIT and SAVE a canvas and could not
 * MAKE one: the create route was reachable only from a test's own HTTP client,
 * and the streaming journey said so in its header. A canvas is carved out of a
 * RANGE of a stored answer, so the gesture that creates one is a selection —
 * the reader highlights the part of the answer they want to work on and asks
 * for it as a document.
 *
 * ── WHAT THE REFERENCE ACTUALLY HAD, STATED RATHER THAN IMPLIED ───────────
 * The reference app offered creation from two places and neither is a
 * selection: a fenced block's own "edit in canvas" control (the whole block,
 * whose range the markdown tokeniser already knows), and a whole-message
 * "edit response" (range `0 … length`). It reads no DOM selection anywhere.
 * So this control is NOT a port of a reference control — it is the same
 * product gesture reached the way this transcript can reach it, and the range
 * contract it sends is the reference's own, byte for byte
 * (`../../lib/canvasSelection` states the byte rule and why it matters).
 *
 * ── THE AFFORDANCE FLOATS, BUT IT IS NOT POSITIONED AT THE CARET ──────────
 * It is pinned to the top-right of the text item rather than to the selection
 * rectangle. `Range.getBoundingClientRect` is the only way to find that
 * rectangle, and it is one of the layout APIs a jsdom test environment does
 * not implement — a control positioned by it could not be proven to appear at
 * all in the composition-root test, and an unproven control is what this whole
 * area keeps producing. A fixed anchor needs no layout read, and the reader
 * still gets a control that appears on selection and vanishes with it.
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo, useRef } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';
import { Markdown } from '@/shared/ui/Markdown';

import { useTextSelectionWithin } from '../../model/useTextSelectionWithin';

/** What the create needs to know about the highlighted words. */
export interface CanvasSelectionPayload {
  /** The highlighted text, exactly as the reader selected it. */
  readonly selectedText: string;
  /** The stored item the selection came from, when the read that fed this row named one. */
  readonly messageItemId: number | undefined;
}

export interface SelectableAnswerTextProps {
  /** The item's markdown. */
  readonly content: string;
  /** The stored `message_items` row id, when this row's read supplies it. */
  readonly messageItemId?: number | undefined;
  /**
   * Carves the highlighted range out of this item. Omitted — a surface with no
   * canvas editor mounted, or a streaming answer — no affordance is rendered
   * at all, rather than one that leads nowhere.
   */
  readonly onCreateCanvas?: ((payload: CanvasSelectionPayload) => void) | undefined;
  /** The word TTS is reading. */
  readonly spokenRange?: { readonly start: number; readonly end: number } | undefined;
}

export function SelectableAnswerText({
  content,
  messageItemId,
  onCreateCanvas,
  spokenRange,
}: SelectableAnswerTextProps): ReactNode {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const { selectedText, clearSelection } = useTextSelectionWithin(containerRef);

  /*
   * THE RENDERED ANSWER MUST NOT BE REBUILT WHEN THE SELECTION CHANGES.
   *
   * A DOM selection is anchored to actual text NODES. Noticing a selection
   * sets state here, and re-rendering the markdown replaces those nodes — so
   * the browser collapses the very selection that had just been made, the next
   * read sees nothing, and the control disappears the instant after it
   * appears. Measured, not theorised: without this memo the button rendered
   * and `document.getSelection()` was already empty on the same tick.
   *
   * Holding the element identity stable makes React skip that subtree
   * entirely, so the text nodes — and the selection over them — survive.
   */
  const rendered = useMemo(() => <Markdown spokenRange={spokenRange}>{content}</Markdown>, [content, spokenRange]);

  /**
   * THE PRESS MUST NOT DESTROY WHAT IT ACTS ON.
   *
   * Pressing the mouse down on a control collapses the document selection —
   * that is the browser's own behaviour for a plain mousedown, and
   * `@testing-library/user-event` emulates it faithfully. So a "do this to the
   * selection" button that does not refuse the default has already destroyed
   * its own subject before its `click` handler runs: the selection reads as
   * empty, this component re-renders without the control, and the click lands
   * on a node that is no longer in the document. Nothing happens and nothing
   * says why.
   *
   * Refusing the default on mousedown is what every floating selection toolbar
   * does, and it is the whole reason this control works at all.
   */
  const onMouseDown = useCallback((event: React.MouseEvent) => {
    event.preventDefault();
  }, []);

  const onClick = useCallback(() => {
    if (selectedText === undefined) return;
    onCreateCanvas?.({ selectedText, messageItemId });
    // The highlight is dropped as soon as it has been acted on: the create
    // REWRITES this item into text / canvas / text, so the selection this
    // control still held would name an item id that no longer exists.
    clearSelection();
    document.getSelection()?.removeAllRanges();
  }, [clearSelection, messageItemId, onCreateCanvas, selectedText]);

  const offersCanvas = onCreateCanvas !== undefined && selectedText !== undefined;

  return (
    <Box ref={containerRef} sx={{ position: 'relative', width: '100%' }} data-testid="answer-text-item">
      {rendered}
      {offersCanvas && (
        <Button
          size="small"
          variant="contained"
          onClick={onClick}
          onMouseDown={onMouseDown}
          data-testid="canvas-create-from-selection"
          sx={{ position: 'absolute', top: 0, right: 0, zIndex: 1, textTransform: 'none' }}
        >
          {t('features.chatMessages.canvas.selection.create', 'Create canvas')}
        </Button>
      )}
    </Box>
  );
}
