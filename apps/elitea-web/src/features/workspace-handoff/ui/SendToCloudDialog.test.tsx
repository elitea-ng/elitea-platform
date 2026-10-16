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
          { path: '.env', reason: 'platform', tracked: true, includable: false },
          { path: 'secrets/', reason: 'path_deny', tracked: false, includable: false },
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
    expect(within(screen.getByTestId('handoff-excluded-platform')).getByText('.env')).toBeInTheDocument();
    expect(within(screen.getByTestId('handoff-excluded-path-deny')).getByText('secrets/')).toBeInTheDocument();
    // Nothing includable, nothing to include.
    expect(screen.queryByRole('button', { name: 'Include anyway' })).toBeNull();
    expect(screen.queryByTestId('handoff-policy-defaults')).toBeNull();
    expect(screen.queryByTestId('handoff-env')).toBeNull();
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
    await waitFor(() => expect(ipc.calls.sends).toEqual([{ workspaceId: 'w1', conversationUuid: 'u1', leave: ['config/prod.yml', 'deploy.sh'], confirmIncludes: false }]));
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
    // While sending, nothing can be sent twice; Cancel stops the send.
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeEnabled();
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

describe('SendToCloudDialog exclusions and includes', () => {
  const excluded = [
    { path: '.env', reason: 'platform', tracked: false, includable: true },
    { path: 'vendor/big.bin', reason: 'project', tracked: true, includable: true },
    { path: 'scratch/', reason: 'user', tracked: false, includable: true },
    { path: 'notes.md', reason: 'eliteaignore', tracked: true, includable: true },
    { path: 'keys/prod.pem', reason: 'path_deny', tracked: false, includable: false },
    { path: 'socket', reason: 'not_a_file', tracked: false, includable: false },
  ] as const;

  it('groups what is left out by who decided it', async () => {
    mount(createFakeHandoffIpc({ preview: { excluded: excluded.map((entry) => ({ ...entry })) } }));
    const platform = await screen.findByTestId('handoff-excluded-platform');
    expect(platform).toHaveTextContent('Platform (1)');
    expect(platform).toHaveTextContent('.env');
    expect(screen.getByTestId('handoff-excluded-project')).toHaveTextContent('Project (1)vendor/big.bin');
    const folder = screen.getByTestId('handoff-excluded-folder');
    expect(folder).toHaveTextContent('Your folder (2)');
    expect(folder).toHaveTextContent('scratch/ · matches your folder’s exclusions');
    expect(folder).toHaveTextContent('notes.md · listed in .eliteaignore');
    expect(screen.getByTestId('handoff-excluded-path-deny')).toHaveTextContent('Hard deny (1)keys/prod.pem');
    expect(screen.getByTestId('handoff-excluded-other')).toHaveTextContent('socket · not a regular file');
    // Include anyway only where it is includable.
    expect(within(platform).getByRole('button', { name: 'Include .env anyway' })).toBeInTheDocument();
    expect(within(screen.getByTestId('handoff-excluded-path-deny')).queryByRole('button')).toBeNull();
  });

  it('puts a file left on the laptop in your folder group', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ preview: { findings: [{ path: 'deploy.sh', line: 3, rule: 'github-token' }] } });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Leave deploy.sh on my laptop' }));
    expect(await screen.findByTestId('handoff-excluded-folder')).toHaveTextContent('deploy.sh · left on your laptop');
  });

  it('keeps a show more bound per group', async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 11 }, (_, index) => ({ path: `gen/${String(index)}.o`, reason: 'project' as const, tracked: false, includable: false }));
    mount(createFakeHandoffIpc({ preview: { excluded: many } }));
    const project = await screen.findByTestId('handoff-excluded-project');
    expect(within(project).getAllByRole('listitem')).toHaveLength(8);
    await user.click(within(project).getByRole('button', { name: 'and 3 more' }));
    expect(within(project).getAllByRole('listitem')).toHaveLength(11);
  });

  it('hides Include anyway entirely when the policy does not allow includes', async () => {
    mount(createFakeHandoffIpc({ policy: { allow_user_include: false }, preview: { excluded: excluded.map((entry) => ({ ...entry })) } }));
    await screen.findByTestId('handoff-excluded-platform');
    expect(screen.queryByRole('button', { name: /anyway/ })).toBeNull();
  });

  it('includes a file anyway: persists the exact path in the folder settings and previews again', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ preview: { excluded: excluded.map((entry) => ({ ...entry })) } });
    mount(ipc);
    await user.click(await screen.findByRole('button', { name: 'Include vendor/big.bin anyway' }));

    await waitFor(() => expect(ipc.calls.settingsSets).toEqual([{ workspaceId: 'w1', settings: { exclude_patterns: [], include: ['vendor/big.bin'], env_names: [] } }]));
    expect(ipc.calls.settingsGets).toEqual(['w1']);
    const included = await screen.findByTestId('handoff-included');
    expect(included).toHaveTextContent('vendor/big.bin');
    expect(ipc.calls.previews.length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByTestId('handoff-excluded-project')).toBeNull();

    await user.click(within(included).getByRole('button', { name: 'Stop including vendor/big.bin' }));
    await waitFor(() => expect(ipc.calls.settingsSets.at(-1)?.settings.include).toEqual([]));
    await waitFor(() => expect(screen.queryByTestId('handoff-included')).toBeNull());
    expect(await screen.findByTestId('handoff-excluded-project')).toHaveTextContent('vendor/big.bin');
  });

  it('asks to confirm the findings of included files before Send, and passes the confirmation', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({
      preview: { excluded: [{ path: '.env.local', reason: 'platform', tracked: false, includable: true }], findings: [{ path: '.env.local', line: 2, rule: 'api-key' }] },
      settings: { w1: { include: ['.env.local'] } },
    });
    mount(ipc);

    const warning = await screen.findByTestId('handoff-include-findings');
    expect(warning).toHaveTextContent('.env.local:2 · api-key');
    // Not a blocking finding: no "Leave on my laptop" list.
    expect(screen.queryByTestId('handoff-findings')).toBeNull();
    const confirm = within(warning).getByRole('checkbox', { name: 'I confirm sending these included files for this send' });
    expect(confirm).not.toBeChecked();
    expect(sendButton()).toBeDisabled();

    await user.click(confirm);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    await waitFor(() => expect(ipc.calls.sends).toEqual([{ workspaceId: 'w1', conversationUuid: 'u1', leave: [], confirmIncludes: true }]));
  });

  it('unchecks the confirmation when the preview changes', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({
      preview: {
        excluded: [{ path: '.env.local', reason: 'platform', tracked: false, includable: true }],
        findings: [{ path: '.env.local', line: 2, rule: 'api-key' }, { path: 'deploy.sh', line: 1, rule: 'gh' }],
      },
      settings: { w1: { include: ['.env.local'] } },
    });
    mount(ipc);
    await user.click(await screen.findByRole('checkbox', { name: 'I confirm sending these included files for this send' }));
    expect(screen.getByRole('checkbox', { name: 'I confirm sending these included files for this send' })).toBeChecked();
    await user.click(screen.getByRole('button', { name: 'Leave deploy.sh on my laptop' }));
    await waitFor(() => expect(screen.queryByTestId('handoff-findings')).toBeNull());
    expect(screen.getByRole('checkbox', { name: 'I confirm sending these included files for this send' })).not.toBeChecked();
    expect(sendButton()).toBeDisabled();
  });

  it('notes when the server gave no handoff policy and the defaults are shown', async () => {
    mount(createFakeHandoffIpc({ policy: { source: 'defaults' } }));
    expect(await screen.findByTestId('handoff-policy-defaults')).toHaveTextContent('The server did not provide its handoff policy');
  });

  it('shows the room left in the project’s cloud storage when known', async () => {
    mount(createFakeHandoffIpc({ preview: { project_workspace_bytes_left: 3 * 1024 * 1024 * 1024 } }));
    expect(await screen.findByTestId('handoff-summary')).toHaveTextContent('3.0 GB left in this project’s cloud storage');
  });
});

describe('SendToCloudDialog environment variables', () => {
  const offered = [
    { name: 'API_URL', length: 23 },
    { name: 'DATABASE_URL', length: 61 },
    { name: 'LOG_LEVEL', length: 4 },
  ];

  it('says so, and offers nothing, when the login shell could not be read', async () => {
    mount(createFakeHandoffIpc({ policy: { env_harvest: true }, envOffered: offered, envNotice: 'the login shell did not answer in 10s' }));
    const env = await screen.findByTestId('handoff-env');
    expect(screen.getByTestId('handoff-env-notice')).toHaveTextContent('Your login shell’s variables could not be read (the login shell did not answer in 10s), so none are offered.');
    expect(within(env).queryAllByRole('checkbox')).toHaveLength(0);
  });

  it('is not shown when the policy does not harvest the environment', async () => {
    mount(createFakeHandoffIpc({ envOffered: offered }));
    await screen.findByTestId('handoff-summary');
    expect(screen.queryByTestId('handoff-env')).toBeNull();
  });

  it('lists names with masked lengths only, nothing preselected, and persists a toggle', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc({ policy: { env_harvest: true }, envOffered: offered });
    mount(ipc);
    const env = await screen.findByTestId('handoff-env');
    expect(env).toHaveTextContent('API_URL •••••• (23 chars)');
    expect(env).toHaveTextContent('DATABASE_URL •••••• (61 chars)');
    for (const box of within(env).getAllByRole('checkbox')) expect(box).not.toBeChecked();
    expect(screen.getByTestId('handoff-env-totals')).toHaveTextContent('0 of at most 50 variables');

    await user.type(within(env).getByRole('textbox', { name: 'Search variables' }), 'data');
    expect(within(env).getAllByRole('checkbox')).toHaveLength(1);
    await user.click(within(env).getByRole('checkbox', { name: /DATABASE_URL/ }));
    await waitFor(() => expect(ipc.calls.settingsSets.at(-1)?.settings.env_names).toEqual(['DATABASE_URL']));
    await waitFor(() => expect(within(screen.getByTestId('handoff-env')).getByRole('checkbox', { name: /DATABASE_URL/ })).toBeChecked());
    expect(screen.getByTestId('handoff-env-totals')).toHaveTextContent('1 of at most 50 variables');
  });

  it('preselects only the names in the folder settings and shows the ones missing now', async () => {
    mount(createFakeHandoffIpc({ policy: { env_harvest: true }, envOffered: offered, settings: { w1: { env_names: ['LOG_LEVEL', 'GONE_TOKEN'] } } }));
    const env = await screen.findByTestId('handoff-env');
    expect(within(env).getByRole('checkbox', { name: /LOG_LEVEL/ })).toBeChecked();
    expect(within(env).getByRole('checkbox', { name: /API_URL/ })).not.toBeChecked();
    expect(screen.getByTestId('handoff-env-missing')).toHaveTextContent('GONE_TOKEN');
  });

  it('blocks the send over the limits', async () => {
    mount(
      createFakeHandoffIpc({
        policy: { env_harvest: true, env_limits: { max_vars: 1, max_bytes: 1024 } },
        envOffered: offered,
        settings: { w1: { env_names: ['API_URL', 'LOG_LEVEL'] } },
      }),
    );
    expect(await screen.findByTestId('handoff-env-over')).toHaveTextContent('over the limit');
    expect(sendButton()).toBeDisabled();
  });
});

describe('SendToCloudDialog cancel', () => {
  it('cancels a running send and says the thread stays here, not as an error', async () => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.holdSend();
    const { onSent } = mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    act(() => ipc.emit({ conversation_uuid: 'u1', phase: 'begin', done: 0, total: 0, message: null }));
    expect(await screen.findByTestId('handoff-progress')).toHaveTextContent('Starting the handoff on the server');

    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(ipc.calls.cancels).toEqual(['u1']);
    const note = await screen.findByTestId('handoff-send-cancelled');
    expect(note).toHaveTextContent('Sending was cancelled; the thread stays on this computer.');
    expect(screen.queryByTestId('handoff-send-error')).toBeNull();
    expect(onSent).not.toHaveBeenCalled();
    expect(sendButton()).toBeEnabled();
  });

  it.each([
    ['include_confirm_required', 'Confirm sending them for this send'],
    ['env_too_large', 'environment variables you chose are over the limit'],
    ['path_denied', 'blocked by your organisation'],
  ])('says what %s means', async (code, text) => {
    const user = userEvent.setup();
    const ipc = createFakeHandoffIpc();
    ipc.failNext('send', code, 'raw');
    mount(ipc);
    await waitFor(() => expect(sendButton()).toBeEnabled());
    await user.click(sendButton());
    expect(await screen.findByTestId('handoff-send-error')).toHaveTextContent(text);
  });
});
