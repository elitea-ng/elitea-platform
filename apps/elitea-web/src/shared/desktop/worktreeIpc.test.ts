import { describe, expect, it, vi } from 'vitest';

import { createWorktreeIpc } from './worktreeIpc';
import { WorkspaceIpcError } from './workspaceIpc';

describe('createWorktreeIpc', () => {
  it('maps to the worktree_* commands with snake_case arguments', async () => {
    const invoke = vi.fn().mockResolvedValue(null);
    const ipc = createWorktreeIpc(invoke);
    await ipc.create('w1', 'u1');
    await ipc.create('w1', 'u2', 'checkpoint:3');
    await ipc.list('w1');
    await ipc.diff('w1', 'u1');
    await expect(ipc.remove('w1', 'u1', true)).resolves.toBeUndefined();
    await ipc.preflight('w1', 'u1');
    await ipc.apply('pf-1');
    await ipc.apply('pf-2', 'apply_with_markers');
    await ipc.undo('cp-1');
    await ipc.undo('cp-2', true);
    expect(invoke.mock.calls).toEqual([
      ['worktree_create', { workspace_id: 'w1', thread_uuid: 'u1' }],
      ['worktree_create', { workspace_id: 'w1', thread_uuid: 'u2', base: 'checkpoint:3' }],
      ['worktree_list', { workspace_id: 'w1' }],
      ['worktree_diff', { workspace_id: 'w1', thread_uuid: 'u1' }],
      ['worktree_remove', { workspace_id: 'w1', thread_uuid: 'u1', force: true }],
      ['worktree_merge_preflight', { workspace_id: 'w1', thread_uuid: 'u1' }],
      ['worktree_merge_apply', { preflight_id: 'pf-1' }],
      ['worktree_merge_apply', { preflight_id: 'pf-2', mode: 'apply_with_markers' }],
      ['worktree_merge_undo', { checkpoint_id: 'cp-1' }],
      ['worktree_merge_undo', { checkpoint_id: 'cp-2', confirm_older: true }],
    ]);
  });

  it('rejects with the host code', async () => {
    const ipc = createWorktreeIpc(vi.fn().mockRejectedValue({ code: 'worktree_dirty', message: 'uncommitted changes' }));
    const error = await ipc.remove('w1', 'u1', false).catch((e: unknown) => e);
    expect(error).toBeInstanceOf(WorkspaceIpcError);
    expect((error as WorkspaceIpcError).code).toBe('worktree_dirty');
  });
});
