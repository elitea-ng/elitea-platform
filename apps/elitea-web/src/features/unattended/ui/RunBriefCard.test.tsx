import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { RunBrief } from '@/shared/desktop/unattendedIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RunBriefCard } from './RunBriefCard';

const brief: RunBrief = {
  goal: 'Fix the failing login test',
  done_when: [
    { id: 'c1', text: 'The login test passes' },
    { id: 'c2', text: 'No other test fails' },
    { id: 'c3', text: 'The fix has a unit test' },
  ],
  constraints: ['Do not change the public API'],
  assumptions: ['The test runner is vitest'],
  ambiguity: 'low',
};

describe('RunBriefCard', () => {
  it('is one line collapsed, and opens to the goal, the conditions, the constraints and the assumptions', async () => {
    renderWithTheme(<RunBriefCard value={{ state: 'ready', brief }} />);
    const card = screen.getByTestId('run-brief-card');
    expect(card).toHaveAttribute('data-state', 'ready');
    expect(card).toHaveTextContent('Goal: Fix the failing login test · Done when: 3 checks');
    expect(screen.queryByTestId('run-brief-card-details')).toBeNull();
    const toggle = screen.getByRole('button', { name: 'Show the brief' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await userEvent.setup().click(toggle);
    const details = screen.getByTestId('run-brief-card-details');
    for (const text of ['Fix the failing login test', 'The login test passes', 'No other test fails', 'The fix has a unit test', 'Do not change the public API', 'The test runner is vitest']) {
      expect(details).toHaveTextContent(text);
    }
    expect(screen.getByRole('button', { name: 'Hide the brief' })).toHaveAttribute('aria-expanded', 'true');
  });

  it('says "1 check" for one condition', () => {
    renderWithTheme(<RunBriefCard value={{ state: 'ready', brief: { ...brief, done_when: [{ id: 'c1', text: 'a' }] } }} />);
    expect(screen.getByTestId('run-brief-card')).toHaveTextContent('Done when: 1 check');
  });

  it('shows only the goal line when the brief has no conditions', () => {
    renderWithTheme(<RunBriefCard value={{ state: 'ready', brief: { ...brief, done_when: [] } }} />);
    expect(screen.getByTestId('run-brief-card')).toHaveTextContent(/^Goal: Fix the failing login test$/u);
    expect(screen.queryByRole('button')).toBeNull();
  });

  it('shows a small line while the brief is written, and nothing when it failed, is off or has none', () => {
    const { rerender } = renderWithTheme(<RunBriefCard value={{ state: 'pending' }} />);
    expect(screen.getByTestId('run-brief-card')).toHaveTextContent('Writing the brief…');
    for (const state of ['failed', 'off', 'none'] as const) {
      rerender(<RunBriefCard value={{ state }} />);
      expect(screen.queryByTestId('run-brief-card')).toBeNull();
    }
    rerender(<RunBriefCard value={undefined} />);
    expect(screen.queryByTestId('run-brief-card')).toBeNull();
  });

  it('shows the model text as plain text, never as markup', async () => {
    const markup = '<img src=x onerror=alert(1)> **bold**';
    renderWithTheme(<RunBriefCard value={{ state: 'ready', brief: { ...brief, goal: markup, constraints: [markup] } }} />);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Show the brief' }));
    const card = screen.getByTestId('run-brief-card');
    expect(card.querySelector('img')).toBeNull();
    expect(card.querySelector('strong')).toBeNull();
    expect(screen.getByTestId('run-brief-card-details')).toHaveTextContent(markup);
  });
});
