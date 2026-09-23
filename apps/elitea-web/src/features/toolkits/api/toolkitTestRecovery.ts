import { createStorage } from '@/shared/lib/storage';
import type { TestToolkitToolOutcome } from './toolkitTestRun';

type ToolkitIdentity = string | number | undefined | null;

const KEY = 'toolkits.pendingTest';
interface PendingTest { readonly projectId: string; readonly toolkitId: string; readonly taskId: string; readonly lookup?: 'request' }
function validIdentity(value: unknown): value is string {
  return typeof value === 'string' && /^[A-Za-z0-9_-]{1,128}$/.test(value);
}

/** Keep only the latest tab-local execution identity. Logout clears this namespace. */
export function rememberToolkitTest(projectId: ToolkitIdentity, toolkitId: ToolkitIdentity, taskId: string, lookup?: 'request'): void {
  if (projectId === undefined || projectId === null || toolkitId === undefined || toolkitId === null) return;
  const value = { projectId: String(projectId), toolkitId: String(toolkitId), taskId, ...(lookup ? { lookup } : {}) };
  if (!validIdentity(value.projectId) || !validIdentity(value.toolkitId) || !validIdentity(taskId)) return;
  try { createStorage('session').setJSON(KEY, value); } catch { /* Storage can be disabled; the live observer still works. */ }
}

export function recalledToolkitTest(projectId: ToolkitIdentity, toolkitId: ToolkitIdentity): Pick<PendingTest, 'taskId' | 'lookup'> | undefined {
  try {
    const value = createStorage('session').getJSON<PendingTest>(KEY, (raw) => {
      if (typeof raw !== 'object' || raw === null) throw new Error('Invalid saved test identity');
      const candidate = raw as Partial<PendingTest>;
      if (!validIdentity(candidate.projectId) || !validIdentity(candidate.toolkitId) || !validIdentity(candidate.taskId)) throw new Error('Invalid saved test identity');
      if (candidate.lookup !== undefined && candidate.lookup !== 'request') throw new Error('Invalid saved lookup');
      return { projectId: candidate.projectId, toolkitId: candidate.toolkitId, taskId: candidate.taskId, ...(candidate.lookup ? { lookup: candidate.lookup } : {}) };
    });
    return value?.projectId === String(projectId) && value.toolkitId === String(toolkitId) ? { taskId: value.taskId, ...(value.lookup ? { lookup: value.lookup } : {}) } : undefined;
  } catch { return undefined; }
}

/** A missing admission is inconclusive. Keep its reference for a later read. */
export function rememberToolkitTestOutcome(projectId: ToolkitIdentity, toolkitId: ToolkitIdentity, outcome: TestToolkitToolOutcome): void {
  if (outcome.kind === 'unconfirmed') return;
  if (outcome.kind === 'authorizationRequired') rememberToolkitTest(projectId, toolkitId, outcome.taskId);
  else if (outcome.kind === 'timeout' && outcome.taskId) rememberToolkitTest(projectId, toolkitId, outcome.taskId, outcome.lookup);
  else forgetToolkitTest(projectId, toolkitId);
}

export function forgetToolkitTest(projectId: ToolkitIdentity, toolkitId: ToolkitIdentity): void {
  if (recalledToolkitTest(projectId, toolkitId) === undefined) return;
  try { createStorage('session').remove(KEY); } catch { /* Disabled storage must not prevent a new test. */ }
}
