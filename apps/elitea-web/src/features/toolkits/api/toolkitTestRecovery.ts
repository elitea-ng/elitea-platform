import { createStorage } from '@/shared/lib/storage';

const KEY = 'toolkits.pendingTest';
interface PendingTest { readonly projectId: string; readonly toolkitId: string; readonly taskId: string }
function validIdentity(value: unknown): value is string {
  return typeof value === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(value);
}

/** Keep only the latest tab-local execution identity. Logout clears this namespace. */
export function rememberToolkitTest(projectId: unknown, toolkitId: unknown, taskId: string): void {
  if (projectId === undefined || projectId === null || toolkitId === undefined || toolkitId === null) return;
  const value = { projectId: String(projectId), toolkitId: String(toolkitId), taskId };
  if (!validIdentity(value.projectId) || !validIdentity(value.toolkitId) || !validIdentity(taskId)) return;
  try { createStorage('session').setJSON(KEY, value); } catch { /* Storage can be disabled; the live observer still works. */ }
}

export function recalledToolkitTest(projectId: unknown, toolkitId: unknown): string | undefined {
  try {
    const value = createStorage('session').getJSON<PendingTest>(KEY, (raw) => {
      if (typeof raw !== 'object' || raw === null) throw new Error('Invalid saved test identity');
      const candidate = raw as Partial<PendingTest>;
      if (!validIdentity(candidate.projectId) || !validIdentity(candidate.toolkitId) || !validIdentity(candidate.taskId)) throw new Error('Invalid saved test identity');
      return { projectId: candidate.projectId, toolkitId: candidate.toolkitId, taskId: candidate.taskId };
    });
    return value?.projectId === String(projectId) && value.toolkitId === String(toolkitId) ? value.taskId : undefined;
  } catch { return undefined; }
}

export function forgetToolkitTest(projectId: unknown, toolkitId: unknown): void {
  if (recalledToolkitTest(projectId, toolkitId) === undefined) return;
  try { createStorage('session').remove(KEY); } catch { /* Disabled storage must not prevent a new test. */ }
}
