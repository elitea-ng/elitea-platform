import { describe, expect, it, vi } from 'vitest';

import { createDoctorIpc, needsAttention, type DoctorCheck } from './doctorIpc';
import { WorkspaceIpcError } from './workspaceIpc';

describe('createDoctorIpc', () => {
  it('maps to doctor_run / doctor_fix with snake_case arguments', async () => {
    const invoke = vi
      .fn()
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce({ message: 'Done.' })
      .mockResolvedValueOnce({ message: 'Removed.' });
    const ipc = createDoctorIpc(invoke);
    await ipc.run();
    await ipc.run('local');
    expect(await ipc.fix('credentials.tighten')).toBe('Done.');
    expect(await ipc.fix('workspaces.drop_missing', true)).toBe('Removed.');
    expect(invoke.mock.calls).toEqual([
      ['doctor_run', {}],
      ['doctor_run', { scope: 'local' }],
      ['doctor_fix', { fix_id: 'credentials.tighten' }],
      ['doctor_fix', { fix_id: 'workspaces.drop_missing', confirm: true }],
    ]);
  });

  it('rejects with the host message', async () => {
    const ipc = createDoctorIpc(vi.fn().mockRejectedValue('belongs to another user; it is not changed'));
    await expect(ipc.fix('dir.tighten.data')).rejects.toBeInstanceOf(WorkspaceIpcError);
  });

  it('needs attention only for a failure', () => {
    const check = (status: DoctorCheck['status']): DoctorCheck => ({ id: 'x', title: 'X', status, message: '' });
    expect(needsAttention([check('ok'), check('warn')])).toBe(false);
    expect(needsAttention([check('ok'), check('fail')])).toBe(true);
  });
});
