/**
 * The WORDS of an answer, and the two places they can arrive from.
 *
 * Split out of `ApplicationAnswer.tsx` when the canvas CREATE gesture landed:
 * that file was one line under the §3.5 400-line budget, and this cluster is
 * the part of it with its own subject — everything here answers "what did the
 * assistant say, and what can the reader do with a piece of it".
 *
 * ── WHY THERE ARE TWO BRANCHES AND NOT ONE ────────────────────────────────
 * `answer.content` and the TEXT items are two spellings of the same words, and
 * which one arrives depends on the read: the paginated messages route — the
 * one the chat page actually uses — collapses a group's text into `content`
 * and serves no text item, while the conversation-details read serves the
 * items. So `content` is rendered only when no TEXT item states it. Gating
 * that on `items.length` instead (canvases included) would drop the words of
 * an answer whose only item on that route is its canvas.
 *
 * That is also why the selection affordance is attached to BOTH branches. A
 * create gesture wired only to the item list would be dead on the very route
 * the page reads — the "both halves correct, the composition root empty"
 * shape this repository keeps meeting.
 */
import type { ReactNode } from 'react';
import { useCallback, useMemo } from 'react';

import { AnswerMessageItems } from './AnswerMessageItems';
import type { AnswerItem } from './AnswerMessageItems';
import { SelectableAnswerText } from './SelectableAnswerText';
import type { CanvasEditPayload, CodeBlockInfo } from '../canvas/Canvas';

/** What a highlighted range in an answer says about itself. */
export interface AnswerCanvasSelection {
  /**
   * The answering message group's UUID — the transcript's own identity for it.
   *
   * Not the row id the create route takes: `entities/message` normalises a
   * group's `id` to its uuid, and inventing a number here would be a guess.
   * The resolver at the composition root looks the group up instead.
   */
  readonly messageGroupUuid: string;
  /** The highlighted text, exactly as the reader selected it. */
  readonly selectedText: string;
  /** The stored text item the selection came from, when this row's read named one. */
  readonly messageItemId: number | undefined;
  /**
   * Issue #879: forces the created canvas's kind rather than letting
   * `canvasKindForSelection` guess from the text shape. The "Open as
   * document" answer action sets this to `'document'` unconditionally — it
   * is carving out the WHOLE answer as prose, not asking the heuristic to
   * look at it. The selection-drag affordance leaves this `undefined` and
   * gets the heuristic's guess.
   */
  readonly kind?: 'code' | 'document' | undefined;
}

export interface AnswerContentProps {
  /** The group's collapsed text, as the paginated route serves it. */
  readonly content: string;
  /** The group's stored items, as the details read serves them. */
  readonly items: readonly AnswerItem[];
  /** This answer's message group UUID, stamped onto every selection payload. */
  readonly messageGroupUuid: string;
  readonly isStreaming?: boolean;
  readonly spokenRange?: { readonly start: number; readonly end: number } | undefined;
  readonly onEditCanvas?: ((payload: CanvasEditPayload) => void) | undefined;
  readonly selectedCodeBlockInfo?: CodeBlockInfo | undefined;
  readonly onCreateCanvasFromSelection?: ((payload: AnswerCanvasSelection) => void) | undefined;
}

export function AnswerContent({
  content,
  items,
  messageGroupUuid,
  isStreaming = false,
  spokenRange,
  onEditCanvas,
  selectedCodeBlockInfo,
  onCreateCanvasFromSelection,
}: AnswerContentProps): ReactNode {
  const hasTextItems = useMemo(() => items.some((item) => item.kind === 'text'), [items]);

  /* The item knows the words; this row knows which answer they belong to. */
  const onSelectionCanvas = useCallback(
    (payload: { readonly selectedText: string; readonly messageItemId: number | undefined }) => {
      onCreateCanvasFromSelection?.({ ...payload, messageGroupUuid });
    },
    [messageGroupUuid, onCreateCanvasFromSelection],
  );

  // A streaming answer is still being written, so a range carved out of it
  // would name offsets that move under the carve.
  const selectionHandler = onCreateCanvasFromSelection !== undefined && !isStreaming ? onSelectionCanvas : undefined;

  return (
    <>
      {!!content && !hasTextItems && (
        <SelectableAnswerText
          content={content}
          messageItemId={undefined}
          spokenRange={spokenRange}
          {...(selectionHandler !== undefined ? { onCreateCanvas: selectionHandler } : {})}
        />
      )}

      {items.length > 0 && (
        <AnswerMessageItems
          items={items}
          isStreaming={isStreaming}
          onEditCanvas={onEditCanvas}
          selectedCodeBlockInfo={selectedCodeBlockInfo}
          spokenRange={spokenRange}
          {...(selectionHandler !== undefined ? { onCreateCanvasFromSelection: selectionHandler } : {})}
        />
      )}
    </>
  );
}
