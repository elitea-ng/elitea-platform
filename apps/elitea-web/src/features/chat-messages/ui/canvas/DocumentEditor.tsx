/**
 * ui/canvas/DocumentEditor.tsx — the `document` canvas pane (issue #879): a
 * rich-text editor for prose, with Markdown as the source of truth.
 *
 * ── WHY THIS FILE IS LAZY-LOADED ────────────────────────────────────────
 * `@tiptap/react` + `@tiptap/starter-kit` + `@tiptap/extension-table` +
 * `tiptap-markdown` pull in ProseMirror's whole editing engine — real
 * weight, and one only a canvas that is actually a `document` ever needs.
 * `./CanvasEditor.tsx` imports this module via `React.lazy(() =>
 * import('./DocumentEditor'))`, so the code/table/mermaid path (the common
 * case) never fetches this chunk at all. Nothing in THIS file needs to know
 * that — `React.lazy`/`Suspense` is entirely the caller's concern — but it is
 * why this stays its own module rather than a branch inside
 * `CanvasEditor.tsx` itself.
 *
 * ── CONTENT MODEL ─────────────────────────────────────────────────────────
 * Markdown in, Markdown out. `tiptap-markdown`'s `Markdown` extension patches
 * `editor.commands.setContent` to accept a Markdown string (parsed through
 * `markdown-it`) and adds `editor.storage.markdown.getMarkdown()` to read the
 * live document back out as Markdown — so `getCode`/`setCode` below never
 * touch HTML directly, matching every other canvas pane's contract (`code`
 * and `table` already speak Markdown/plain text over their `getCode`/
 * `setCode`).
 *
 * Tables: `@tiptap/extension-table`'s node is literally named `table` (with
 * `tableRow`/`tableCell`/`tableHeader` children), and `tiptap-markdown` ships
 * a serializer keyed on that exact name — a *first-row-of-`tableHeader`,
 * everything-else-`tableCell`* table round-trips as a GFM pipe table with NO
 * extra code here; anything more exotic (merged cells, multi-paragraph
 * cells) falls back to an embedded raw-HTML block instead of being dropped
 * (`html: true` below). Verified empirically in
 * `./DocumentEditor.test.tsx` — this is the one part of this port worth
 * distrusting until it is seen actually running, since it depends on that
 * naming coincidence rather than a documented contract.
 *
 * ── THE IMPERATIVE SURFACE ──────────────────────────────────────────────
 * Same shape as `./table/MarkdownTableEditor.tsx`'s handle
 * (`undo`/`redo`/`getCode`/`setCode`) — `CanvasEditor`'s `activeEditor()`
 * already dispatches to whichever pane is mounted through exactly these
 * members, so a THIRD pane only has to match the existing two, not teach the
 * host a new shape.
 */
import { forwardRef, useEffect, useImperativeHandle, useMemo, useRef, useState } from 'react';

import { Box } from '@mui/material';
import type { EditorEvents } from '@tiptap/core';
import { EditorContent, useEditor } from '@tiptap/react';
import StarterKit from '@tiptap/starter-kit';
import { TableKit } from '@tiptap/extension-table';
import { Markdown } from 'tiptap-markdown';

import { DocumentEditorToolbar } from './DocumentEditorToolbar';

/** The imperative surface `CanvasEditor` drives through its editor ref — matches `MarkdownTableEditorHandle`/`CodeMirrorEditorHandle`'s `undo`/`redo`/`getCode`(/`setCode`) shape. */
export interface DocumentEditorHandle {
  readonly undo: () => void;
  readonly redo: () => void;
  /** The live document as Markdown — the same "read the live pane, not a debounced mirror" contract `CanvasEditor`'s Copy/Save already rely on for the other two panes. */
  readonly getCode: () => string;
  /** Replaces the whole document from Markdown (canvas sync, quick-fix-style external write). */
  readonly setCode: (markdown: string) => void;
}

export interface DocumentEditorProps {
  readonly content: {
    readonly initialMarkdown: string;
    readonly onChange?: ((markdown: string) => void) | undefined;
  };
  readonly history?: {
    readonly onCanUndo?: ((canUndo: boolean) => void) | undefined;
    readonly onCanRedo?: ((canRedo: boolean) => void) | undefined;
  };
  readonly readOnly?: boolean | undefined;
  readonly 'aria-label'?: string | undefined;
}

/** Matches `CodeMirrorEditor`'s own debounce — one write-back per typing pause, not one per keystroke. */
const CHANGE_DEBOUNCE_MS = 30;

const DocumentEditorImpl = forwardRef<DocumentEditorHandle, DocumentEditorProps>(
  function DocumentEditor({ content, history, readOnly = false, 'aria-label': ariaLabel }, ref) {
    // Only the FIRST markdown matters, same rationale as `MarkdownTableEditor`'s
    // `initialMarkdownData`: later external writes arrive through `setCode` on
    // the ref (canvas sync), not by this prop changing.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    const initialMarkdown = useMemo(() => content.initialMarkdown, []);

    const extensions = useMemo(
      () => [
        StarterKit,
        TableKit.configure({ table: { resizable: false } }),
        Markdown.configure({ html: true, transformPastedText: true, transformCopiedText: true, linkify: true }),
      ],
      [],
    );

    const onChangeRef = useRef(content.onChange);
    onChangeRef.current = content.onChange;
    const debounceRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

    // Same bail-out-when-unchanged rule `CanvasEditor`'s own history callbacks
    // use, and for the identical reason: a setter that always returns a new
    // object defeats React's re-render skip, and this fires on every
    // transaction (every keystroke).
    const [, setHistoryTick] = useState(0);
    const lastHistory = useRef({ canUndo: false, canRedo: false });

    const editor = useEditor({
      extensions,
      content: initialMarkdown,
      editable: !readOnly,
      immediatelyRender: false,
      onUpdate: ({ editor: current }: EditorEvents['update']) => {
        if (debounceRef.current !== undefined) clearTimeout(debounceRef.current);
        debounceRef.current = setTimeout(() => {
          onChangeRef.current?.(current.storage.markdown.getMarkdown());
        }, CHANGE_DEBOUNCE_MS);
      },
      onTransaction: ({ editor: current }: EditorEvents['transaction']) => {
        const canUndo = current.can().undo();
        const canRedo = current.can().redo();
        if (canUndo === lastHistory.current.canUndo && canRedo === lastHistory.current.canRedo) return;
        lastHistory.current = { canUndo, canRedo };
        history?.onCanUndo?.(canUndo);
        history?.onCanRedo?.(canRedo);
        // Forces this component to re-render so the toolbar's `isActive(...)`
        // reads (bold/heading/list/…) reflect the transaction that just ran —
        // `useEditor` already does this itself by default, but the tick also
        // covers the case where only OUR ref, not editor identity, changed.
        setHistoryTick((tick) => tick + 1);
      },
    });

    useEffect(() => {
      editor?.setEditable(!readOnly);
    }, [editor, readOnly]);

    useEffect(
      () => () => {
        if (debounceRef.current !== undefined) clearTimeout(debounceRef.current);
      },
      [],
    );

    useImperativeHandle(
      ref,
      () => ({
        undo: () => editor?.chain().focus().undo().run(),
        redo: () => editor?.chain().focus().redo().run(),
        getCode: () => editor?.storage.markdown.getMarkdown() ?? initialMarkdown,
        setCode: (markdown: string) => {
          editor?.commands.setContent(markdown);
        },
      }),
      [editor, initialMarkdown],
    );

    return (
      <Box
        data-testid="canvas-document-editor"
        sx={{ display: 'flex', flexDirection: 'column', height: '100%', width: '100%', overflow: 'hidden' }}
      >
        <DocumentEditorToolbar editor={editor} readOnly={readOnly} />
        <Box
          sx={(theme) => ({
            flex: 1,
            overflow: 'auto',
            padding: '0.75rem 1rem',
            '& .ProseMirror': {
              minHeight: '100%',
              outline: 'none',
              color: theme.vars.palette.text.primary,
              fontSize: '0.9375rem',
              lineHeight: 1.6,
            },
            '& .ProseMirror table': {
              borderCollapse: 'collapse',
              width: '100%',
              margin: '0.75rem 0',
            },
            '& .ProseMirror th, & .ProseMirror td': {
              border: `1px solid ${theme.vars.palette.divider}`,
              padding: '0.375rem 0.5rem',
              verticalAlign: 'top',
            },
            '& .ProseMirror th': {
              backgroundColor: theme.vars.palette.background.aiAnswerBkg,
              fontWeight: 600,
            },
            '& .ProseMirror blockquote': {
              borderLeft: `3px solid ${theme.vars.palette.divider}`,
              margin: '0.5rem 0',
              paddingLeft: '0.75rem',
              color: theme.vars.palette.text.secondary,
            },
            '& .ProseMirror code': {
              fontFamily: 'monospace',
              backgroundColor: theme.vars.palette.background.aiAnswerBkg,
              borderRadius: theme.vars.shape.radiusSm,
              padding: '0.125rem 0.25rem',
            },
          })}
        >
          <EditorContent editor={editor} aria-label={ariaLabel} />
        </Box>
      </Box>
    );
  },
);

DocumentEditorImpl.displayName = 'DocumentEditor';

// Named AND default export: `React.lazy(() => import('./DocumentEditor'))`
// needs a default export (its contract requires the resolved module's
// `default` to be the component); the named export keeps every other
// caller (this file's own test) able to `import { DocumentEditor }` like
// every sibling pane in this directory, with no special-casing for the one
// component that happens to be lazy-loaded elsewhere.
export { DocumentEditorImpl as DocumentEditor };
export default DocumentEditorImpl;
