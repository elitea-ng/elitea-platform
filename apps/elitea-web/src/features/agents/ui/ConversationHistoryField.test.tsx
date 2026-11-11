import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, within } from '@testing-library/react';

import { renderWithProviders } from '../__tests__/testUtils';

import { ApplicationAdvanceSettings } from './ApplicationAdvanceSettings';
import { ConversationHistoryField } from './ConversationHistoryField';

/** The history select is the field's first combobox, the side-talk select its second. */
function pickMode(optionTestId: string): void {
  fireEvent.mouseDown(within(screen.getByTestId('agent-context-history')).getAllByRole('combobox')[0]!);
  fireEvent.click(screen.getByTestId(optionTestId));
}

describe('ConversationHistoryField', () => {
  it('shows the default when the version stores no block', () => {
    renderWithProviders(<ConversationHistoryField value={undefined} onChange={vi.fn()} />);
    expect(screen.getByTestId('agent-context-history-mode')).toHaveValue('full');
    expect(within(screen.getByTestId('agent-context-history')).getAllByRole('combobox')[0]).toHaveTextContent('Everything it may see');
  });

  it.each(['since_joined', 'addressed_only', 'none'] as const)('writes the %s mode', (mode) => {
    const onChange = vi.fn();
    renderWithProviders(<ConversationHistoryField value={undefined} onChange={onChange} />);
    pickMode(`agent-context-history-${mode}`);
    expect(onChange).toHaveBeenCalledWith({ history: mode, include_side_talk: 'party' });
  });

  it('shows a stored mode', () => {
    renderWithProviders(<ConversationHistoryField value={{ history: 'addressed_only', include_side_talk: 'none' }} onChange={vi.fn()} />);
    expect(screen.getByTestId('agent-context-history-mode')).toHaveValue('addressed_only');
    expect(screen.getByTestId('agent-context-side-talk')).toHaveValue('none');
  });

  it('writes null when the default is chosen again, clearing the stored block', () => {
    const onChange = vi.fn();
    renderWithProviders(<ConversationHistoryField value={{ history: 'none', include_side_talk: 'party' }} onChange={onChange} />);
    pickMode('agent-context-history-full');
    expect(onChange).toHaveBeenCalledWith(null);
  });

  it('shows the server’s refusal on the field', () => {
    renderWithProviders(
      <ConversationHistoryField value={undefined} onChange={vi.fn()} error="meta.context.history must be one of full, since_joined, addressed_only, none" />,
    );
    expect(screen.getByTestId('agent-context-history-error')).toHaveTextContent('meta.context.history must be one of');
  });

  it('is mounted by the Advanced panel with the refusal it was given', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={25}
        onStepLimitChange={vi.fn()}
        showOutputContract
        contentControls={{
          citations: { value: null, onChange: vi.fn() },
          contextHistory: { value: { history: 'since_joined', include_side_talk: 'party' }, onChange: vi.fn(), error: 'refused' },
        }}
      />,
    );
    expect(screen.getByTestId('agent-context-history')).toBeInTheDocument();
    expect(screen.getByTestId('agent-context-history-error')).toHaveTextContent('refused');
  });

  it('is not mounted where the page does not save the meta', () => {
    renderWithProviders(
      <ApplicationAdvanceSettings
        stepLimit={25}
        onStepLimitChange={vi.fn()}
        contentControls={{
          citations: { value: null, onChange: vi.fn() },
          contextHistory: { value: undefined, onChange: vi.fn() },
        }}
      />,
    );
    expect(screen.queryByTestId('agent-context-history')).not.toBeInTheDocument();
  });
});
