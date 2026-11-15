import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ListSelectionSection } from './ListSelectionSection';

function renderSection(props: Partial<React.ComponentProps<typeof ListSelectionSection>> = {}) {
  return renderWithTheme(
    <ListSelectionSection
      testIdPrefix="agent"
      count={3}
      noun="agents"
      onClear={() => undefined}
      {...props}
    />,
  );
}

describe('ListSelectionSection', () => {
  it('says how many rows are selected, as text', () => {
    renderSection();
    expect(screen.getByTestId('agent-selection-count')).toHaveTextContent('3 agents selected');
  });

  it('offers only the actions the page supports', () => {
    renderSection({ onDelete: () => undefined });
    expect(screen.getByTestId('agent-selection-delete')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-selection-add-tag')).toBeNull();
    expect(screen.queryByTestId('agent-selection-export')).toBeNull();
  });

  it('adds one tag to the selection through a small form', async () => {
    const onAddTag = vi.fn();
    const user = userEvent.setup();
    renderSection({ onAddTag });

    await user.click(screen.getByTestId('agent-selection-add-tag'));
    await user.type(screen.getByTestId('agent-selection-tag-input'), '  finance ');
    await user.click(screen.getByTestId('agent-selection-tag-apply'));

    expect(onAddTag).toHaveBeenCalledWith('finance');
    expect(screen.queryByTestId('agent-selection-tag-input')).toBeNull();
  });

  it('does not apply an empty tag', async () => {
    const onAddTag = vi.fn();
    const user = userEvent.setup();
    renderSection({ onAddTag });

    await user.click(screen.getByTestId('agent-selection-add-tag'));

    expect(screen.getByTestId('agent-selection-tag-apply')).toBeDisabled();
    expect(onAddTag).not.toHaveBeenCalled();
  });

  it('runs Export and Delete, and clears the selection', async () => {
    const onExport = vi.fn();
    const onDelete = vi.fn();
    const onClear = vi.fn();
    const user = userEvent.setup();
    renderSection({ onExport, onDelete, onClear });

    await user.click(screen.getByTestId('agent-selection-export'));
    await user.click(screen.getByTestId('agent-selection-delete'));
    await user.click(screen.getByTestId('agent-selection-clear'));

    expect(onExport).toHaveBeenCalled();
    expect(onDelete).toHaveBeenCalled();
    expect(onClear).toHaveBeenCalled();
  });

  it('waits while an action runs, and says a refusal as an alert', () => {
    renderSection({ onDelete: () => undefined, busy: true, error: '1 agents could not be deleted.' });
    expect(screen.getByTestId('agent-selection-delete')).toBeDisabled();
    expect(screen.getByRole('alert')).toHaveTextContent('could not be deleted');
  });
});
