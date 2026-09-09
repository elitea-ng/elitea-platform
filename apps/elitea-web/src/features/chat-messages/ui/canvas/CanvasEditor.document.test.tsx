/**
 * `CanvasEditor` hosts the `document` pane (issue #879) behind the SAME
 * header as code/table/mermaid — this pins the wiring from the OUTSIDE, the
 * way `CanvasEditor.table.test.tsx` pins the table pane: a passing unit test
 * on `DocumentEditor.tsx` alone could not see whether `CanvasEditor` actually
 * mounts it for `language: 'document'`, dispatches Undo/Close to it, or
 * whether Suspense ever resolves under a real (non-mocked) dynamic import in
 * this test environment — the class of gap issue #597 named.
 */
import type { ReactNode } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CanvasEditor } from './CanvasEditor';

installCodeMirrorTestPolyfills();

function withSocket(children: ReactNode): React.ReactElement {
  return <SocketClientContext.Provider value={createTestSocketClient()}>{children}</SocketClientContext.Provider>;
}

function renderDocumentCanvas(codeBlock: string, extraProps: Record<string, unknown> = {}) {
  const onCloseCanvasEditor = vi.fn();
  const view = renderWithTheme(
    withSocket(
      <CanvasEditor
        selectedCodeBlockInfo={{ codeBlock, language: 'document', isBlock: true }}
        onCloseCanvasEditor={onCloseCanvasEditor}
        {...extraProps}
      />,
    ),
  );
  return { onCloseCanvasEditor, view };
}

describe('CanvasEditor — document canvas (issue #879)', () => {
  it('mounts the rich-text document editor, not an empty placeholder or the code/table panes', async () => {
    renderDocumentCanvas('# A document\n\nSome prose.');

    const editor = await screen.findByTestId('canvas-document-editor');
    expect(editor).toBeTruthy();
    expect(screen.queryByTestId('chat-table-canvas-grid')).not.toBeInTheDocument();
    expect(screen.getByText('A document')).toBeInTheDocument();
    expect(screen.getByText('Some prose.')).toBeInTheDocument();
  });

  it('offers no language-select dropdown for a document (it is a top-level canvas kind, like a table)', async () => {
    renderDocumentCanvas('prose');
    await screen.findByTestId('canvas-document-editor');
    expect(screen.queryByLabelText('Select language')).not.toBeInTheDocument();
  });

  it('closing hands back the LIVE Markdown document, with the document language', async () => {
    const { onCloseCanvasEditor } = renderDocumentCanvas('start');
    await screen.findByTestId('canvas-document-editor');

    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    const [, finalResult, language] = onCloseCanvasEditor.mock.calls[0] as [boolean, string, string];
    expect(finalResult).toContain('start');
    expect(language).toBe('document');
  });

  it('the formatting toolbar is wired to the mounted editor and enabled for an editable canvas', async () => {
    renderDocumentCanvas('start');
    await screen.findByTestId('canvas-document-editor');

    // Proof the toolbar is wired to the SAME editor instance CanvasEditor
    // mounted, not a disconnected one: `DocumentEditorToolbar` renders
    // nothing at all until `useEditor` hands it a live instance (see its own
    // `if (!editor) return null;`), so these controls existing already means
    // the wiring held.
    for (const testId of ['canvas-document-bold', 'canvas-document-italic', 'canvas-document-insert-table']) {
      expect(screen.getByTestId(testId)).not.toBeDisabled();
    }
  });
});
