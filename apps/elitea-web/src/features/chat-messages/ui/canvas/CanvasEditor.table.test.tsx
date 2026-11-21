/**
 * `CanvasEditor` hosts TWO editors behind ONE header, and the header's
 * undo/redo/copy/close must dispatch to whichever pane is mounted:
 * `CodeMirrorEditor` for code and mermaid source, `MarkdownTableEditor` for a
 * `markdownTable` canvas. Their undo stacks are separate, so `canUndo` has to
 * follow the active pane too — a single shared flag would leave the header
 * enabled from the pane that just unmounted.
 *
 * This file pins that dispatch from the OUTSIDE (through the header buttons a
 * user actually clicks), plus the seats that used to be TODO comments — a
 * header button that called nothing and a `MarkdownTableEditor` placeholder.
 * A passing unit test on either half alone could not see that wiring, which is
 * the recurring failure mode here (issue #597).
 */
import type { ReactNode } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { parseMarkdownTable } from '../../lib/markdownTable';
import { CanvasEditor } from './CanvasEditor';
import { testTimeout } from '@/test/timeouts';

installCodeMirrorTestPolyfills();

const TABLE = '| Name | Expression |\n| --- | --- |\n| or | a \\| b |\n';
const CODE = 'const answer = 41;\n';

/** The canvas socket hooks require a client in context (R-M1: the in-memory double, never a vi.mock of socket.io). */
function withSocket(children: ReactNode): React.ReactElement {
  return <SocketClientContext.Provider value={createTestSocketClient()}>{children}</SocketClientContext.Provider>;
}

function renderCanvas(codeBlock: string, language: string) {
  const onCloseCanvasEditor = vi.fn();
  const view = renderWithTheme(
    withSocket(
      <CanvasEditor
        selectedCodeBlockInfo={{ codeBlock, language, isBlock: true }}
        onCloseCanvasEditor={onCloseCanvasEditor}
      />,
    ),
  );
  return { onCloseCanvasEditor, view };
}

const renderTableCanvas = () => renderCanvas(TABLE, 'markdownTable');

/**
 * CM6's `.cm-content` is the element that actually carries `role="textbox"` and
 * receives keystrokes — the same accessor `CodeMirrorEditor.test.tsx` uses.
 * Going through `getByRole` instead hangs under jsdom.
 */
function codeMirrorContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

/** The markdown the editor hands back when the user closes it. */
function closedWith(onCloseCanvasEditor: ReturnType<typeof vi.fn>): string {
  return onCloseCanvasEditor.mock.calls[0]?.[1] as string;
}

describe('CanvasEditor — markdown table canvas', () => {
  it('mounts the table editor rather than an empty placeholder', () => {
    renderTableCanvas();
    expect(screen.getByTestId('chat-table-canvas-grid')).toBeTruthy();
    expect(screen.getByText('a | b')).toBeTruthy();
  });

  it('the header Add row button reaches the table editor and the change is serialised back out', async () => {
    const { onCloseCanvasEditor } = renderTableCanvas();

    await userEvent.click(screen.getByRole('button', { name: 'Add row' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(parseMarkdownTable(closedWith(onCloseCanvasEditor)).rows).toHaveLength(2);
  });

  it('the header Add column button reaches the table editor', async () => {
    const { onCloseCanvasEditor } = renderTableCanvas();

    await userEvent.click(screen.getByRole('button', { name: 'Add column' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(parseMarkdownTable(closedWith(onCloseCanvasEditor)).headers).toEqual([
      'Name',
      'Expression',
      'Column_3',
    ]);
  });

  it('renders the CSV import control on a table canvas', () => {
    renderTableCanvas();
    expect(screen.getByTestId('canvas-table-import-input')).toBeTruthy();
  });

  it('closes with the table pane’s LIVE markdown, not the debounced mirror', async () => {
    const { onCloseCanvasEditor } = renderTableCanvas();

    // No wait between the edit and the close: the 30ms `onChange` debounce has
    // not fired, so a close reading the mirrored `code` would hand back the
    // pre-edit table.
    await userEvent.click(screen.getByRole('button', { name: 'Add row' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(parseMarkdownTable(closedWith(onCloseCanvasEditor)).rows).toHaveLength(2);
  });
});

describe('CanvasEditor — undo/redo dispatches to the active pane', () => {
  it('the table pane: undo is disabled until an edit, then reverses it', async () => {
    const { onCloseCanvasEditor } = renderTableCanvas();

    const undo = screen.getByRole('button', { name: 'Undo' });
    expect(undo.hasAttribute('disabled')).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: 'Add row' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(false));

    await userEvent.click(screen.getByRole('button', { name: 'Undo' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(parseMarkdownTable(closedWith(onCloseCanvasEditor)).rows).toHaveLength(1);
  });

  it('the table pane: redo re-applies what undo reversed', async () => {
    const { onCloseCanvasEditor } = renderTableCanvas();

    await userEvent.click(screen.getByRole('button', { name: 'Add row' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(false));
    await userEvent.click(screen.getByRole('button', { name: 'Undo' }));

    await waitFor(() => expect(screen.getByRole('button', { name: 'Redo' }).hasAttribute('disabled')).toBe(false));
    await userEvent.click(screen.getByRole('button', { name: 'Redo' }));
    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(parseMarkdownTable(closedWith(onCloseCanvasEditor)).rows).toHaveLength(2);
  });

  it('the code pane: undo is disabled until a keystroke, then reverses it', async () => {
    const user = userEvent.setup();
    const { onCloseCanvasEditor, view } = renderCanvas(CODE, 'javascript');

    expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(true);

    await user.click(codeMirrorContent(view.container));
    await user.keyboard('X');

    await waitFor(() => expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(false));
    await user.click(screen.getByRole('button', { name: 'Undo' }));
    await user.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(closedWith(onCloseCanvasEditor)).toBe(CODE);
  });

  it('the code pane: closes with the LIVE document, inside the change debounce', async () => {
    const user = userEvent.setup();
    const { onCloseCanvasEditor, view } = renderCanvas(CODE, 'javascript');

    await user.click(codeMirrorContent(view.container));
    await user.keyboard('X');
    // No wait: the 30ms debounce has not fired, so the mirrored `code` is still
    // pre-keystroke. Close must still report what the editor actually holds.
    await user.click(screen.getByRole('button', { name: 'Close editor' }));

    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(closedWith(onCloseCanvasEditor)).toBe(`X${CODE}`);
  });

  it('each pane answers from its OWN history — a table edit does not enable undo on a code canvas', async () => {
    // Build undo depth in the TABLE pane of one editor…
    const table = renderTableCanvas();
    await userEvent.click(screen.getByRole('button', { name: 'Add row' }));
    await waitFor(() => expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(false));
    table.view.unmount();

    // …and confirm a code canvas starts from its own empty one. A single shared
    // `canUndo` would leave this button enabled with nothing to undo.
    const { onCloseCanvasEditor } = renderCanvas(CODE, 'javascript');
    expect(screen.getByRole('button', { name: 'Undo' }).hasAttribute('disabled')).toBe(true);

    await userEvent.click(screen.getByRole('button', { name: 'Close editor' }));
    await waitFor(() => expect(onCloseCanvasEditor).toHaveBeenCalled());
    expect(closedWith(onCloseCanvasEditor)).toBe(CODE);
  });
});

/*
 * The `quickFix` objects below are hand-built, NOT produced by
 * `useMermaidQuickFix`. The hook cannot report `isAvailable: true` in any build
 * today: its first condition is `hasBackendCapability('llmPredictBlocking')`, and
 * `predict_llm` is not routed (#194). These cases are about what CANVAS EDITOR
 * does with a capability once one exists — whether it offers the control, and
 * for which diagrams. Whether the hook can ever hand it one is
 * `./MermaidQuickFixButton.test.tsx`'s subject, and its first case is the one
 * that describes today.
 */
describe('CanvasEditor — mermaid canvas', () => {
  it('renders no quick-fix control when no capability was injected', async () => {
    renderCanvas('graph TD;\n  A --> B;', 'mermaid');
    await waitFor(() => expect(screen.getByTestId('canvas-mermaid-diagram')).toBeTruthy());
    expect(screen.queryByTestId('canvas-mermaid-quick-fix')).toBeNull();
  });

  it('renders no quick-fix control for a diagram that renders, even with a capability', async () => {
    const quickFix = {
      capability: { isAvailable: true, reason: null, tooltip: 'Quick Fix: small (low-tier)', model: null },
      run: vi.fn(),
    };
    renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={{ codeBlock: 'graph TD;\n  A[Start] --> B[Finish];', language: 'mermaid', isBlock: true }}
          onCloseCanvasEditor={vi.fn()}
          quickFix={quickFix as never}
        />,
      ),
    );

    await waitFor(
      () => expect(screen.getByTestId('canvas-mermaid-diagram').querySelector('svg')).not.toBeNull(),
      { timeout: 15000 },
    );
    expect(screen.queryByTestId('canvas-mermaid-quick-fix')).toBeNull();
  }, testTimeout(20000));

  it('offers the quick-fix control once the diagram fails to render', async () => {
    const quickFix = {
      capability: { isAvailable: true, reason: null, tooltip: 'Quick Fix: small (low-tier)', model: null },
      run: vi.fn(),
    };
    renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={{ codeBlock: 'graph TD;\n  A -->', language: 'mermaid', isBlock: true }}
          onCloseCanvasEditor={vi.fn()}
          quickFix={quickFix as never}
        />,
      ),
    );

    await waitFor(() => expect(screen.getByTestId('canvas-mermaid-quick-fix')).toBeTruthy(), { timeout: 15000 });
  }, testTimeout(20000));
});

describe('CanvasEditor — concurrent-edit notice', () => {
  const NOTICE = /Anyone with access can edit this canvas/;

  it('is shown on a shared, editable canvas', () => {
    renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={{ codeBlock: TABLE, language: 'markdownTable', isBlock: true, canvasId: 'c-1' }}
          onCloseCanvasEditor={vi.fn()}
        />,
      ),
    );
    expect(screen.getByText(NOTICE)).toBeTruthy();
  });

  it('is hidden on a read-only canvas, and on one with no canvas id', () => {
    const { unmount } = renderWithTheme(
      withSocket(
        <CanvasEditor
          selectedCodeBlockInfo={{ codeBlock: TABLE, language: 'markdownTable', isBlock: true, canvasId: 'c-1' }}
          onCloseCanvasEditor={vi.fn()}
          viewOnly
        />,
      ),
    );
    expect(screen.queryByText(NOTICE)).toBeNull();
    unmount();

    renderTableCanvas();
    expect(screen.queryByText(NOTICE)).toBeNull();
  });
});
