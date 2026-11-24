import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { AddOptionalParameterButton } from './AddOptionalParameterButton';

describe('AddOptionalParameterButton', () => {
  it('renders nothing when there are no optional parameters to add', () => {
    const { container } = renderWithTheme(
      <AddOptionalParameterButton options={[]} onAdd={vi.fn()} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it('opens a menu of optional parameters and calls onAdd with the chosen key', async () => {
    const onAdd = vi.fn();
    const user = userEvent.setup();
    renderWithTheme(
      <AddOptionalParameterButton
        options={[
          { key: 'timeout_ms', label: 'Timeout ms' },
          { key: 'max_results', label: 'Max results' },
        ]}
        onAdd={onAdd}
        tooltip="Add an optional argument of the selected tool."
        testIdPrefix="pipeline-toolkit-node"
      />,
    );

    await user.click(screen.getByTestId('pipeline-toolkit-node-add-optional-parameter'));
    await user.click(screen.getByTestId('pipeline-toolkit-node-optional-parameter-max_results'));

    expect(onAdd).toHaveBeenCalledTimes(1);
    expect(onAdd).toHaveBeenCalledWith('max_results');
  });

  it('disables the trigger button when disabled is true', () => {
    renderWithTheme(
      <AddOptionalParameterButton
        options={[{ key: 'limit', label: 'Limit' }]}
        onAdd={vi.fn()}
        disabled
        testIdPrefix="pipeline-mcp-node"
      />,
    );
    expect(screen.getByTestId('pipeline-mcp-node-add-optional-parameter')).toBeDisabled();
  });

  it('closes the open menu and blocks selection if disabled flips true while open', async () => {
    const onAdd = vi.fn();
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(
      <AddOptionalParameterButton
        options={[{ key: 'limit', label: 'Limit' }]}
        onAdd={onAdd}
        testIdPrefix="pipeline-mcp-node"
      />,
    );

    await user.click(screen.getByTestId('pipeline-mcp-node-add-optional-parameter'));
    expect(screen.getByTestId('pipeline-mcp-node-optional-parameter-limit')).toBeVisible();

    rerender(
      <AddOptionalParameterButton
        options={[{ key: 'limit', label: 'Limit' }]}
        onAdd={onAdd}
        disabled
        testIdPrefix="pipeline-mcp-node"
      />,
    );
    await waitFor(() =>
      expect(screen.queryByTestId('pipeline-mcp-node-optional-parameter-limit')).not.toBeVisible(),
    );
    expect(onAdd).not.toHaveBeenCalled();
  });
});
