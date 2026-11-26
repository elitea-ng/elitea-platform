import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { testTimeout } from '@/test/timeouts';

import { TrustBadge } from './TrustBadge';
import { SandboxUnavailableOffer } from './trustEntryPoints';
import { fakeTrustIpc, renderWithTrust, TRUSTED, UNTRUSTED } from './trustTestKit';

describe('TrustBadge (ADR-0061 decision 5i)', { timeout: testTimeout(20_000) }, () => {
  it('shows nothing for an untrusted folder', async () => {
    const ipc = fakeTrustIpc(UNTRUSTED);
    renderWithTrust(<TrustBadge workspaceId="w1" workspaceName="repo" />, ipc);
    await vi.waitFor(() => expect(ipc.status).toHaveBeenCalledWith('w1'));
    expect(screen.queryByTestId('trust-badge')).toBeNull();
  });

  it('shows "No sandbox", and Revoke revokes', async () => {
    const ipc = fakeTrustIpc(TRUSTED);
    const user = userEvent.setup({ delay: null });
    renderWithTrust(<TrustBadge workspaceId="w1" workspaceName="repo" />, ipc);
    const badge = await screen.findByTestId('trust-badge');
    expect(badge).toHaveTextContent('No sandbox');
    await user.click(badge);
    await user.click(await screen.findByTestId('trust-revoke'));
    await vi.waitFor(() => expect(ipc.revoke).toHaveBeenCalledWith('w1'));
    await vi.waitFor(() => expect(screen.queryByTestId('trust-badge')).toBeNull());
  });

  it('shows "Trust paused" with the reason: the organisation, or the file that changed', async () => {
    const byPolicy = fakeTrustIpc({ ...TRUSTED, state: 'suspended', suspended_by: 'policy' });
    const { unmount } = renderWithTrust(<TrustBadge workspaceId="w1" />, byPolicy);
    const badge = await screen.findByTestId('trust-badge');
    expect(badge).toHaveTextContent('Trust paused');
    expect(badge).toHaveAttribute('title', 'Trust paused by your organisation');
    unmount();
    renderWithTrust(<TrustBadge workspaceId="w1" variant="glyph" />, fakeTrustIpc({ ...TRUSTED, state: 'suspended', suspended_by: 'tamper', tampered_file: 'rules.json' }));
    expect(await screen.findByTestId('trust-badge')).toHaveAttribute('title', expect.stringContaining('rules.json changed outside the app'));
  });

  it('shows "Environment fallback" when the login-shell environment could not be read', async () => {
    renderWithTrust(<TrustBadge workspaceId="w1" />, fakeTrustIpc({ ...TRUSTED, env_fallback: true }));
    expect(await screen.findByTestId('trust-badge')).toHaveTextContent('No sandbox · Environment fallback');
  });

  it('revokes a worktree badge on its parent folder', async () => {
    const ipc = fakeTrustIpc({ ...TRUSTED, workspace_id: 'task', inherited_from: 'w1', grantable: false });
    const user = userEvent.setup({ delay: null });
    renderWithTrust(<TrustBadge workspaceId="task" />, ipc);
    await user.click(await screen.findByTestId('trust-badge'));
    await user.click(await screen.findByTestId('trust-revoke'));
    await vi.waitFor(() => expect(ipc.revoke).toHaveBeenCalledWith('w1'));
  });
});

describe('SandboxUnavailableOffer', { timeout: testTimeout(20_000) }, () => {
  const windows = { ...UNTRUSTED, platform: 'windows' as const, sandbox_available: false };

  it('offers "Trust this folder…" when the policy allows trust, and opens the dialog', async () => {
    const user = userEvent.setup({ delay: null });
    renderWithTrust(<SandboxUnavailableOffer workspaceId="w1" workspaceName="repo" />, fakeTrustIpc(windows));
    const offer = await screen.findByTestId('sandbox-unavailable-offer');
    await user.click(screen.getByRole('button', { name: 'Trust this folder…' }));
    expect(offer).toBeInTheDocument();
    expect(await screen.findByTestId('trust-dialog-no-sandbox')).toHaveTextContent('Windows has no sandbox');
  });

  it('offers nothing when the policy does not allow trust', async () => {
    const ipc = fakeTrustIpc({ ...windows, allowed_by_policy: false, policy_blocks: ['ceiling'] });
    renderWithTrust(<SandboxUnavailableOffer workspaceId="w1" workspaceName="repo" />, ipc);
    await vi.waitFor(() => expect(ipc.status).toHaveBeenCalled());
    expect(screen.queryByTestId('sandbox-unavailable-offer')).toBeNull();
  });
});
