import { EditorView } from '@codemirror/view';
import { act, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { YamlCodeEditor } from './YamlCodeEditor';

installCodeMirrorTestPolyfills();

/** CM6's `.cm-content` is the real `role="textbox"` element every assertion below reads/types into. */
function getContent(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

/**
 * Every `<style>` element's text, concatenated — the real, rendered CSS
 * output. Same pattern as `src/app/providers/BrandThemeProvider.test.tsx`'s
 * `renderedStyleText()`: jsdom's `getComputedStyle` does not evaluate
 * descendant-combinator rules (verified empirically — a rule like
 * `.css-x .error_yaml_code { border-left: ... }` never shows up via
 * `getComputedStyle(mark).borderLeft` in this test environment even though
 * the rule is genuinely present and correct), so asserting on the emitted
 * stylesheet text is the real, reliable way to test an `sx`-driven nested
 * selector here.
 */
function renderedStyleText(): string {
  return [...document.querySelectorAll('style')].map((style) => style.textContent ?? '').join('\n');
}

describe('YamlCodeEditor', () => {
  it('renders the initial code', () => {
    const { container } = renderWithTheme(<YamlCodeEditor code="a: b" onChangeCode={vi.fn()} />);
    expect(getContent(container)).toHaveTextContent('a: b');
  });

  it('reports edits via onChangeCode, debounced', async () => {
    const user = userEvent.setup();
    const onChangeCode = vi.fn();
    const { container } = renderWithTheme(<YamlCodeEditor code="a: b" onChangeCode={onChangeCode} />);
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('X');
    await vi.waitFor(() => {
      expect(onChangeCode).toHaveBeenCalledWith('Xa: b');
    });
  });

  it('does not accept edits when disabled', async () => {
    const user = userEvent.setup();
    const onChangeCode = vi.fn();
    const { container } = renderWithTheme(<YamlCodeEditor code="a: b" onChangeCode={onChangeCode} disabled />);
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('X');
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(content).toHaveTextContent('a: b');
    expect(onChangeCode).not.toHaveBeenCalled();
  });

  it('re-syncs its content when the `code` prop changes externally (e.g. a caller-driven reset)', () => {
    const { container, rerender } = renderWithTheme(<YamlCodeEditor code="first: 1" onChangeCode={vi.fn()} />);
    expect(getContent(container)).toHaveTextContent('first: 1');
    rerender(<YamlCodeEditor code="second: 2" onChangeCode={vi.fn()} />);
    expect(getContent(container)).toHaveTextContent('second: 2');
  });

  it('flags invalid YAML with the baseline error mark class once the linter runs', async () => {
    const user = userEvent.setup();
    const { container } = renderWithTheme(<YamlCodeEditor code="a: [" onChangeCode={vi.fn()} />);
    const content = getContent(container);
    await user.click(content);
    // Nudge the doc (a no-op keystroke would not trigger a new lint pass).
    await user.keyboard('{End} ');

    await vi.waitFor(
      () => {
        expect(container.querySelector('.error_yaml_code')).not.toBeNull();
      },
      { timeout: 2000 },
    );
  });

  it('clears the error mark once the document becomes valid YAML again', async () => {
    const user = userEvent.setup();
    const { container } = renderWithTheme(<YamlCodeEditor code="a: [" onChangeCode={vi.fn()} />);
    const content = getContent(container);
    await user.click(content);
    await user.keyboard('{End} ');
    await vi.waitFor(
      () => {
        expect(container.querySelector('.error_yaml_code')).not.toBeNull();
      },
      { timeout: 2000 },
    );

    await user.keyboard('{Backspace}{Backspace}');
    await vi.waitFor(
      () => {
        expect(container.querySelector('.error_yaml_code')).toBeNull();
      },
      { timeout: 2000 },
    );
  });

  it('carries the data-testid and pan/drag/wheel-exclusion class the pipeline flow canvas relies on', () => {
    const { getByTestId } = renderWithTheme(<YamlCodeEditor code="a: b" onChangeCode={vi.fn()} />);
    const wrapper = getByTestId('pipeline-yaml-editor');
    expect(wrapper).toHaveClass('nopan', 'nodrag', 'nowheel');
  });

  it('colourises a mapping key distinctly from a comment and from an untagged plain value (the YAML language extension is actually installed)', () => {
    const { container } = renderWithTheme(<YamlCodeEditor code={'name: value\n# a comment'} onChangeCode={vi.fn()} />);
    const content = getContent(container);
    const spans = [...content.querySelectorAll('span')];

    const keySpan = spans.find((span) => span.textContent === 'name');
    if (!keySpan) throw new Error('expected a highlighted span for the "name" key');
    // Matches `CodeMirrorEditor.tsx`'s `highlightStyle` rule for
    // `tags.propertyName`: bold, `text.primary`.
    expect(getComputedStyle(keySpan).fontWeight).toBe('600');
    expect(getComputedStyle(keySpan).color).toBe('var(--el-palette-text-primary)');

    const commentSpan = spans.find((span) => span.textContent === '# a comment');
    if (!commentSpan) throw new Error('expected a highlighted span for the comment');
    // Matches the `tags.comment` rule: italic, `text.secondary`.
    expect(getComputedStyle(commentSpan).fontStyle).toBe('italic');
    expect(getComputedStyle(commentSpan).color).toBe('var(--el-palette-text-secondary)');

    // The unquoted scalar value carries no language tag, so — unlike the
    // key and the comment — it is never wrapped in its own highlighted span.
    expect(spans.some((span) => span.textContent === 'value')).toBe(false);
  });

  it('pairs the error-mark background with a `border.error`-coloured left border, restoring a strong visual signal for an invalid-YAML line', async () => {
    const user = userEvent.setup();
    const { container } = renderWithTheme(<YamlCodeEditor code="a: [" onChangeCode={vi.fn()} />);
    const styleText = renderedStyleText();
    expect(styleText).toContain('.error_yaml_code{');
    expect(styleText).toMatch(
      /\.error_yaml_code\{[^}]*border-left:0\.1875rem solid var\(--el-palette-border-error\)/,
    );
    // Sanity: the rule is actually reachable by the diagnostic mark CM6
    // renders once the linter flags this invalid document, not just present
    // somewhere unrelated in the stylesheet.
    await user.click(getContent(container));
    await user.keyboard('{End} ');
    await vi.waitFor(() => {
      expect(container.querySelector('.error_yaml_code')).not.toBeNull();
    });
  });

  it('renders Copy YAML and Full screen view actions and opens/closes the full-screen modal', async () => {
    const user = userEvent.setup();
    const onFullScreenChange = vi.fn();
    const { getByTestId, queryByTestId } = renderWithTheme(
      <YamlCodeEditor
        code="nodes: []"
        onChangeCode={vi.fn()}
        onFullScreenChange={onFullScreenChange}
      />,
    );

    expect(getByTestId('pipeline-yaml-copy-button')).toBeInTheDocument();
    expect(queryByTestId('pipeline-yaml-full-screen-modal')).not.toBeInTheDocument();

    await user.click(getByTestId('pipeline-yaml-full-screen-button'));
    expect(getByTestId('pipeline-yaml-full-screen-modal')).toBeInTheDocument();
    expect(onFullScreenChange).toHaveBeenCalledWith(true);

    await user.click(getByTestId('pipeline-yaml-full-screen-close-button'));
    expect(queryByTestId('pipeline-yaml-full-screen-modal')).not.toBeInTheDocument();
    expect(onFullScreenChange).toHaveBeenCalledWith(false);
  });

  it('flushes pending un-debounced edits when closing full-screen modal within 30ms', () => {
    const onChangeCode = vi.fn();
    const { getByTestId, queryByTestId } = renderWithTheme(
      <YamlCodeEditor
        code="nodes: []"
        onChangeCode={onChangeCode}
      />,
    );

    fireEvent.click(getByTestId('pipeline-yaml-full-screen-button'));
    const modal = getByTestId('pipeline-yaml-full-screen-modal');
    const modalContent = modal.querySelector('.cm-content');
    if (!(modalContent instanceof HTMLElement)) throw new Error('Full-screen CodeMirror content not found');
    const view = EditorView.findFromDOM(modalContent);
    if (!view) throw new Error('Full-screen CodeMirror view not found');

    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    try {
      act(() => {
        view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: 'nodes:\n  - id: fast_edit' } });
      });
      // Close BEFORE the 30ms debounce fires — live text must be flushed synchronously.
      fireEvent.click(getByTestId('pipeline-yaml-full-screen-close-button'));
      expect(queryByTestId('pipeline-yaml-full-screen-modal')).not.toBeInTheDocument();
      expect(onChangeCode).toHaveBeenCalledWith('nodes:\n  - id: fast_edit');
    } finally {
      vi.useRealTimers();
    }
  });
});
