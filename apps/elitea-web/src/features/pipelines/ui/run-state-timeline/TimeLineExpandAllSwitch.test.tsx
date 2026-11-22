import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { TimeLineExpandAllSwitch } from './TimeLineExpandAllSwitch';

describe('TimeLineExpandAllSwitch', () => {
  it('renders a switch labeled "Expand all steps" and emits the next boolean state', () => {
    const onChange = vi.fn();
    renderWithTheme(
      <TimeLineExpandAllSwitch
        checked={false}
        onChange={onChange}
      />,
    );

    expect(screen.getByText('Expand all steps')).toBeInTheDocument();
    const toggle = screen.getByTestId('pipeline-run-details-timeline-expand-all');
    expect(toggle).toHaveAttribute('role', 'switch');
    expect(toggle).not.toBeChecked();

    fireEvent.click(toggle);
    expect(onChange).toHaveBeenCalledWith(true);
  });
});
