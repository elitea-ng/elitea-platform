import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

import { renderWithProviders } from '../__tests__/testUtils';

import { AgentInternalToolSwitch } from './AgentInternalToolSwitch';

describe('AgentInternalToolSwitch', () => {
  it('renders the title and the resolved icon', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="pyodide"
        title="Python sandbox"
        icon="PythonIcon"
        checked={false}
        onCheckedChange={vi.fn()}
      />,
    );
    expect(screen.getByText('Python sandbox')).toBeInTheDocument();
  });

  it('renders without crashing for an unknown icon key', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="mystery_tool"
        title="Mystery tool"
        icon="NotARealIcon"
        checked={false}
        onCheckedChange={vi.fn()}
      />,
    );
    expect(screen.getByText('Mystery tool')).toBeInTheDocument();
  });

  it('reflects the checked prop', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="pyodide"
        title="Python sandbox"
        icon="PythonIcon"
        checked
        onCheckedChange={vi.fn()}
      />,
    );
    expect(screen.getByRole('switch')).toBeChecked();
  });

  it('calls onCheckedChange with the new value when toggled', () => {
    const onCheckedChange = vi.fn();
    renderWithProviders(
      <AgentInternalToolSwitch
        name="pyodide"
        title="Python sandbox"
        icon="PythonIcon"
        checked={false}
        onCheckedChange={onCheckedChange}
      />,
    );
    fireEvent.click(screen.getByRole('switch'));
    expect(onCheckedChange).toHaveBeenCalledWith(true);
  });

  it('disables the switch when disabled is true', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="pyodide"
        title="Python sandbox"
        icon="PythonIcon"
        checked={false}
        onCheckedChange={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByRole('switch')).toBeDisabled();
  });

  it('keys the testid off the canonical name, not the display title', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="lazy_tools_mode"
        title="Smart Tools Selection"
        icon="ToolsIcon"
        checked={false}
        onCheckedChange={vi.fn()}
      />,
    );
    // The chat-stream journey picks one switch out of eight by this handle and
    // then asserts the SAME string survives into `meta.internal_tools`. Keyed
    // off the title it would read `internal-tool-smart-tools-selection` and
    // move the moment that copy is reworded or translated — a journey failure
    // about a word rather than about the feature.
    expect(screen.getByTestId('internal-tool-lazy_tools_mode')).toBeInTheDocument();
    expect(screen.queryByTestId('internal-tool-smart-tools-selection')).toBeNull();
  });

  it('renders an info tooltip when infoTooltip.text is supplied', () => {
    renderWithProviders(
      <AgentInternalToolSwitch
        name="pyodide"
        title="Python sandbox"
        icon="PythonIcon"
        checked={false}
        onCheckedChange={vi.fn()}
        infoTooltip={{ text: 'Runs code in a sandbox' }}
      />,
    );
    expect(screen.getByRole('button', { name: 'Runs code in a sandbox' })).toBeInTheDocument();
  });
});
