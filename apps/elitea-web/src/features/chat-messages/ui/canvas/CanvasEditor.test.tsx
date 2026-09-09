/**
 * The canvas editor rendered an EMPTY bordered `<Box>` where its editor
 * belongs — four TODO comments naming `CodeMirrorEditor`,
 * `MarkdownTableEditor`, `MermaidDiagramOutput` and `notifyChange` — while
 * its header row rendered live-looking Undo/Redo/Copy buttons wired to
 * `() => {}`. A screenshot of that is indistinguishable from a working
 * editor.
 *
 * These cases pin the code editor and the three header controls it feeds.
 * The table and mermaid branches are deliberately still unported (see the
 * file's own TODOs), so nothing here asserts them.
 */
import type { ReactElement } from 'react';

import userEvent from '@testing-library/user-event';
import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';

import { CanvasEditor } from './CanvasEditor';

/** The canvas editor joins a canvas socket room on mount; without a provider `useSocketClient` throws outright. */
function withSocket(ui: ReactElement): ReactElement {
  return <SocketClientContext.Provider value={createTestSocketClient()}>{ui}</SocketClientContext.Provider>;
}

installCodeMirrorTestPolyfills();

function getContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

const BLOCK = { codeBlock: 'hello', language: 'markdown', isBlock: true };

describe('CanvasEditor', () => {
  it('renders a real CodeMirror document, not an empty placeholder box', () => {
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor
        selectedCodeBlockInfo={BLOCK}
        onCloseCanvasEditor={vi.fn()}
      />),
    );
    expect(getContent(container)).toHaveTextContent('hello');
  });

  it('carries edits out through onCloseCanvasEditor', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor
        selectedCodeBlockInfo={BLOCK}
        onCloseCanvasEditor={onClose}
      />),
    );

    await user.click(getContent(container));
    await user.keyboard('Z');
    await vi.waitFor(() => {
      // The close button is the header's first control; its tooltip is "Close".
      expect(container.querySelector('.cm-content')).toHaveTextContent('Zhello');
    });

    await user.click(screen.getByTestId('canvas-edit-close'));
    await vi.waitFor(() => {
      expect(onClose).toHaveBeenCalled();
    });
    const [, finalResult] = onClose.mock.calls.at(-1) as [boolean, string, string];
    expect(finalResult).toBe('Zhello');
  });

  /*
   * Undo starts disabled and becomes enabled on the first edit. Before this
   * wiring, `canUndo` was a `useState` whose setter was named `_setCanUndo`
   * and never called — the button was permanently disabled, and its `onUndo`
   * was an empty arrow, so BOTH halves had to be fixed for either to matter.
   */
  it('enables Undo once the document changes, and undoing restores the text', async () => {
    const user = userEvent.setup();
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor
        selectedCodeBlockInfo={BLOCK}
        onCloseCanvasEditor={vi.fn()}
      />),
    );

    const undoButton = screen.getByTestId('canvas-edit-undo');
    expect(undoButton).toBeDisabled();

    await user.click(getContent(container));
    await user.keyboard('Z');
    await vi.waitFor(() => {
      expect(screen.getByTestId('canvas-edit-undo')).toBeEnabled();
    });

    await user.click(screen.getByTestId('canvas-edit-undo'));
    await vi.waitFor(() => {
      expect(getContent(container)).toHaveTextContent('hello');
    });
  });

  it('copies the LIVE document, not the last debounced value', async () => {
    const user = userEvent.setup();
    // AFTER `setup()`: userEvent installs its own clipboard stub on
    // `navigator`, so defining this first would be overwritten silently and
    // the assertion below would never see a call.
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText } });
    const { container } = renderWithTheme(
      withSocket(<CanvasEditor
        selectedCodeBlockInfo={BLOCK}
        onCloseCanvasEditor={vi.fn()}
      />),
    );

    await user.click(getContent(container));
    await user.keyboard('Z');
    // Deliberately no wait for the 30ms change debounce.
    await user.click(screen.getByTestId('canvas-edit-copy'));
    expect(writeText).toHaveBeenCalledWith('Zhello');
  });

  describe('"Save to artifacts" (issue #878)', () => {
    it('offers no such control without a `saveToArtifacts` prop', () => {
      renderWithTheme(
        withSocket(<CanvasEditor
          selectedCodeBlockInfo={BLOCK}
          onCloseCanvasEditor={vi.fn()}
        />),
      );
      expect(screen.queryByTestId('canvas-edit-save-to-artifacts')).not.toBeInTheDocument();
    });

    it('opens pre-filled from the existing source, saves the LIVE document, and reports the (possibly new) source back', async () => {
      const user = userEvent.setup();
      const canvasFileTransfer = await import('../../model/canvasFileTransfer');
      vi.spyOn(canvasFileTransfer, 'listArtifactBucketNames').mockResolvedValue(['docs']);
      vi.spyOn(canvasFileTransfer, 'artifactObjectExists').mockResolvedValue(false);
      vi.spyOn(canvasFileTransfer, 'saveCanvasToArtifact').mockResolvedValue({ ok: true });
      const onSaved = vi.fn();

      const { container } = renderWithTheme(
        withSocket(<CanvasEditor
          selectedCodeBlockInfo={BLOCK}
          onCloseCanvasEditor={vi.fn()}
          saveToArtifacts={{ source: { bucket: 'docs', name: 'notes.md' }, onSaved }}
        />),
      );

      // Edit the document BEFORE saving — the dialog must see this, not the
      // content the block opened with.
      await user.click(getContent(container));
      await user.keyboard('Z');

      await user.click(screen.getByTestId('canvas-edit-save-to-artifacts'));
      await screen.findByTestId('canvas-save-to-artifacts-dialog');
      expect(screen.getByTestId('canvas-save-filename-input')).toHaveValue('notes.md');

      await user.click(screen.getByTestId('canvas-save-submit'));

      await vi.waitFor(() => {
        expect(canvasFileTransfer.saveCanvasToArtifact).toHaveBeenCalledWith(
          expect.objectContaining({ bucket: 'docs', name: 'notes.md', content: 'Zhello' }),
        );
      });
      expect(onSaved).toHaveBeenCalledWith({ bucket: 'docs', name: 'notes.md' });
      // The dialog closes itself once the write lands.
      await vi.waitFor(() => {
        expect(screen.queryByTestId('canvas-save-to-artifacts-dialog')).not.toBeInTheDocument();
      });
    });
  });
});
