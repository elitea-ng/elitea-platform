/**
 * The cloud badge on a thread's row in the sidebar: shown while the thread
 * runs in the cloud or is on its way back. A thread that runs here shows
 * nothing (the thread's own header says Local). It reads the host's own
 * records, so it costs no request for a thread that was never sent.
 */
import { ExecutorBadge, useHandoffIpc, useHandoffStatus } from '@/features/workspace-handoff';

export function ThreadCloudBadge({ uuid }: { uuid: string }): React.JSX.Element | null {
  const ipc = useHandoffIpc();
  const { status } = useHandoffStatus(ipc, uuid);
  if (status === undefined || status.executor === 'local') return null;
  return <ExecutorBadge executor={status.executor} />;
}
