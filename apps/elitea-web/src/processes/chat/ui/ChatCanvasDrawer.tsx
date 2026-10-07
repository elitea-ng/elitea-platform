/**
 * The transcript canvas editor's drawer (issue 853, #878).
 *
 * Extracted from `ChatWithEditors.tsx` for that file's §3.5 400-line and
 * cyclomatic budgets. Nothing about the drawer changed in the move — every
 * prop it carries, and the reason each one is load-bearing, is documented
 * inline below exactly as it was at the original call site.
 */
import { useEffect, useRef, type ReactNode, type SyntheticEvent } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Drawer from '@mui/material/Drawer';
import LinearProgress from '@mui/material/LinearProgress';

import { CanvasEditor, isEscapeConsumedInside } from '@/features/chat-messages';
import { t } from '@/shared/i18n';

import type { useCanvasEditing } from './useCanvasEditing';

/**
 * Focus goes back to the canvas block the editor was showing.
 *
 * MUI's drawer returns focus to whatever opened it, and for "Open as document"
 * that was the answer's own button — which the carve REMOVED, because the
 * answer became the canvas block. Focus then fell to `<body>` and a keyboard
 * reader had to tab from the top of the page to find their place. The block's
 * open control is the same affordance in its new home, so it takes focus once
 * the drawer is gone (a frame later, after the drawer's own restore ran).
 */
function useFocusClosedCanvasBlock(isOpen: boolean, closedCanvasId: string | undefined): void {
  const wasOpen = useRef(isOpen);
  useEffect(() => {
    const closing = wasOpen.current && !isOpen;
    wasOpen.current = isOpen;
    if (!closing || closedCanvasId === undefined) return undefined;
    const frame = requestAnimationFrame(() => {
      const block = [...document.querySelectorAll<HTMLElement>('[data-testid="canvas-block"][data-canvas-id]')].find(
        (element) => element.dataset['canvasId'] === closedCanvasId,
      );
      block?.querySelector<HTMLElement>('[data-testid="canvas-block-open"]')?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [isOpen, closedCanvasId]);
}

export interface ChatCanvasDrawerProps {
  readonly canvas: ReturnType<typeof useCanvasEditing>;
  /** The signed-in viewer, for the canvas presence roster. */
  readonly viewerId: string | undefined;
}

export function ChatCanvasDrawer({ canvas, viewerId }: ChatCanvasDrawerProps): ReactNode {
  const isOpen = canvas.isEditingCanvas && canvas.selectedCodeBlockInfo !== undefined;
  useFocusClosedCanvasBlock(isOpen, canvas.lastClosedCanvasId);
  return (
    <>
      {/*
        * The canvas editor's mount — `CanvasEditor.tsx` was fully built and
        * rendered by nothing, which is also what made `useEditorMutex`'s
        * save-before-swap branch a no-op (see `useCanvasEditing`).
        *
        * Rendered as a right-hand drawer rather than the baseline's
        * resizable split pane: `react-split` is not a dependency here, and
        * the port's own module doc already states that trade.
        */}
      {isOpen && canvas.selectedCodeBlockInfo !== undefined && (
        <Drawer
          anchor="right"
          open
          /*
           * Escape and a backdrop click close the editor the SAME way its ✕
           * does — through the editor's own `save()`, which hands
           * `onCloseCanvasEditor` the live document, its undo state and its
           * language. This was `() => canvas.onCloseCanvasEditor()`: no
           * arguments, so `hasChange` was undefined and Escape closed the
           * drawer WITHOUT saving — silently dropping every edit, while
           * `lib/canvasEditorKeys` documents Escape as the save-and-close.
           *
           * Never the handler itself: MUI calls `onClose(event, reason)`, and
           * handed that pair directly the close contract would read the event
           * as "there are changes" and `"backdropClick"` as the document. The
           * bare close is only the fallback for an editor not mounted yet.
           */
          onClose={(event: SyntheticEvent, reason: string) => {
            // An Escape a widget INSIDE the editor already used (CodeMirror's
            // search panel or autocomplete, a table cell edit) is that
            // widget's, not the drawer's — see `isEscapeConsumedInside`.
            if (reason === 'escapeKeyDown' && isEscapeConsumedInside(event)) return;
            // A save is in flight; the drawer closes when it lands.
            if (canvas.isSaving) return;
            const editor = canvas.canvasEditorRef.current;
            if (editor !== null) editor.save();
            else canvas.onCloseCanvasEditor();
          }}
          slotProps={{
            paper: {
              sx: { width: { xs: '100%', md: '48rem' }, maxWidth: '100%', p: 2, boxSizing: 'border-box', display: 'flex', flexDirection: 'column', gap: 1 },
            },
          }}
          data-testid="chat-canvas-editor"
        >
          {/*
            * The closing save's state, beside the editor's own close (its
            * header is directly below). A refused save — a document over the
            * server's size cap answers 413 with a sentence for the user —
            * leaves the editor OPEN with the text in it and says why; the
            * user can shorten it and close again, or discard explicitly.
            */}
          {canvas.isSaving && (
            <LinearProgress aria-label={t('processes.chat.canvas.saving', 'Saving the document…')} data-testid="chat-canvas-saving" />
          )}
          {canvas.saveError !== undefined && (
            <Alert
              severity="error"
              role="alert"
              data-testid="chat-canvas-save-error"
              action={
                <Button color="inherit" size="small" onClick={canvas.discardAndClose} data-testid="chat-canvas-discard">
                  {t('processes.chat.canvas.discardAndClose', 'Discard changes and close')}
                </Button>
              }
            >
              {canvas.saveError}
            </Alert>
          )}
          <Box sx={{ flex: 1, minHeight: 0 }}>
            <CanvasEditor
              ref={canvas.canvasEditorRef}
              selectedCodeBlockInfo={canvas.selectedCodeBlockInfo}
              onCloseCanvasEditor={canvas.onCloseCanvasEditor}
              /*
               * The language picker's own write. It was omitted, so
               * `CanvasEditor`'s `onChangeLanguage` guard (`if (editCanvas &&
               * …)`) was permanently false and switching a canvas's language
               * changed the highlighting and nothing else.
               */
              editCanvas={canvas.editCanvas}
              {...(canvas.projectId !== undefined ? { projectId: canvas.projectId } : {})}
              /*
               * Who is looking. It was omitted, and that is not a cosmetic gap:
               * the presence roster the editor reads back from its OWN first
               * beat contains this tab, so an editor with no viewer identity
               * counted itself as "somebody else is editing this canvas" and
               * mounted read-only — CodeMirror with `aria-readonly`, the table
               * grid with every cell disabled. A single user could not type in
               * their own canvas.
               */
              {...(viewerId !== undefined ? { viewer: { id: viewerId } } : {})}
              /*
               * "Save to artifacts" (issue #878) — offered here too, not only
               * on a file-opened canvas: a canvas MADE from a turn has no
               * artifact identity yet, so every save here is a "save as" (no
               * `source` to pre-fill), and — deliberately NOT threaded through
               * `canvas`'s own save (`editCanvas`, the `elitea_core` PUT) — the
               * two are independent actions: this one additionally publishes
               * the document as a browsable file, it does not replace the
               * canvas's own storage.
               */
              {...(canvas.projectId !== undefined ? { saveToArtifacts: { onSaved: () => undefined } } : {})}
            />
          </Box>
        </Drawer>
      )}
    </>
  );
}
