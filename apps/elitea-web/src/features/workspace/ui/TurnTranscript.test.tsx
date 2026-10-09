import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TurnTranscript } from './TurnTranscript';

function renderText(text: string): HTMLElement {
  renderWithTheme(<TurnTranscript view={{ phase: null, items: [{ type: 'text', key: 't1', text }], approvals: [] }} busy={false} onCancel={vi.fn()} />);
  return screen.getByTestId('turn-text');
}

describe('TurnTranscript', () => {
  it('renders the answer as Markdown, as the chat does', () => {
    const text = renderText('## Elitea\n\n**Bold** and `code`\n\n| A | B |\n| --- | --- |\n| 1 | 2 |\n\n- one\n- two');
    expect(screen.getByRole('heading', { name: 'Elitea' })).toBeInTheDocument();
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(screen.getAllByRole('listitem')).toHaveLength(2);
    expect(text.querySelector('strong')?.textContent).toBe('Bold');
    expect(text.textContent).not.toContain('**');
  });

  it('drops raw HTML the model writes instead of rendering it', () => {
    const text = renderText('before <img src="https://example.invalid/x.png" onerror="alert(1)"> after');
    expect(text.querySelector('img')).toBeNull();
  });
});
