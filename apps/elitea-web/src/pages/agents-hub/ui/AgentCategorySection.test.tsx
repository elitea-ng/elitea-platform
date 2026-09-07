import { screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { ApplicationData } from '../types';

import AgentCategorySection from './AgentCategorySection';

function makeItems(count: number): ApplicationData[] {
  return Array.from({ length: count }, (_, i) => ({
    project_id: '1',
    id: `app-${i}`,
    name: `Agent ${i}`,
    description: '',
    version_id: `v-${i}`,
    version_name: 'v1',
    agent_type: 'agent',
    meta: null,
    tags: [],
    likes: 0,
    is_liked: false,
  }));
}

/** MUI's `useMediaQuery` treats an absent `window.matchMedia` (jsdom's real default) as "unsupported" and returns `defaultMatches` (false) — so leaving this unmocked already exercises the "not a large screen" branch. Mocking it lets the "is a large screen" branch be exercised too. */
function mockMatchMedia(matches: boolean): void {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
}

describe('AgentCategorySection (adversarial-review fix, cluster A13-agents-hub, finding 10)', () => {
  afterEach(() => {
    Reflect.deleteProperty(window, 'matchMedia');
  });

  it('shows INITIAL_CARD_DISPLAY_COUNT.DEFAULT (6) cards on a normal-width viewport', () => {
    renderWithTheme(<AgentCategorySection category="Productivity" items={makeItems(10)} />);
    expect(screen.getAllByText(/^Agent \d+$/)).toHaveLength(6);
    expect(screen.getByText('Show more')).toBeInTheDocument();
  });

  it('shows INITIAL_CARD_DISPLAY_COUNT.LARGE_SCREEN (8) cards once the prompt_list_xl breakpoint matches — previously hardcoded to 8 regardless of viewport', () => {
    mockMatchMedia(true);
    renderWithTheme(<AgentCategorySection category="Productivity" items={makeItems(10)} />);
    expect(screen.getAllByText(/^Agent \d+$/)).toHaveLength(8);
  });

  it('emits a responsive 1/2/3/4-column grid (multiple breakpoint rules) instead of a single fixed 4-column rule', () => {
    renderWithTheme(<AgentCategorySection category="Productivity" items={makeItems(4)} />);
    const css = Array.from(document.querySelectorAll('style'))
      .map(style => style.textContent ?? '')
      .join('\n');
    expect(css).toContain('grid-template-columns:1fr');
    expect(css).toMatch(/@media \(min-width:\s*600px\)/);
    expect(css).toMatch(/@media \(min-width:\s*1800px\)/);
  });

  it('does not show every card at once when there are more than the initial count', () => {
    renderWithTheme(<AgentCategorySection category="Productivity" items={makeItems(10)} />);
    expect(screen.queryByText('Agent 9')).not.toBeInTheDocument();
  });
});
