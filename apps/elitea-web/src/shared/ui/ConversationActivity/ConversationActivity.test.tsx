import { screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme as render } from '../lib/testTheme';
import { ActivityGroupSummary, ConversationActivityGlyph } from './ConversationActivity';

function mockReducedMotion(reduce: boolean): void {
  vi.stubGlobal(
    'matchMedia',
    vi.fn((query: string) => ({
      matches: reduce && query.includes('prefers-reduced-motion'),
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  );
}

afterEach(() => vi.unstubAllGlobals());

describe('ConversationActivityGlyph', () => {
  it.each([
    ['running', 'Running'],
    ['needs_you', 'Needs your input'],
    ['unread', 'Finished, unread'],
    ['done', 'Finished'],
  ] as const)('shows %s with its accessible name', (kind, name) => {
    mockReducedMotion(false);
    render(<ConversationActivityGlyph kind={kind} />);
    const glyph = screen.getByTestId('conversation-activity-glyph');
    expect(glyph).toHaveAttribute('data-kind', kind);
    expect(glyph).toHaveTextContent(name);
    expect(glyph.querySelector('svg')?.closest('[aria-hidden="true"]')).not.toBeNull();
  });

  it('shows nothing without a kind', () => {
    const { container } = render(<ConversationActivityGlyph kind={null} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('spins, unless the person asks for reduced motion', () => {
    mockReducedMotion(false);
    const { unmount } = render(<ConversationActivityGlyph kind="running" />);
    expect(screen.getByTestId('conversation-activity-glyph')).toHaveAttribute('data-motion', 'animated');
    unmount();
    mockReducedMotion(true);
    render(<ConversationActivityGlyph kind="running" />);
    expect(screen.getByTestId('conversation-activity-glyph')).toHaveAttribute('data-motion', 'static');
  });
});

describe('ActivityGroupSummary', () => {
  it('shows the count and "N needs you" only when there is one', () => {
    const { rerender } = render(<ActivityGroupSummary count={4} needsYou={1} />);
    expect(screen.getByTestId('activity-needs-you')).toHaveTextContent('1 needs you');
    expect(screen.getByTestId('activity-row-count')).toHaveTextContent('4');
    rerender(<ActivityGroupSummary count={4} needsYou={0} />);
    expect(screen.queryByTestId('activity-needs-you')).toBeNull();
    rerender(<ActivityGroupSummary needsYou={0} />);
    expect(screen.queryByTestId('activity-group-summary')).toBeNull();
  });
});
