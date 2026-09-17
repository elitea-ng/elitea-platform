/**
 * The transcript canvas editor's drawer (issue 853, #878).
 *
 * Extracted from `ChatWithEditors.tsx` for that file's §3.5 400-line and
 * cyclomatic budgets. Nothing about the drawer changed in the move — every
 * prop it carries, and the reason each one is load-bearing, is documented
 * inline below exactly as it was at the original call site.
 */
import type { ReactNode } from 'react';

import Drawer from '@mui/material/Drawer';

import { CanvasEditor } from '@/features/chat-messages';

import type { useCanvasEditing } from './useCanvasEditing';

export interface ChatCanvasDrawerProps {
  readonly canvas: ReturnType<typeof useCanvasEditing>;
  /** The signed-in viewer, for the canvas presence roster. */
  readonly viewerId: string | undefined;
}

export function ChatCanvasDrawer({ canvas, viewerId }: ChatCanvasDrawerProps): ReactNode {
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
      {canvas.isEditingCanvas && canvas.selectedCodeBlockInfo !== undefined && (
        <Drawer
          anchor="right"
          open
          /*
           * `() => …`, not the handler itself. MUI calls `onClose(event,
           * reason)`, and `onCloseCanvasEditor`'s own contract is
           * `(hasChange, finalResult, language)` — handed the pair directly it
           * would read the event as "there are changes" and the string
           * `"backdropClick"` as the document to save, and write that over the
           * user's canvas.
           */
          onClose={() => canvas.onCloseCanvasEditor()}
          slotProps={{ paper: { sx: { width: { xs: '100%', md: '48rem' }, maxWidth: '100%', p: 2, boxSizing: 'border-box' } } }}
          data-testid="chat-canvas-editor"
        >
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
        </Drawer>
      )}
    </>
  );
}
