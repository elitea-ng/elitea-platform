/**
 * The workspace's and the person's custom "/" commands (`commands_list`),
 * for the composer's menu and `/help`. Only for a thread that runs here:
 * a command is read and expanded by the local host, so a cloud thread
 * offers none. `[]` while it loads and when the read fails; the host reads
 * a command again at the send, so one listed here and gone by then is
 * refused there (`command_unknown`).
 */
import { useQuery } from '@tanstack/react-query';

import type { CustomCommand, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';

const NONE: readonly CustomCommand[] = [];

export function useCustomCommands(ipc: WorkspaceIpc, workspaceId: string, enabled: boolean): readonly CustomCommand[] {
  const query = useQuery({
    queryKey: ['workspace', 'commands', workspaceId],
    queryFn: () => ipc.commands(workspaceId),
    enabled: enabled && workspaceId !== '',
    staleTime: 5_000,
  });
  return enabled ? (query.data ?? NONE) : NONE;
}
