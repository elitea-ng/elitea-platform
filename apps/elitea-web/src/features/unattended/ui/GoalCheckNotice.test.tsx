import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { GoalCheckNotice } from './GoalCheckNotice';

describe('GoalCheckNotice', () => {
  it.each([
    [{ outcome: 'met', conditions: [], met: 3, total: 3 }, 'Done · 3 of 3 checks · model-reviewed'],
    [{ outcome: 'not_met', conditions: [], met: 1, total: 3, reason: 'Test 2 still fails' }, 'Not done yet: Test 2 still fails'],
    [{ outcome: 'not_met', conditions: [], met: 1, total: 3 }, 'Not done yet'],
    [{ outcome: 'blocked', conditions: [], met: 0, total: 2, reason: 'No access to the repo' }, 'Blocked on: No access to the repo'],
    [{ outcome: 'not_checked', conditions: [], met: 0, total: 0 }, 'Not checked'],
  ] as const)('shows %o as "%s"', (check, text) => {
    renderWithTheme(<GoalCheckNotice check={{ ...check, conditions: [] }} />);
    const notice = screen.getByTestId('goal-check-notice');
    expect(notice).toHaveAttribute('data-outcome', check.outcome);
    expect(notice).toHaveTextContent(new RegExp(`^${text.replace(/[.*+?^${}()|[\]\\]/gu, '\\$&')}$`, 'u'));
  });

  it('shows nothing without a check', () => {
    renderWithTheme(<GoalCheckNotice check={undefined} />);
    expect(screen.queryByTestId('goal-check-notice')).toBeNull();
  });
});
