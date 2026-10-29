/**
 * `SettingsPreview` used to render its generated IDE config in a raw `<pre>`
 * because CodeMirror was "not ported to new-app" — a claim that went stale
 * once `shared/ui/CodeMirrorEditor` landed. These tests pin the swap: the
 * content now shows in a read-only CodeMirror editor with a line-number
 * gutter, and it still tracks the selected IDE.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { SettingsPreview } from './SettingsPreview';

installCodeMirrorTestPolyfills();

function getEditor(container: HTMLElement): HTMLElement {
  const content = container.querySelector('.cm-content');
  if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
  return content;
}

describe('SettingsPreview', () => {
  it('renders the generated settings in a read-only CodeMirror editor, not a <pre>', () => {
    const { container } = renderWithTheme(
      <SettingsPreview
        open
        token="tok-abc"
        model={{ id: 7, name: 'gpt-4o' }}
        projectId="42"
        onClose={vi.fn()}
      />,
    );

    expect(container.querySelector('pre')).toBeNull();
    const editor = getEditor(container);
    expect(editor).toHaveAttribute('aria-readonly', 'true');
    expect(editor).toHaveTextContent('eliteacode.authToken');
    expect(container.querySelector('.cm-lineNumbers')).not.toBeNull();
  });

  it('re-renders the editor content when the IDE selection changes', async () => {
    const { container } = renderWithTheme(
      <SettingsPreview
        open
        token="tok-abc"
        model={{ id: 7, name: 'gpt-4o' }}
        projectId="42"
        onClose={vi.fn()}
      />,
    );

    await userEvent.click(screen.getByRole('button', { description: /select ide/i }));
    await userEvent.click(await screen.findByRole('menuitem', { name: /jetbrains/i }));

    expect(getEditor(container)).toHaveTextContent('EliteASettings');
  });

  it('renders nothing when closed', () => {
    const { container } = renderWithTheme(
      <SettingsPreview
        open={false}
        token="tok-abc"
        onClose={vi.fn()}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});
