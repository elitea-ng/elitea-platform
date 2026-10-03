import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RunStateDialog, type RunStateDialogData } from './RunStateDialog';

function baseData(overrides: Partial<RunStateDialogData> = {}): RunStateDialogData {
  return {
    status: 'Completed',
    label: 'Run #1',
    timeline: [
      { id: 'start', status: 'Completed', created_at: '2024-01-01T00:00:00.000Z', state: { counter: 0 } },
      { id: 'increment', status: 'Completed', created_at: '2024-01-01T00:00:05.000Z', state: { counter: 1 } },
    ],
    ...overrides,
  };
}

describe('RunStateDialog', () => {
  it('renders the run label, status, and each timeline step', () => {
    renderWithTheme(
      <RunStateDialog
        data={baseData()}
        state={{ counter: { type: 'number' } }}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    expect(screen.getByText('Run #1')).toBeInTheDocument();
    expect(screen.getByText('Completed')).toBeInTheDocument();
    expect(screen.getByText('counter')).toBeInTheDocument();
  });

  it('shows a stop button while in progress and calls onStop when clicked', () => {
    const onStop = vi.fn();
    renderWithTheme(
      <RunStateDialog
        data={baseData({ status: 'In progress' })}
        state={{}}
        open
        onClose={vi.fn()}
        onStop={onStop}
        onDelete={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Stop run' }));
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it('shows a delete button once finished and calls onDelete when clicked', () => {
    const onDelete = vi.fn();
    renderWithTheme(
      <RunStateDialog
        data={baseData({ status: 'Completed' })}
        state={{}}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={onDelete}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Delete run' }));
    expect(onDelete).toHaveBeenCalledTimes(1);
  });

  it('calls onClose from the header close button', () => {
    const onClose = vi.fn();
    renderWithTheme(
      <RunStateDialog
        data={baseData()}
        state={{}}
        open
        onClose={onClose}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    const closeButtons = screen.getAllByRole('button', { name: 'Close' });
    fireEvent.click(closeButtons[0] as HTMLElement);
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('selects a different timeline step on click, updating the "Timeline step:" value', () => {
    renderWithTheme(
      <RunStateDialog
        data={baseData()}
        state={{}}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: 'start' }));
    expect(screen.getByText('Timeline step:').nextSibling).toHaveTextContent('start');
  });

  it('keys timeline steps by position so a re-executed node (repeated id, e.g. a loop) stays independently selectable', () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    renderWithTheme(
      <RunStateDialog
        data={baseData({
          timeline: [
            { id: 'start', status: 'Completed', created_at: '2024-01-01T00:00:00.000Z', state: { counter: 0 } },
            { id: 'loop-node', status: 'Completed', created_at: '2024-01-01T00:00:05.000Z', state: { counter: 1 } },
            { id: 'loop-node', status: 'Completed', created_at: '2024-01-01T00:00:10.000Z', state: { counter: 2 } },
          ],
        })}
        state={{}}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    // React logs a dev warning when sibling elements in a list share a key
    // (`key={step.id}` collides here since both loop entries share the id
    // "loop-node"). No such warning should fire once steps are keyed by
    // their stable position instead.
    const duplicateKeyWarning = consoleError.mock.calls.some(([message]) =>
      typeof message === 'string' && message.includes('same key'),
    );
    expect(duplicateKeyWarning).toBe(false);

    // Both re-executions of the same node render as distinct, independently
    // selectable steps rather than being collapsed into one.
    const loopButtons = screen.getAllByRole('button', { name: 'loop-node' });
    expect(loopButtons).toHaveLength(2);

    fireEvent.click(loopButtons[1] as HTMLElement);
    expect(screen.getByText('Timeline step:').nextSibling).toHaveTextContent('loop-node');

    consoleError.mockRestore();
  });

  it('opens the fullscreen state value modal from a variable accordion', () => {
    renderWithTheme(
      <RunStateDialog
        data={baseData()}
        state={{ counter: { type: 'number' } }}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    fireEvent.click(screen.getByText('counter'));
    const fullScreenButtons = screen.getAllByLabelText('Full screen view');
    fireEvent.click(fullScreenButtons[0] as HTMLElement);

    const dialogs = screen.getAllByRole('dialog');
    const valueDialog = dialogs[dialogs.length - 1] as HTMLElement;
    expect(within(valueDialog).getByText('counter')).toBeInTheDocument();
  });

  it('labels the terminal step of a finished run "Final state" and shows the final value (#6883)', () => {
    renderWithTheme(
      <RunStateDialog
        data={baseData({
          timeline: [
            { id: 'start', status: 'Completed', created_at: '2024-01-01T00:00:00.000Z', state: { answer: 'none yet' } },
            { id: 'agent', status: 'Completed', created_at: '2024-01-01T00:00:05.000Z', state: { answer: 'the final answer' } },
            { id: 'agent', status: 'Completed', created_at: '2024-01-01T00:00:06.000Z', state: {} },
          ],
        })}
        state={{ answer: { type: 'str' } }}
        open
        onClose={vi.fn()}
        onStop={vi.fn()}
        onDelete={vi.fn()}
      />,
    );

    const steps = screen.getAllByRole('button', { name: 'agent' });
    fireEvent.click(steps[1] as HTMLElement);
    expect(screen.getByText('Final state')).toBeInTheDocument();
    expect(screen.queryByText('After')).not.toBeInTheDocument();
    expect(screen.getAllByText('"the final answer"').length).toBeGreaterThan(0);

    // A middle step keeps the plain Before / After pair.
    fireEvent.click(screen.getByRole('button', { name: 'start' }));
    expect(screen.getByText('After')).toBeInTheDocument();
    expect(screen.queryByText('Final state')).not.toBeInTheDocument();
  });
});
