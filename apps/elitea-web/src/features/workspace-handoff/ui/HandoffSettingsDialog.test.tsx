/**
 * A folder's handoff settings, against the in-memory host: the policy's
 * exclusions read-only, the folder's own patterns, includes and env names
 * added, removed and saved, and the host's validation shown inline.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createFakeHandoffIpc, DEFAULT_POLICY, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';

import { HandoffSettingsDialog } from './HandoffSettingsDialog';
import { renderWithClient } from './testing';

function mount(ipc: FakeHandoffIpc, onClose = vi.fn()): { onClose: typeof onClose } {
  renderWithClient(<HandoffSettingsDialog open ipc={ipc} workspaceId="w1" onClose={onClose} />);
  return { onClose };
}

const saveButton = (): HTMLElement => screen.getByRole('button', { name: 'Save' });

describe('HandoffSettingsDialog', () => {
  it('shows the platform, project and hard-deny patterns read-only, next to the folder’s own', async () => {
    const ipc = createFakeHandoffIpc({
      policy: { exclusions: { platform: ['.env', '*.pem'], project: ['vendor/'], user: [], path_deny: ['secrets/'] } },
      settings: { w1: { exclude_patterns: ['dist/'] } },
    });
    mount(ipc);
    expect(await screen.findByTestId('handoff-settings-platform')).toHaveTextContent('.env, *.pem');
    expect(screen.getByTestId('handoff-settings-project')).toHaveTextContent('vendor/');
    expect(screen.getByTestId('handoff-settings-path-deny')).toHaveTextContent('secrets/');
    expect(within(screen.getByTestId('handoff-settings-exclude')).getByText('dist/')).toBeInTheDocument();
    // The read-only ones cannot be removed.
    expect(screen.queryByRole('button', { name: 'Remove .env' })).toBeNull();
    // Nothing changed yet: nothing to save.
    expect(saveButton()).toBeDisabled();
  });

  it('adds and removes patterns and includes, then saves them', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ settings: { w1: { exclude_patterns: ['dist/'] } } });
    const { onClose } = mount(ipc);
    const exclude = await screen.findByTestId('handoff-settings-exclude');
    await user.type(within(exclude).getByRole('textbox', { name: 'Pattern to leave out' }), '/build');
    await user.click(within(exclude).getByRole('button', { name: 'Add' }));
    await user.click(within(exclude).getByRole('button', { name: 'Remove dist/' }));
    const include = screen.getByTestId('handoff-settings-include');
    await user.type(within(include).getByRole('textbox', { name: 'Path or pattern to include' }), '.env.example{Enter}');
    expect(within(include).getByText('.env.example')).toBeInTheDocument();

    await user.click(saveButton());
    await waitFor(() => expect(ipc.calls.settingsSets).toEqual([{ workspaceId: 'w1', settings: { exclude_patterns: ['/build'], include: ['.env.example'], env_names: [] } }]));
    await waitFor(() => expect(onClose).toHaveBeenCalledTimes(1));
    expect((await ipc.settingsGet('w1')).settings.include).toEqual(['.env.example']);
  });

  it('shows the host’s refusal inline, next to its section, and saves nothing', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    const { onClose } = mount(ipc);
    const include = await screen.findByTestId('handoff-settings-include');
    await user.type(within(include).getByRole('textbox', { name: 'Path or pattern to include' }), '../outside{Enter}');
    await user.click(saveButton());

    const error = await within(screen.getByTestId('handoff-settings-includes')).findByTestId('handoff-settings-error');
    expect(error).toHaveTextContent('The include ../outside is outside the folder.');
    expect(onClose).not.toHaveBeenCalled();
    expect((await ipc.settingsGet('w1')).settings.include).toEqual([]);
    // Editing again clears it.
    await user.click(within(include).getByRole('button', { name: 'Remove ../outside' }));
    expect(screen.queryByTestId('handoff-settings-error')).toBeNull();
  });

  it('shows a refusal without a field at the top', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('settingsSet', 'invalid_request', 'The settings could not be read.');
    mount(ipc);
    const exclude = await screen.findByTestId('handoff-settings-exclude');
    await user.type(within(exclude).getByRole('textbox', { name: 'Pattern to leave out' }), 'tmp/{Enter}');
    await user.click(saveButton());
    expect(await screen.findByTestId('handoff-settings-error')).toHaveTextContent('The settings could not be read.');
  });

  it('hides includes with a note when the policy does not allow them', async () => {
    mount(createFakeHandoffIpc({ policy: { allow_user_include: false } }));
    expect(await screen.findByTestId('handoff-settings-include-off')).toHaveTextContent('does not allow including files');
    expect(screen.queryByTestId('handoff-settings-include')).toBeNull();
  });

  it('offers env names (masked lengths, never values) only when the policy harvests them, and saves the selection', async () => {
    const user = userEvent.setup();
    const offered = [{ name: 'API_URL', length: 23 }, { name: 'TOKEN', length: 40 }];
    const off = createFakeHandoffIpc({ envOffered: offered });
    const { unmount } = renderWithClient(<HandoffSettingsDialog open ipc={off} workspaceId="w1" onClose={vi.fn()} />);
    await screen.findByTestId('handoff-settings-exclusions');
    expect(screen.queryByTestId('handoff-settings-env')).toBeNull();
    unmount();

    const ipc = createFakeHandoffIpc({ policy: { ...DEFAULT_POLICY, env_harvest: true }, envOffered: offered, settings: { w1: { env_names: ['TOKEN'] } } });
    mount(ipc);
    const env = await screen.findByTestId('handoff-settings-env');
    expect(env).toHaveTextContent('API_URL •••••• (23 chars)');
    expect(within(env).getByRole('checkbox', { name: /TOKEN/ })).toBeChecked();
    expect(within(env).getByRole('checkbox', { name: /API_URL/ })).not.toBeChecked();
    await user.click(within(env).getByRole('checkbox', { name: /API_URL/ }));
    await user.click(within(env).getByRole('checkbox', { name: /TOKEN/ }));
    await user.click(saveButton());
    await waitFor(() => expect(ipc.calls.settingsSets.at(-1)?.settings.env_names).toEqual(['API_URL']));
  });
});
