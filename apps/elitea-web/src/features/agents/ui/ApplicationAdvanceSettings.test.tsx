import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen } from '@testing-library/react';

import { renderWithProviders } from '../__tests__/testUtils';

import { ApplicationAdvanceSettings } from './ApplicationAdvanceSettings';

describe('ApplicationAdvanceSettings', () => {
  it('renders the current step limit', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={25}
        onStepLimitChange={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue('25')).toBeInTheDocument();
  });

  it('clamps a step limit above MAX_STEP_LIMIT (999)', () => {
    const onStepLimitChange = vi.fn();
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={0}
        onStepLimitChange={onStepLimitChange}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Step limit/i), { target: { value: '5000' } });
    expect(onStepLimitChange).toHaveBeenCalledWith(999);
  });

  it('reports undefined for a cleared field', () => {
    const onStepLimitChange = vi.fn();
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={onStepLimitChange}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Step limit/i), { target: { value: '' } });
    expect(onStepLimitChange).toHaveBeenCalledWith(undefined);
  });

  it('renders the model-settings slot above the step limit', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={vi.fn()}
        modelSettingsSlot={<div>MODEL PICKER</div>}
      />,
    );
    const slot = screen.getByText('MODEL PICKER');
    expect(slot).toBeVisible();
    // Ordering matters to the read: the picker names the model, the field
    // under it caps that model's tool loop.
    expect(slot.compareDocumentPosition(screen.getByLabelText(/Step limit/i))).toBe(
      Node.DOCUMENT_POSITION_FOLLOWING,
    );
  });

  it('does not render the ignore-project-context toggle unless showIgnoreProjectContext is set', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={vi.fn()}
      />,
    );
    expect(screen.queryByText(/Ignore Project Context/i)).not.toBeInTheDocument();
  });

  it('toggles ignoreProjectContext via the checkbox when shown', () => {
    const onIgnoreProjectContextChange = vi.fn();
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={vi.fn()}
        showIgnoreProjectContext
        ignoreProjectContext={false}
        onIgnoreProjectContextChange={onIgnoreProjectContextChange}
      />,
    );
    fireEvent.click(screen.getByRole('checkbox'));
    expect(onIgnoreProjectContextChange).toHaveBeenCalledWith(true);
  });

  it('does nothing when the toggle changes but no onIgnoreProjectContextChange handler is supplied', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={vi.fn()}
        showIgnoreProjectContext
        ignoreProjectContext={false}
      />,
    );
    // Should not throw when the optional handler is absent.
    expect(() => fireEvent.click(screen.getByRole('checkbox'))).not.toThrow();
  });

  it('clamps a negative step limit up to MIN_STEP_LIMIT (0)', () => {
    const onStepLimitChange = vi.fn();
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={onStepLimitChange}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Step limit/i), { target: { value: '-50' } });
    expect(onStepLimitChange).toHaveBeenCalledWith(0);
  });

  it('reports undefined for a non-numeric value', () => {
    const onStepLimitChange = vi.fn();
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={onStepLimitChange}
      />,
    );
    fireEvent.change(screen.getByLabelText(/Step limit/i), { target: { value: 'abc' } });
    expect(onStepLimitChange).toHaveBeenCalledWith(undefined);
  });

  it('disables the step limit input and checkbox when disabled is set', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={10}
        onStepLimitChange={vi.fn()}
        showIgnoreProjectContext
        ignoreProjectContext={false}
        onIgnoreProjectContextChange={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByLabelText(/Step limit/i)).toBeDisabled();
    expect(screen.getByRole('checkbox')).toBeDisabled();
  });

  describe('keydown filtering on the step limit input', () => {
    it('allows navigation keys such as Backspace', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={25}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      const notCancelled = fireEvent.keyDown(input, { key: 'Backspace' });
      expect(notCancelled).toBe(true);
    });

    it('allows a digit that keeps the value within MAX_STEP_LIMIT', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={25}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      // current value is "25"; appending "9" -> "259" <= 999
      const notCancelled = fireEvent.keyDown(input, { key: '9' });
      expect(notCancelled).toBe(true);
    });

    it('blocks a digit that would push the value above MAX_STEP_LIMIT', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={999}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      // current value is "999"; appending "9" -> "9999" > 999
      const cancelled = fireEvent.keyDown(input, { key: '9' });
      expect(cancelled).toBe(false);
    });

    it('blocks a non-digit, non-navigation key', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={25}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      const cancelled = fireEvent.keyDown(input, { key: 'a' });
      expect(cancelled).toBe(false);
    });

    it('allows any key when a modifier (ctrl/meta) is held', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={999}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      const notCancelled = fireEvent.keyDown(input, { key: 'a', ctrlKey: true });
      expect(notCancelled).toBe(true);
    });

    it('allows any key when the meta modifier is held', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={999}
          onStepLimitChange={vi.fn()}
        />,
      );
      const input = screen.getByLabelText(/Step limit/i);
      const notCancelled = fireEvent.keyDown(input, { key: 'a', metaKey: true });
      expect(notCancelled).toBe(true);
    });
  });

  describe('output contract (#1500)', () => {
    it('is not shown unless the caller opts in', () => {
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={25}
          onStepLimitChange={vi.fn()}
          outputContract={{ type: 'regex', pattern: 'a' }}
          onOutputContractChange={vi.fn()}
        />,
      );
      expect(screen.queryByTestId('agent-output-contract')).not.toBeInTheDocument();
    });

    it('shows the stored contract and writes an edit', () => {
      const onOutputContractChange = vi.fn();
      renderWithProviders(
        <ApplicationAdvanceSettings
          stepLimit={25}
          onStepLimitChange={vi.fn()}
          outputContract={{ type: 'regex', pattern: 'a' }}
          onOutputContractChange={onOutputContractChange}
          showOutputContract
        />,
      );
      const pattern = screen.getByTestId('agent-output-contract-pattern');
      expect(pattern).toHaveValue('a');
      fireEvent.change(pattern, { target: { value: '^b$' } });
      expect(onOutputContractChange).toHaveBeenCalledWith({ type: 'regex', pattern: '^b$' });
    });
  });
});
