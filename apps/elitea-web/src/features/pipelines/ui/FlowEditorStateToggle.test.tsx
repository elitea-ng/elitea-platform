import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { FlowEditorStateToggle } from './FlowEditorStateToggle';

describe('FlowEditorStateToggle', () => {
  it('renders the State button when closed', () => {
    renderWithTheme(
      <FlowEditorStateToggle
        isOpen={false}
        onToggle={vi.fn()}
      />,
    );
    expect(screen.getByRole('button', { name: 'State' })).toBeInTheDocument();
  });

  it('renders nothing when the drawer is already open', () => {
    renderWithTheme(
      <FlowEditorStateToggle
        isOpen
        onToggle={vi.fn()}
      />,
    );
    expect(screen.queryByRole('button', { name: 'State' })).not.toBeInTheDocument();
  });

  it('calls onToggle when clicked', async () => {
    const user = userEvent.setup();
    const onToggle = vi.fn();
    renderWithTheme(
      <FlowEditorStateToggle
        isOpen={false}
        onToggle={onToggle}
      />,
    );

    await user.click(screen.getByRole('button', { name: 'State' }));

    expect(onToggle).toHaveBeenCalledTimes(1);
  });

  it('shifts left by rightOffset when the node config drawer is open', () => {
    renderWithTheme(
      <FlowEditorStateToggle
        isOpen={false}
        onToggle={vi.fn()}
        rightOffset={400}
      />,
    );

    const wrapper = screen.getByTestId('pipeline-state-toggle-wrapper');
    expect(wrapper).toHaveAttribute('data-right-offset', '400');
    expect(wrapper).toHaveStyle({ right: '420px' });
  });
});
