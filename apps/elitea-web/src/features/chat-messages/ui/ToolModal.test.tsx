/**
 * The dismiss control of `ToolModal` is an icon-only `IconButton`.
 *
 * `CloseIcon` renders an `<svg>` with no text, so the button had an empty
 * accessible name. A screen reader announced it as a bare "button". The user
 * could not tell the dismiss control from any other control in the modal.
 *
 * This test names the control by its accessible name. It fails if the
 * `aria-label` goes away again.
 *
 * The rest of the file pins the INPUT | OUTPUT split restored from the
 * baseline (`apps/elitea-ui/src/components/Chat/ToolModal.jsx`): two
 * read-only CodeMirror panes, each with its own language selector and copy
 * button, and the two-part `"<type> - <name>"` title.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { ToolModal } from './ToolModal';
import { resolveToolModalLanguage } from './ToolModalPane';
import { TraceStepDetailContext } from '../model/traceStepDetail';

installCodeMirrorTestPolyfills();

describe('ToolModal', () => {
  it('names the dismiss control and closes on click', async () => {
    const onClose = vi.fn();
    renderWithTheme(
      <ToolModal
        open
        onClose={onClose}
        toolAction={{ name: 'search_docs' }}
      />,
    );

    expect(screen.getAllByRole('heading', { name: 'search_docs' }).length).toBeGreaterThan(0);

    await userEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('composes the title as "<type> - <name>" when the action carries both', () => {
    renderWithTheme(
      <ToolModal
        open
        onClose={vi.fn()}
        toolAction={{ type: 'toolkit', name: 'Agent Exception Stacktrace' }}
      />,
    );

    expect(
      screen.getAllByRole('heading', { name: 'toolkit - Agent Exception Stacktrace' }).length,
    ).toBeGreaterThan(0);
  });

  it('renders INPUT and OUTPUT as two read-only editors carrying their own text', () => {
    renderWithTheme(
      <ToolModal
        open
        onClose={vi.fn()}
        toolAction={{ name: 'search_docs', toolInputs: { q: 'kittens' }, toolOutputs: 'found 3' }}
      />,
    );

    expect(screen.getByText('INPUT')).toBeInTheDocument();
    expect(screen.getByText('OUTPUT')).toBeInTheDocument();

    const editors = document.body.querySelectorAll('.cm-content');
    expect(editors).toHaveLength(2);
    expect(editors[0]).toHaveTextContent('"q": "kittens"');
    expect(editors[1]).toHaveTextContent('found 3');
    // Read-only: neither pane accepts edits.
    for (const editor of editors) expect(editor).toHaveAttribute('aria-readonly', 'true');
    // A line-number gutter is present in both panes.
    expect(document.body.querySelectorAll('.cm-lineNumbers').length).toBe(2);
  });

  it('falls back to `content` for the OUTPUT pane when there are no toolOutputs', () => {
    renderWithTheme(
      <ToolModal
        open
        onClose={vi.fn()}
        toolAction={{ name: 'search_docs', content: 'plain content' }}
      />,
    );

    const editors = document.body.querySelectorAll('.cm-content');
    expect(editors[1]).toHaveTextContent('plain content');
  });

  it('gives each pane its own language selector defaulting to Auto-detect, plus a copy button', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    // Not `vi.stubGlobal('navigator', {...navigator, ...})` — spreading the
    // real `Navigator` instance drops its prototype (oxlint
    // `no-misused-spread`). Redefining the one property leaves it intact.
    const originalClipboard = Object.getOwnPropertyDescriptor(globalThis.navigator, 'clipboard');
    Object.defineProperty(globalThis.navigator, 'clipboard', { value: { writeText }, configurable: true });

    renderWithTheme(
      <ToolModal
        open
        onClose={vi.fn()}
        toolAction={{ name: 'search_docs', toolInputs: 'in', toolOutputs: 'out' }}
      />,
    );

    const inputSelect = screen.getByLabelText('Content type for INPUT');
    expect(within(inputSelect.parentElement as HTMLElement).getByText('Auto-detect')).toBeInTheDocument();
    expect(screen.getByLabelText('Content type for OUTPUT')).toBeInTheDocument();

    await userEvent.click(screen.getByRole('button', { name: 'Copy OUTPUT' }));
    expect(writeText).toHaveBeenCalledWith('out');

    if (originalClipboard) Object.defineProperty(globalThis.navigator, 'clipboard', originalClipboard);
  });
});

describe('resolveToolModalLanguage', () => {
  it('passes an explicit selection through untouched', () => {
    expect(resolveToolModalLanguage('json', 'not json at all')).toBe('json');
    expect(resolveToolModalLanguage('text', '{"a":1}')).toBe('text');
  });

  it('detects JSON documents under auto', () => {
    expect(resolveToolModalLanguage('auto', '  {"a": 1} ')).toBe('json');
    expect(resolveToolModalLanguage('auto', '[1, 2, 3]')).toBe('json');
  });

  it('falls back to text under auto for anything that is not parseable JSON', () => {
    expect(resolveToolModalLanguage('auto', '{ broken')).toBe('text');
    expect(resolveToolModalLanguage('auto', 'a stack trace')).toBe('text');
    expect(resolveToolModalLanguage('auto', '')).toBe('text');
  });
});

/**
 * #951 — a pin rebuilt from a persisted trace row has no body of its own: the
 * listing that rebuilt it is deliberately light. These pin the ONE request it
 * is allowed to make, and the three cases in which it must make none.
 */
describe('ToolModal on a restored trace pin', () => {
  const restored = { name: 'mock_tool_status', traceStepId: 11, traceMessageGroupId: 1121 } as const;

  function renderUnderLoader(
    load: (stepId: number, groupId: number) => Promise<{ toolInputs: unknown; output: string }>,
    toolAction: Record<string, unknown> = restored,
  ) {
    return renderWithTheme(
      <TraceStepDetailContext value={load}>
        <ToolModal open onClose={vi.fn()} toolAction={toolAction} />
      </TraceStepDetailContext>,
    );
  }

  it('fetches the step body once and shows it in the OUTPUT pane', async () => {
    const load = vi.fn().mockResolvedValue({ toolInputs: { response_search: 'a' }, output: 'tool.unavailable' });
    renderUnderLoader(load);

    expect(await screen.findByText('tool.unavailable')).toBeInTheDocument();
    expect(load).toHaveBeenCalledExactlyOnceWith(11, 1121);
    expect(within(screen.getByTestId('tool-modal-pane-input')).getByText(/response_search/)).toBeInTheDocument();
  });

  it('asks for nothing when the action already carries its own body', () => {
    const load = vi.fn();
    renderUnderLoader(load, { ...restored, toolOutputs: 'live output' });
    expect(load).not.toHaveBeenCalled();
  });

  it('asks for nothing for a live pin, which names no trace row', () => {
    const load = vi.fn();
    renderUnderLoader(load, { name: 'mock_tool_status' });
    expect(load).not.toHaveBeenCalled();
  });

  it('leaves the pin as it was when the read fails', async () => {
    const load = vi.fn().mockRejectedValue(new Error('403'));
    renderUnderLoader(load);
    await vi.waitFor(() => { expect(load).toHaveBeenCalled(); });
    expect(screen.getAllByRole('heading', { name: 'mock_tool_status' }).length).toBeGreaterThan(0);
  });
});
