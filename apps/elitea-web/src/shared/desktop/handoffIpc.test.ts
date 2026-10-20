import { describe, expect, it, vi } from 'vitest';

import { createHandoffIpc, handoffAllowedBy, HANDOFF_EVENT_CHANNEL, HandoffIpcError, type HandoffEvent } from './handoffIpc';
import { createFakeHandoffIpc } from './handoffIpc.fake';
import { WorkspaceIpcError, type ListenFn } from './workspaceIpc';

const noListen: ListenFn = () => Promise.resolve(() => undefined);

describe('createHandoffIpc', () => {
  it('maps every command to its host name and snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createHandoffIpc(invoke, noListen);

    await ipc.preview('w1', 'u1');
    await ipc.preview('w1', 'u1', ['.env.local']);
    await ipc.send('w1', 'u1', ['a.txt']);
    await ipc.send('w1', 'u1', [], { confirmIncludes: true });
    await ipc.send('w1', 'u1', [], { confirmIncludes: false });
    await ipc.cancel('u1');
    await ipc.settingsGet('w1');
    await ipc.settingsSet('w1', { exclude_patterns: ['dist/'], include: ['.env.example'], env_names: ['API_URL'] });
    await ipc.status('u1');
    await ipc.status('u1', 42);
    await ipc.reclaim('u1');
    await ipc.preflight('u1');
    await ipc.apply('pf-1', 'worktree');
    await ipc.undo('w1:7');
    await ipc.erase('u1');
    await ipc.erase('u1', 42);

    expect(invoke.mock.calls).toEqual([
      ['handoff_preview', { workspace_id: 'w1', conversation_uuid: 'u1' }],
      ['handoff_preview', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: ['.env.local'] }],
      ['handoff_send', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: ['a.txt'] }],
      ['handoff_send', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: [], confirm_includes: true }],
      ['handoff_send', { workspace_id: 'w1', conversation_uuid: 'u1', leave_on_laptop: [] }],
      ['handoff_cancel', { conversation_uuid: 'u1' }],
      ['handoff_settings_get', { workspace_id: 'w1' }],
      ['handoff_settings_set', { workspace_id: 'w1', settings: { exclude_patterns: ['dist/'], include: ['.env.example'], env_names: ['API_URL'] } }],
      ['handoff_status', { conversation_uuid: 'u1' }],
      ['handoff_status', { conversation_uuid: 'u1', project_id: 42 }],
      ['handoff_reclaim', { conversation_uuid: 'u1' }],
      ['handoff_preflight', { conversation_uuid: 'u1' }],
      ['handoff_apply', { preflight_id: 'pf-1', mode: 'worktree' }],
      ['handoff_undo', { checkpoint_id: 'w1:7' }],
      ['handoff_erase', { conversation_uuid: 'u1' }],
      ['handoff_erase', { conversation_uuid: 'u1', project_id: 42 }],
    ]);
  });

  it('reads many statuses in one call, and asks nothing for none', async () => {
    const local = { state: 'LOCAL', epoch: 0, executor: 'local', read_only: false, sent_from_here: false };
    const invoke = vi.fn().mockResolvedValue({ statuses: { u1: local, u2: { ...local, state: 'CLOUD_IDLE', executor: 'cloud' } } });
    const ipc = createHandoffIpc(invoke, noListen);
    await expect(ipc.statusMany([])).resolves.toEqual({});
    expect(invoke).not.toHaveBeenCalled();
    const statuses = await ipc.statusMany(['u1', 'u2']);
    expect(invoke.mock.calls).toEqual([['handoff_status_many', { conversation_uuids: ['u1', 'u2'] }]]);
    expect(statuses['u2']).toMatchObject({ executor: 'cloud' });
  });

  it('rejects with the host code, message and details', async () => {
    const findings = [{ path: '.env', line: 3, rule: 'aws-key' }];
    const ipc = createHandoffIpc(vi.fn().mockRejectedValue({ code: 'secret_found', message: 'a secret', details: { findings } }), noListen);
    const error = await ipc.send('w1', 'u1', []).catch((cause: unknown) => cause);
    expect(error).toBeInstanceOf(HandoffIpcError);
    // A WorkspaceIpcError too: the screens' shared error handling still reads `code`.
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect(error).toMatchObject({ code: 'secret_found', message: 'a secret', details: { findings } });
  });

  it('reads whether a send is allowed from the stored device policy (local work and the handoff switch)', async () => {
    const policy = (local_work: unknown) => createHandoffIpc(vi.fn().mockResolvedValue({ policy: { local_work } }), noListen).allowed();
    await expect(policy({ allowed: true, cloud_handoff: true })).resolves.toBe(true);
    await expect(policy({ allowed: true, cloud_handoff: false })).resolves.toBe(false);
    await expect(policy({ allowed: false, cloud_handoff: true })).resolves.toBe(false);
    await expect(policy({ allowed: true })).resolves.toBe(false);
    expect(handoffAllowedBy(null)).toBe(false);
    expect(handoffAllowedBy({})).toBe(false);
  });

  it('listens on handoff://event and hands over the payload', async () => {
    const event: HandoffEvent = { conversation_uuid: 'u1', phase: 'upload', done: 3, total: 9, message: null };
    const listen = vi.fn<ListenFn>((_channel, handler) => {
      handler(event);
      return Promise.resolve(() => undefined);
    });
    const handler = vi.fn();
    await createHandoffIpc(vi.fn(), listen).onEvent(handler);
    expect(listen.mock.calls[0]?.[0]).toBe(HANDOFF_EVENT_CHANNEL);
    expect(handler).toHaveBeenCalledWith(event);
  });
});

describe('createFakeHandoffIpc', () => {
  it('moves a thread to the cloud on send and back to local on erase', async () => {
    const ipc = createFakeHandoffIpc();
    await ipc.send('w1', 'u1', []);
    expect(await ipc.status('u1')).toMatchObject({ state: 'CLOUD_IDLE', executor: 'cloud', read_only: true });
    await ipc.erase('u1');
    expect(await ipc.status('u1')).toMatchObject({ state: 'LOCAL', executor: 'local' });
  });

  it('previews with files left on the laptop out, their findings with them', async () => {
    const ipc = createFakeHandoffIpc({
      preview: { findings: [{ path: 'a.yml', line: 1, rule: 'aws' }, { path: 'b.sh', line: 2, rule: 'gh' }] },
    });
    const preview = await ipc.preview('w1', 'u1', ['a.yml']);
    expect(preview.findings).toEqual([{ path: 'b.sh', line: 2, rule: 'gh' }]);
    expect(preview.excluded).toEqual([{ path: 'a.yml', reason: 'leave_on_laptop', tracked: true, includable: false }]);
  });

  it('sends an included file once its settings include it, with its findings as warnings that need a confirmation', async () => {
    const ipc = createFakeHandoffIpc({
      preview: { excluded: [{ path: '.env.local', reason: 'platform', tracked: false, includable: true }], findings: [{ path: '.env.local', line: 3, rule: 'token' }] },
    });
    expect((await ipc.preview('w1', 'u1')).findings).toEqual([]);
    await ipc.settingsSet('w1', { exclude_patterns: [], include: ['.env.local'], env_names: [] });
    const preview = await ipc.preview('w1', 'u1');
    expect(preview.included).toEqual([{ path: '.env.local', reason: 'platform' }]);
    expect(preview.include_findings).toEqual([{ path: '.env.local', line: 3, rule: 'token' }]);
    await expect(ipc.send('w1', 'u1', [])).rejects.toMatchObject({ code: 'include_confirm_required' });
    await expect(ipc.send('w1', 'u1', [], { confirmIncludes: true })).resolves.toMatchObject({ included: 1 });
  });

  it('offers env names and lengths only when the policy harvests them, nothing selected', async () => {
    const ipc = createFakeHandoffIpc({ envOffered: [{ name: 'API_URL', length: 23 }] });
    expect((await ipc.preview('w1', 'u1')).env).toBeNull();
    ipc.setPolicy({ env_harvest: true });
    expect((await ipc.preview('w1', 'u1')).env).toEqual({ offered: [{ name: 'API_URL', length: 23 }], selected: [], missing: [], vars: 0, bytes: 0, over_limit: false, notice: null });
    await ipc.settingsSet('w1', { exclude_patterns: [], include: [], env_names: ['API_URL', 'GONE'] });
    expect((await ipc.preview('w1', 'u1')).env).toMatchObject({ selected: ['API_URL', 'GONE'], missing: ['GONE'], vars: 1, bytes: 30 });
  });

  it('refuses bad settings with invalid_request and keeps the old ones', async () => {
    const ipc = createFakeHandoffIpc();
    await expect(ipc.settingsSet('w1', { exclude_patterns: [], include: ['../up'], env_names: [] })).rejects.toMatchObject({ code: 'invalid_request' });
    await expect(ipc.settingsSet('w1', { exclude_patterns: [], include: [], env_names: ['1BAD'] })).rejects.toMatchObject({ code: 'invalid_request' });
    expect((await ipc.settingsGet('w1')).settings).toEqual({ exclude_patterns: [], include: [], env_names: [] });
  });

  it('cancels a held send: it rejects handoff_cancelled and the thread stays local', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.holdSend();
    const sending = ipc.send('w1', 'u1', []);
    await Promise.resolve();
    await expect(ipc.cancel('u1')).resolves.toEqual({ cancelled: true });
    await expect(sending).rejects.toMatchObject({ code: 'handoff_cancelled' });
    expect(await ipc.status('u1')).toMatchObject({ state: 'LOCAL' });
    await expect(ipc.cancel('u1')).resolves.toEqual({ cancelled: false });
  });

  it('answers many statuses, each thread its own', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.setStatusOf('u2', { state: 'CLOUD_IDLE', executor: 'cloud' });
    const statuses = await ipc.statusMany(['u1', 'u2']);
    expect(statuses['u1']).toMatchObject({ state: 'LOCAL' });
    expect(statuses['u2']).toMatchObject({ state: 'CLOUD_IDLE' });
    expect(ipc.calls.statusManys).toEqual([['u1', 'u2']]);
  });

  it('rejects the next call the way the host does', async () => {
    const ipc = createFakeHandoffIpc();
    ipc.failNext('apply', 'integrity_failed', 'bad', { x: 1 });
    await expect(ipc.apply('pf', 'apply')).rejects.toMatchObject({ code: 'integrity_failed', details: { x: 1 } });
    await expect(ipc.apply('pf', 'apply')).resolves.toMatchObject({ outcome: 'applied' });
  });
});
