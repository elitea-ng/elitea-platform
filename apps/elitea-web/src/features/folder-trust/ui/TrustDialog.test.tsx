import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { testTimeout } from '@/test/timeouts';

import { TRUST_CONFIRMATION_VERSION } from '../model/trustCopy';
import { TrustDialog } from './TrustDialog';
import { fakeTrustIpc, renderWithTrust, UNTRUSTED } from './trustTestKit';

describe('TrustDialog (ADR-0061 decision 5a)', { timeout: testTimeout(20_000) }, () => {
  it('says the four statements, and Trust waits for the tick', async () => {
    const ipc = fakeTrustIpc(UNTRUSTED);
    const onClose = vi.fn();
    const user = userEvent.setup({ delay: null });
    renderWithTrust(<TrustDialog open workspaceName="repo" status={UNTRUSTED} onClose={onClose} />, ipc);
    const statements = within(screen.getByTestId('trust-dialog-statements')).getAllByRole('listitem');
    expect(statements.map((item) => item.textContent)).toEqual([
      'Commands will be able to read and change any file your user account can, including keys, cloud logins and this app’s own data.',
      'They can reach any network host.',
      'Content in this folder, and anything an agent reads, can steer what the agent asks to run.',
      'Trust only folders whose content you trust, and keep approvals on for anything you would not run yourself.',
    ]);
    expect(screen.getByRole('heading', { name: /Trust “repo”/ })).toBeInTheDocument();
    expect(screen.queryByTestId('trust-dialog-no-sandbox')).toBeNull();
    const confirm = screen.getByTestId('trust-dialog-confirm');
    expect(confirm).toBeDisabled();
    await user.click(screen.getByLabelText(/Run the commands of “repo” without a sandbox/));
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await vi.waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(ipc.grant).toHaveBeenCalledWith('w1', TRUST_CONFIRMATION_VERSION, false);
  });

  it('sends "Ask for every command" from the advanced options', async () => {
    const ipc = fakeTrustIpc(UNTRUSTED);
    const user = userEvent.setup({ delay: null });
    renderWithTrust(<TrustDialog open workspaceName="repo" status={UNTRUSTED} onClose={vi.fn()} />, ipc);
    await user.click(screen.getByRole('button', { name: 'Advanced options' }));
    await user.click(screen.getByLabelText('Ask for every command'));
    await user.click(screen.getByLabelText(/Run the commands of “repo”/));
    await user.click(screen.getByTestId('trust-dialog-confirm'));
    await vi.waitFor(() => expect(ipc.grant).toHaveBeenCalledWith('w1', TRUST_CONFIRMATION_VERSION, true));
  });

  it('says why the policy refuses, and offers no Trust', () => {
    const blocked = { ...UNTRUSTED, allowed_by_policy: false, policy_blocks: ['ceiling' as const, 'folder_trust' as const] };
    renderWithTrust(<TrustDialog open workspaceName="repo" status={blocked} onClose={vi.fn()} />, fakeTrustIpc(blocked));
    const reasons = screen.getByTestId('trust-dialog-blocked');
    expect(reasons).toHaveTextContent('its ceiling is below full access');
    expect(reasons).toHaveTextContent('Your organisation has turned trusted folders off.');
    expect(screen.queryByTestId('trust-dialog-confirm')).toBeNull();
    expect(screen.queryByRole('checkbox')).toBeNull();
  });

  it('says that Windows has no sandbox to fall back to', () => {
    const windows = { ...UNTRUSTED, platform: 'windows' as const, sandbox_available: false };
    renderWithTrust(<TrustDialog open workspaceName="repo" status={windows} onClose={vi.fn()} />, fakeTrustIpc(windows));
    expect(screen.getByTestId('trust-dialog-no-sandbox')).toHaveTextContent('Windows has no sandbox to fall back to');
  });

  it('refuses a task worktree: it follows its folder', () => {
    const worktree = { ...UNTRUSTED, inherited_from: 'root', grantable: false };
    renderWithTrust(<TrustDialog open workspaceName="task" status={worktree} onClose={vi.fn()} />, fakeTrustIpc(worktree));
    expect(screen.getByTestId('trust-dialog-inherited')).toHaveTextContent('follows the trust of the folder it was made from');
    expect(screen.queryByTestId('trust-dialog-confirm')).toBeNull();
  });
});
