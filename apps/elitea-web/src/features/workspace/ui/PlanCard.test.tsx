import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { PlanProposalPayload } from '@/shared/desktop/planIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { MAX_FEEDBACK_CHARS, PlanCard } from './PlanCard';

const PROPOSAL: PlanProposalPayload = {
  request_id: 'r1',
  call_id: 'c1',
  plan: '# Rename X\n\n1. Find the callers\n2. Rename it\n\n<img src=x onerror="alert(1)">',
};

describe('PlanCard', () => {
  it('renders nothing without a proposal', () => {
    renderWithTheme(<PlanCard proposal={undefined} onRespond={vi.fn()} />);
    expect(screen.queryByTestId('plan-card')).not.toBeInTheDocument();
  });

  it('shows the plan as Markdown, with no HTML from the model', () => {
    renderWithTheme(<PlanCard proposal={PROPOSAL} onRespond={vi.fn()} />);
    const plan = screen.getByTestId('plan-card-plan');
    expect(screen.getByRole('heading', { name: 'Rename X' })).toBeInTheDocument();
    expect(plan).toHaveTextContent('Find the callers');
    expect(plan.querySelector('img')).toBeNull();
  });

  it('accepts without feedback, and offers no "always"', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<PlanCard proposal={PROPOSAL} onRespond={onRespond} />);
    await user.type(screen.getByTestId('plan-card-feedback'), 'ignored');
    await user.click(screen.getByRole('button', { name: 'Accept plan' }));
    expect(onRespond.mock.calls).toEqual([['r1', 'accept']]);
    expect(screen.queryByRole('button', { name: /always/i })).not.toBeInTheDocument();
  });

  it('declines with the trimmed feedback, or without any', async () => {
    const onRespond = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(<PlanCard proposal={PROPOSAL} onRespond={onRespond} />);
    await user.click(screen.getByRole('button', { name: 'Decline' }));
    await user.type(screen.getByTestId('plan-card-feedback'), '  Keep the old name  ');
    await user.click(screen.getByRole('button', { name: 'Decline' }));
    expect(onRespond.mock.calls).toEqual([
      ['r1', 'decline'],
      ['r1', 'decline', 'Keep the old name'],
    ]);
  });

  it('bounds the feedback box to what the host keeps', () => {
    renderWithTheme(<PlanCard proposal={PROPOSAL} onRespond={vi.fn()} />);
    expect(screen.getByTestId('plan-card-feedback')).toHaveAttribute('maxlength', String(MAX_FEEDBACK_CHARS));
  });
});
