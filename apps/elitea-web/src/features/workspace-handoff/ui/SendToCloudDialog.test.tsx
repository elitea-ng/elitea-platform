/**
 * The send dialog, driven against the in-memory host: every state of the
 * preview (ok, findings, caps), the send with live progress, and each kind of
 * refusal.
 */
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { createFakeHandoffIpc, type FakeHandoffIpc } from '@/shared/desktop/handoffIpc.fake';

import { SendToCloudDialog } from './SendToCloudDialog';
import { renderWithClient } from './testing';

function mount(ipc: FakeHandoffIpc, onSent = vi.fn(), onClose = vi.fn()): { onSent: typeof onSent; onClose: typeof onClose } {
  renderWithClient(<SendToCloudDialog open ipc={ipc} workspaceId="w1" conversationUuid="u1" onSent={onSent} onClose={onClose} />);
  return { onSent, onClose };
}

const sendButton = (): HTMLElement => screen.getByRole('button', { name: 'Send to cloud' });

describe('SendToCloudDialog preview', () => {
  it('shows what would be sent, what is left out and that the platform can read it', async () => {
    const ipc = createFakeHandoffIpc({
      preview: {
        files: 1234,
        bytes: 5 * 1024 * 1024,
        unpacked_bytes: 20 * 1024 * 1024,
        excluded: [
          { path: '.env', reason: 'mandatory', tracked: true },
          { path: 'secrets/', reason: 'path_deny', tracked: false },
        ],
      },
    });
    mount(ipc);

    const summary = await screen.findByTestId('handoff-summary');
    expect(summary).toHaveTextContent('1,234 files · 5.0 MB to upload · 20 MB unpacked');
    expect(summary).toHaveTextContent('History: none');
    expect(screen.getByTestId('handoff-platform-note')).toHaveTextContent('The platform can read the files you upload');
    const excluded = screen.getByTestId('handoff-excluded');
    expect(excluded).toHaveTextContent('Left out (2)');
    expect(excluded).toHaveTextContent('.env · never sent (secrets and keys)');
    expect(excluded).toHaveTextContent('secrets/ · blocked by your organisation');
    expect(sendButton()).toBeEnabled();
    expect(ipc.calls.previews).toEqual([{ workspaceId: 'w1', conversationUuid: 'u1', leave: [] }]);
  });

  it('says so for a folder that is not a repository', async () => {
    mount(createFakeHandoffIpc({ preview: { shadow: true } }));
    expect(await screen.findByTestId('handoff-summary')).toHaveTextContent('not a git repository');
  });

  it('blocks the send on secret findings (file:line:rule) and unblocks it when each file is left on the laptop', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({
      preview: {
        findings: [
          { path: 'config/prod.yml', line: 12, rule: 'aws-access-key' },
          { path: 'config/prod.yml', line: 40, rule: 'password-in-url' },
          { path: 'deploy.sh', line: 3, rule: 'github-token' },
        ],
      },
    });
    mount(ipc);

    const findings = await screen.findByTestId('handoff-findings');
    expect(findings).toHaveTextContent('config/prod.yml:12 · aws-access-key');
    expect(findings).toHaveTextContent('config/prod.yml:40 · password-in-url');
    expect(findings).toHaveTextContent('deploy.sh:3 · github-token');
    expect(sendButton()).toBeDisabled();

    await user.click(within(findings).getByRole('button', { name: 'Leave config/prod.yml on my laptop' }));
    // The preview is read again with the file left out.
    await waitFor(() => expect(ipc.calls.previews.at(-1)).toEqual({ workspaceId: 'w1', conversationUuid: 'u1', leave: ['config/prod.yml'] }));
    await waitFor(() => expect(screen.getByTestId('handoff-findings')).not.toHaveTextContent('config/prod.yml'));
    expect(sendButton()).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Leave deploy.sh on my laptop' }));
    await waitFor(() => expect(screen.queryByTestId('handoff-findings')).toBeNull());
    await waitFor(() => expect(sendButton()).toBeEnabled());

    await user.click(sendButton());
    await waitFor(() => expect(ipc.calls.sends).toEqual([{ workspaceId: 'w1', conversationUuid: 'u1', leave: ['config/prod.yml', 'deploy.sh'] }]));
  });

  it('refuses an oversized folder with the breakdown of the caps and the largest files', async () => {
    mount(
      createFakeHandoffIpc({
        preview: {
          caps: {
            violations: [
              { cap: 'max_file_bytes', limit: 25 * 1024 * 1024, actual: 90 * 1024 * 1024, path: 'data/dump.bin' },
              { cap: 'max_files', limit: 20_000, actual: 31_000, path: null },
            ],
            largest: [{ path: 'data/dump.bin', bytes: 90 * 1024 * 1024 }],
          },
        },
      }),
    );
    const alert = await screen.findByTestId('handoff-caps-error');
    expect(alert).toHaveTextContent('This folder is too large to send');
    const caps = within(alert).getByTestId('handoff-caps');
    expect(caps).toHaveTextContent('Size of one file: 90 MB (the limit is 25 MB) · data/dump.bin');
    expect(caps).toHaveTextContent('Number of files: 31,000 (the limit is 20,000)');
    expect(caps).toHaveTextContent('Largest files');
    expect(sendButton()).toBeDisabled();
  });

  it('shows the refusal when the folder cannot be read at all', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.failNext('preview', 'handoff_unsupported', 'subfolder');
    mount(ipc);
    expect(await screen.findByTestId('handoff-send-error')).toHaveTextContent('Only the top folder of a git repository');
    expect(sendButton()).toBeDisabled();
  });
});

describe('SendToCloudDialog send', () => {
  it('shows each phase of the send as the host reports it, then reports the thread sent', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.holdSend();
    const { onSent } = mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await waitFor(() => expect(ipc.subscriberCount()).toBe(1));

    await user.click(sendButton());
    const progress = await screen.findByTestId('handoff-progress');
    expect(progress).toHaveTextContent('Starting');

    act(() => ipc.emit({ conversation_uuid: 'u1', phase: 'scan', done: 3, total: 10, message: null }));
    expect(progress).toHaveTextContent('Checking for secrets');
    act(() => ipc.emit({ conversation_uuid: 'u1', phase: 'upload', done: 5, total: 10, message: 'segment 1 of 2' }));
    expect(progress).toHaveTextContent('Uploading: segment 1 of 2');
    expect(within(progress).getByRole('progressbar')).toHaveAttribute('aria-valuenow', '50');
    // Another thread's progress is not this one's.
    act(() => ipc.emit({ conversation_uuid: 'other', phase: 'publish', done: 1, total: 1, message: null }));
    expect(progress).toHaveTextContent('Uploading');
    // While sending, nothing can be cancelled or sent twice.
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled();
    expect(sendButton()).toBeDisabled();

    act(() => ipc.releaseSend());
    await waitFor(() => expect(onSent).toHaveBeenCalledTimes(1));
    expect(onSent.mock.calls[0]?.[0]).toMatchObject({ handoff_id: 'h-1', state: 'CLOUD_IDLE' });
  });

  it('shows the findings a send was refused for and lets the file be left on the laptop', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', 'secret_found', 'secret', { findings: [{ path: 'late.env', line: 1, rule: 'private-key' }] });
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());

    expect(await screen.findByTestId('handoff-send-error')).toHaveTextContent('Some files look like they hold secrets');
    expect(screen.getByTestId('handoff-findings')).toHaveTextContent('late.env:1 · private-key');
    expect(sendButton()).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Leave late.env on my laptop' }));
    await waitFor(() => expect(screen.queryByTestId('handoff-findings')).toBeNull());
    await waitFor(() => expect(screen.queryByTestId('handoff-send-error')).toBeNull());
    await waitFor(() => expect(sendButton()).toBeEnabled());
  });

  it('shows the caps a send was refused for after the begin call', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', 'handoff_too_large', 'big', {
      violations: [{ cap: 'max_segment_bytes', limit: 200 * 1024 * 1024, actual: 300 * 1024 * 1024, path: null }],
      largest: [],
    });
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    const refusal = await screen.findByTestId('handoff-send-error');
    expect(refusal).toHaveTextContent('Size of what is sent: 300 MB (the limit is 200 MB)');
  });

  it('shows the quota a send was refused for', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', 'workspace_quota_exceeded', 'full', { quota_bytes: 1024 * 1024 * 1024, used_bytes: 1000 * 1024 * 1024 });
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    const refusal = await screen.findByTestId('handoff-send-error');
    expect(refusal).toHaveTextContent('cloud workspace storage is full');
    expect(refusal).toHaveTextContent('quota bytes: 1.0 GB');
    expect(refusal).toHaveTextContent('used bytes: 1000 MB');
  });

  it.each([
    ['local_turn_in_progress', 'has not been committed yet'],
    ['cloud_handoff_disabled', 'turned off by your organisation'],
    ['history_mode_not_allowed', 'does not allow sending the git history'],
    ['workspace_busy', 'An agent is still working in this folder'],
  ])('says what %s means', async (code, text) => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', code, 'raw');
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    expect(await screen.findByTestId('handoff-send-error')).toHaveTextContent(text);
    // Back to a state where it can be tried again.
    expect(sendButton()).toBeEnabled();
  });

  it('falls back to the host message for a code it does not know', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', 'something_new', 'The host says this.');
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    expect(await screen.findByTestId('handoff-send-error')).toHaveTextContent('The host says this.');
  });
});
