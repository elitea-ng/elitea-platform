import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { writeStepLimit } from '@/shared/desktop/stepLimit';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ComposerMoreMenu } from '@/shared/ui/native-composer';

import StepLimitMenu, { prefetchStepLimit } from './StepLimitMenu';

/** The step limit as the composer offers it: in its ⋯ menu. */
function renderInMore(onError = vi.fn(), workspaceId = 'w1') {
  return renderWithTheme(<ComposerMoreMenu>{(close) => <StepLimitMenu workspaceId={workspaceId} onError={onError} close={close} />}</ComposerMoreMenu>);
}

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

describe('StepLimitMenu', () => {
  afterEach(() => {
    delete (globalThis as { __TAURI_INTERNALS__?: unknown }).__TAURI_INTERNALS__;
  });

  it('shows the limit in the ⋯ menu, sets a preset, unlimited or a number of your own, and follows /steps', async () => {
    const host = fakeHost({ step_limit: null, diagnostics: false });
    const user = userEvent.setup();
    renderInMore();
    const more = screen.getByTestId('composer-more');
    const openSteps = async (): Promise<void> => {
      await user.click(more);
      await user.click(await screen.findByTestId('steps-chip'));
    };

    await user.click(more);
    const row = await screen.findByTestId('steps-chip');
    expect(row).toHaveTextContent('Step limit');
    expect(row).toHaveTextContent('1024 steps');
    await user.click(row);
    // The row opens the choices beside it, the current one checked.
    expect(await screen.findByTestId('steps-chip-default')).toHaveAttribute('aria-checked', 'true');
    await user.click(screen.getByTestId('steps-chip-unlimited'));
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    expect(host.settings()).toEqual({ step_limit: 'unlimited', diagnostics: false });
    await user.click(more);
    await waitFor(() => expect(screen.getByTestId('steps-chip')).toHaveTextContent('∞ steps'));
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());

    await openSteps();
    await user.click(await screen.findByTestId('steps-chip-custom'));
    const input = screen.getByTestId('steps-chip-custom-input');
    await user.type(input, '0');
    expect(screen.getByTestId('steps-chip-custom-set')).toBeDisabled();
    await user.clear(input);
    await user.type(input, '3000{Enter}');
    await waitFor(() => expect(host.settings()['step_limit']).toBe(3000));
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());

    // `/steps` (or anything else that stores the limit) moves the row too.
    await writeStepLimit('w1', null);
    await user.click(more);
    await waitFor(() => expect(screen.getByTestId('steps-chip')).toHaveTextContent('1024 steps'));
  });

  it('opens whole once the limit was read ahead: the row is there on the first frame', async () => {
    fakeHost({ step_limit: 500 });
    await act(async () => {
      prefetchStepLimit('w-ahead');
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    renderInMore(vi.fn(), 'w-ahead');
    fireEvent.click(screen.getByTestId('composer-more'));
    expect(screen.getByTestId('steps-chip')).toHaveTextContent('500 steps');
  });

  it('offers no step limit outside the desktop host', async () => {
    renderInMore(vi.fn(), 'w-outside');
    await userEvent.click(screen.getByTestId('composer-more'));
    await screen.findByRole('menu');
    expect(screen.queryByTestId('steps-chip')).toBeNull();
  });
});
