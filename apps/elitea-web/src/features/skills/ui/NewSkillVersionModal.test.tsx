import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithProviders } from '../__tests__/testUtils';
import { NewSkillVersionModal } from './NewSkillVersionModal';

function modal(overrides: Partial<React.ComponentProps<typeof NewSkillVersionModal>> = {}): React.ReactElement {
  return (
    <NewSkillVersionModal
      open
      existingNames={['base']}
      confirming={false}
      onClose={vi.fn()}
      onConfirm={vi.fn()}
      {...overrides}
    />
  );
}

describe('NewSkillVersionModal', () => {
  it('re-seeds the name when the versions list arrives after the dialog opened', () => {
    const { rerender } = renderWithProviders(modal({ existingNames: [] }));
    expect(screen.getByTestId('skill-version-name-input')).toHaveValue('v1');
    rerender(modal({ existingNames: ['base', 'v1', 'v2'] }));
    expect(screen.getByTestId('skill-version-name-input')).toHaveValue('v3');
  });

  it('keeps what the user typed when the list changes', async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithProviders(modal());
    const input = screen.getByTestId('skill-version-name-input');
    await user.clear(input);
    await user.type(input, 'mine');
    rerender(modal({ existingNames: ['base', 'v1'] }));
    expect(screen.getByTestId('skill-version-name-input')).toHaveValue('mine');
  });

  it('drops a server refusal once the name changes, and after a close and reopen', async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithProviders(modal({ error: 'Version already exists.' }));
    expect(screen.getByText('Version already exists.')).toBeInTheDocument();
    await user.type(screen.getByTestId('skill-version-name-input'), 'x');
    expect(screen.queryByText('Version already exists.')).not.toBeInTheDocument();

    rerender(modal({ open: false, error: undefined }));
    rerender(modal({ open: true, error: undefined }));
    expect(screen.queryByText('Version already exists.')).not.toBeInTheDocument();
  });
});
