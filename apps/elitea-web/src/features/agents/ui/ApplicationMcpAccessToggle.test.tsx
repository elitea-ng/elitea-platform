import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ApplicationMcpAccessToggle } from './ApplicationMcpAccessToggle';

describe('ApplicationMcpAccessToggle', () => {
  it('shows the current external MCP exposure state', () => {
    const { getByRole } = renderWithTheme(<ApplicationMcpAccessToggle checked onChange={() => {}} entityType="agent" />);

    expect(getByRole('switch', { name: 'Enable MCP access' })).toBeChecked();
  });

  it('reports the next state to the owning version form', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(<ApplicationMcpAccessToggle checked={false} onChange={onChange} entityType="pipeline" />);

    await user.click(getByRole('switch', { name: 'Enable MCP access' }));

    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it('does not change a read-only version', () => {
    const onChange = vi.fn();
    const { getByRole } = renderWithTheme(
      <ApplicationMcpAccessToggle checked={false} onChange={onChange} disabled entityType="pipeline" />,
    );
    const toggle = getByRole('switch', { name: 'Enable MCP access' });

    expect(toggle).toBeDisabled();
    expect(onChange).not.toHaveBeenCalled();
  });
});
