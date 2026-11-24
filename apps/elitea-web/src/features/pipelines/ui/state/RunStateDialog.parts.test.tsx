import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RunStatus, StateItemView, StateValueModal } from './RunStateDialog.parts';

describe('RunStateDialog.parts', () => {
  it('RunStatus renders the raw status text', () => {
    renderWithTheme(<RunStatus status="Completed" />);
    expect(screen.getByText('Completed')).toBeInTheDocument();
  });

  it('StateItemView renders Before/After sections and wires fullscreen clicks', () => {
    const onFullScreen = vi.fn();
    renderWithTheme(
      <StateItemView
        name="counter"
        valueBefore={1}
        valueAfter={2}
        onFullScreen={onFullScreen}
      />,
    );

    expect(screen.getByText('Before')).toBeInTheDocument();
    expect(screen.getByText('After')).toBeInTheDocument();
    expect(screen.getByText('1')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();

    fireEvent.click(screen.getAllByLabelText('Full screen view')[0] as HTMLElement);
    expect(onFullScreen).toHaveBeenCalledWith('counter', 1);
  });

  it('StateValueModal shows the label and stringified value, and closes', () => {
    const onClose = vi.fn();
    renderWithTheme(
      <StateValueModal
        open
        onClose={onClose}
        label="counter"
        value={{ a: 1 }}
      />,
    );

    expect(screen.getByText('counter')).toBeInTheDocument();
    expect(screen.getByText(JSON.stringify({ a: 1 }))).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
