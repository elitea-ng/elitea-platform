import { screen, within } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { PlanView } from '@/shared/desktop/planIpc';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TodoPane } from './TodoPane';

const PLAN: PlanView = {
  title: 'Rename X',
  explanation: 'Callers first, then the definition.',
  status: 'in_progress',
  steps: [
    { n: 1, step: 'Find the callers', status: 'completed' },
    { n: 2, step: 'Rename it', status: 'in_progress' },
    { n: 3, step: '<b>Run the tests</b>', status: 'pending' },
    { n: 4, step: 'Update the docs', status: 'cancelled' },
  ],
};

describe('TodoPane', () => {
  it('says there is no plan yet', () => {
    renderWithTheme(<TodoPane plan={null} />);
    expect(screen.getByTestId('todo-empty')).toBeInTheDocument();
  });

  it('shows the title, the note, progress and each step with its status, as plain text', () => {
    renderWithTheme(<TodoPane plan={PLAN} />);
    const pane = screen.getByTestId('todo-pane');
    expect(within(pane).getByRole('heading', { name: 'Rename X' })).toBeInTheDocument();
    expect(pane).toHaveTextContent('Callers first, then the definition.');
    expect(screen.getByTestId('todo-progress')).toHaveTextContent('1 of 4 done');
    const steps = screen.getAllByTestId('todo-step');
    expect(steps.map((step) => step.getAttribute('data-status'))).toEqual(['completed', 'in_progress', 'pending', 'cancelled']);
    expect(steps[1]).toHaveTextContent('Rename it (In progress)');
    expect(steps[2]).toHaveTextContent('<b>Run the tests</b>');
    expect(pane.querySelector('b')).toBeNull();
  });
});
