/**
 * The cloud badge on a thread's row in the sidebar: shown while the thread
 * runs in the cloud or is on its way back. A thread that runs here shows
 * nothing (the thread's own header says Local). The status comes from the
 * sidebar's one batched `handoff_status_many` read, not from a call per row.
 */
import { ExecutorBadge } from '@/features/workspace-handoff';
import type { HandoffStatus } from '@/shared/desktop/handoffIpc';

export function ThreadCloudBadge({ status }: { status: HandoffStatus | undefined }): React.JSX.Element | null {
  if (status === undefined || status.executor === 'local') return null;
  return <ExecutorBadge executor={status.executor} />;
}
