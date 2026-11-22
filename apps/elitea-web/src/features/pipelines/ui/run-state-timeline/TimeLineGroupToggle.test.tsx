import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TimeLineGroupToggle } from './TimeLineGroupToggle';

describe('TimeLineGroupToggle', () => {
  it('renders an accessible button with aria-expanded and invokes onToggle', () => {
    const onToggle = vi.fn();
    renderWithTheme(
      <TimeLineGroupToggle
        label="Expand Sub Pipeline (3 steps)"
        expanded={false}
        onToggle={onToggle}
        testId="pipeline-run-details-timeline-group-toggle-1"
      />,
    );

    const button = screen.getByTestId('pipeline-run-details-timeline-group-toggle-1');
    expect(button).toHaveAttribute('aria-label', 'Expand Sub Pipeline (3 steps)');
    expect(button).toHaveAttribute('aria-expanded', 'false');

    fireEvent.click(button);
    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it('reflects expanded=true state on aria-expanded', () => {
    renderWithTheme(
      <TimeLineGroupToggle
        label="Collapse Sub Pipeline (3 steps)"
        expanded
        onToggle={vi.fn()}
      />,
    );

    const button = screen.getByRole('button', { name: 'Collapse Sub Pipeline (3 steps)' });
    expect(button).toHaveAttribute('aria-expanded', 'true');
  });
});
