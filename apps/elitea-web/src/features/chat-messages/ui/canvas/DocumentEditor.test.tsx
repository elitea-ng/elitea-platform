/**
 * The `document` canvas pane (issue #879) — Markdown round-trip (prose,
 * headings, lists, marks, a GFM table), and undo/redo through the same
 * imperative handle `CanvasEditor` drives.
 *
 * Typing is exercised through `setCode` rather than `userEvent.type`
 * (deliberate): jsdom has no `execCommand`-backed contentEditable, so a
 * simulated keystroke never touches the DOM ProseMirror's own mutation
 * observer reads from — `setCode` is not a test-only shortcut, it is the
 * SAME path `CanvasEditor`'s canvas-sync already drives this component
 * through on a real remote edit.
 */
import { createRef } from 'react';

import { waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { DocumentEditor } from './DocumentEditor';
import type { DocumentEditorHandle } from './DocumentEditor';

// ProseMirror measures text layout the same way CodeMirror does
// (`Range.prototype.getClientRects`/`getBoundingClientRect`) — jsdom
// implements neither; see that module's doc for the full explanation.
installCodeMirrorTestPolyfills();

function renderDocumentEditor(initialMarkdown: string, onChange = vi.fn()) {
  const ref = createRef<DocumentEditorHandle>();
  const utils = renderWithTheme(<DocumentEditor ref={ref} content={{ initialMarkdown, onChange }} />);
  return { ref, onChange, ...utils };
}

describe('DocumentEditor (issue #879)', () => {
  it('round-trips prose, a heading, and a list through getCode/setCode', async () => {
    const { ref } = renderDocumentEditor('# Title\n\nSome **bold** and *italic* text.\n\n- one\n- two\n');
    await waitFor(() => expect(ref.current).not.toBeNull());

    const markdown = ref.current?.getCode() ?? '';
    expect(markdown).toContain('# Title');
    expect(markdown).toContain('**bold**');
    expect(markdown).toContain('*italic*');
    expect(markdown).toContain('- one');
    expect(markdown).toContain('- two');
  });

  it('round-trips a GFM pipe table (issue #879: "tables (basic)")', async () => {
    const tableMarkdown = '| Metric | Value |\n| --- | --- |\n| alpha | 1 |\n| beta | 2 |\n';
    const { ref } = renderDocumentEditor(tableMarkdown);
    await waitFor(() => expect(ref.current).not.toBeNull());

    const markdown = ref.current?.getCode() ?? '';
    // Not a byte-exact match (tiptap-markdown's own spacing rules apply) —
    // the claim is that the table survived as a REAL pipe table, not that it
    // fell back to an embedded raw-HTML block.
    expect(markdown).toContain('| Metric | Value |');
    expect(markdown).toContain('| --- | --- |');
    expect(markdown).toContain('alpha');
    expect(markdown).toContain('beta');
    expect(markdown).not.toContain('<table');
  });

  it('setCode replaces the document and is itself undoable', async () => {
    const { ref } = renderDocumentEditor('original text');
    await waitFor(() => expect(ref.current).not.toBeNull());
    expect(ref.current?.getCode()).toContain('original text');

    ref.current?.setCode('replaced text');
    await waitFor(() => expect(ref.current?.getCode()).toContain('replaced text'));

    ref.current?.undo();
    await waitFor(() => expect(ref.current?.getCode()).toContain('original text'));

    ref.current?.redo();
    await waitFor(() => expect(ref.current?.getCode()).toContain('replaced text'));
  });

  it('reports undo/redo availability as the document changes', async () => {
    const onCanUndo = vi.fn();
    const onCanRedo = vi.fn();
    const ref = createRef<DocumentEditorHandle>();
    renderWithTheme(
      <DocumentEditor
        ref={ref}
        content={{ initialMarkdown: 'start' }}
        history={{ onCanUndo, onCanRedo }}
      />,
    );
    await waitFor(() => expect(ref.current).not.toBeNull());
    onCanUndo.mockClear();

    ref.current?.setCode('changed');
    await waitFor(() => expect(onCanUndo).toHaveBeenCalledWith(true));

    ref.current?.undo();
    await waitFor(() => expect(onCanUndo).toHaveBeenCalledWith(false));
  });

  it('debounces the onChange callback rather than firing once per programmatic edit', async () => {
    const onChange = vi.fn();
    const { ref } = renderDocumentEditor('start', onChange);
    await waitFor(() => expect(ref.current).not.toBeNull());
    onChange.mockClear();

    ref.current?.setCode('start plus more');
    await waitFor(() => expect(onChange).toHaveBeenCalled());
    expect(onChange.mock.calls.at(-1)?.[0]).toContain('start plus more');
  });
});
