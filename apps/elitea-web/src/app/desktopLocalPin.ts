/**
 * Desktop only (ADR-0029): tells the skill dialogs which conversations are local threads on this computer, so a pin
 * goes to the host that runs them (#2183). The lookup and the host client load on first use, so the web build, where
 * the branch below is dead code, carries none of it.
 */
import { registerLocalThreadPinFinder } from '@/shared/desktop/localThreadPin';

if (import.meta.env.MODE === 'desktop') {
  registerLocalThreadPinFinder(async (conversationId, projectId) => {
    const [{ findLocalWorkThread }, { tauriWorkspaceIpc }] = await Promise.all([import('@/pages/workspace/localThreadLookup'), import('@/shared/desktop/workspaceIpc')]);
    const ipc = tauriWorkspaceIpc();
    const workspaceId = await findLocalWorkThread(conversationId, ipc);
    if (ipc === undefined || workspaceId === null) return null;
    return (skillId) => ipc.pinThreadSkill(workspaceId, conversationId, { projectId, skillId });
  });
}
