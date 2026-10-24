import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { writeStepLimit } from '@/shared/desktop/stepLimit';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import StepsChip from './StepsChip';

/** The host's agent settings, as `workspace_agent_settings_get` / `_set` keep them. */
function fakeHost(initial: Record<string, unknown>) {
  let settings = { ...initial };
  const invoke = vi.fn((command: string, args: { settings?: Record<string, unknown> }) => {
    if (command === 'workspace_agent_settings_set' && args.settings !== undefined) settings = args.settings;
    return Promise.resolve(settings);
  });
  (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__ = { invoke };
  return { settings: () => settings };
}

describe('StepsChip', () => {
  afterEach(() => {
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  it('shows the limit, sets a preset, unlimited or a number of your own, and follows /steps', async () => {
    const host = fakeHost({ step_limit: null, diagnostics: false });
    const user = userEvent.setup();
    renderWithTheme(<StepsChip workspaceId="w1" onError={vi.fn()} />);
    const chip = await screen.findByTestId('steps-chip');
    expect(chip).toHaveTextContent('1024 steps');

    await user.click(chip);
    await user.click(screen.getByTestId('steps-chip-unlimited'));
    await waitFor(() => expect(chip).toHaveTextContent('∞ steps'));
    expect(host.settings()).toEqual({ step_limit: 'unlimited', diagnostics: false });

    await user.click(chip);
    await user.click(screen.getByTestId('steps-chip-custom'));
    const input = screen.getByTestId('steps-chip-custom-input');
    await user.type(input, '0');
    expect(screen.getByTestId('steps-chip-custom-set')).toBeDisabled();
    await user.clear(input);
    await user.type(input, '3000{Enter}');
    await waitFor(() => expect(chip).toHaveTextContent('3000 steps'));
    expect(host.settings()['step_limit']).toBe(3000);

    // `/steps` (or anything else that stores the limit) moves the chip too.
    await writeStepLimit('w1', null);
    await waitFor(() => expect(chip).toHaveTextContent('1024 steps'));
  });

  it('shows nothing outside the desktop host', () => {
    renderWithTheme(<StepsChip workspaceId="w1" onError={vi.fn()} />);
    expect(screen.queryByTestId('steps-chip')).toBeNull();
  });
});
