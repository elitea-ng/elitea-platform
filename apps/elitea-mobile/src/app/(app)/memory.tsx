import { useState } from 'react';
import { Alert, RefreshControl } from 'react-native';

import { speaksContract } from '@/api/discovery';
import { clearMemories, deleteMemory, MemoryError, memoryErrorText, useForgetMemories, useMemories } from '@/api/queries/memories';
import { useDesign } from '@/design/DesignProvider';
import { MemoryView } from '@/features/settings/MemoryView';
import { useShareOut } from '@/security/policy';
import { getSession, useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/**
 * Settings > Memory: what the workspace remembers about the caller in the
 * selected project (client contract 1.3). Swipe a row to delete it; "Clear
 * all memories" deletes every one after a confirmation. Online only: nothing here is
 * kept on the device.
 */
export default function MemoryScreen() {
  const { colors } = useDesign();
  const projectId = useSession((s) => s.projectId);
  const workspace = useWorkspaceInfo();
  const supported = speaksContract(workspace?.discovery, 1, 3);
  const memories = useMemories(projectId, supported);
  const forget = useForgetMemories(projectId);
  const selectable = useShareOut();
  const [note, setNote] = useState<string | null>(null);
  const rows = memories.data ?? [];
  // A failed refresh keeps the old rows on screen (F3): say so above them. An
  // empty list shows the error in its place instead. A delete or clear note wins.
  const banner = note ?? (memories.error && rows.length > 0 ? memoryErrorText(memories.error) : null);

  const onDelete = async (id: string): Promise<boolean> => {
    if (projectId === null) return false;
    try {
      await deleteMemory(projectId, id);
      await forget([id]);
      setNote(null);
      return true;
    } catch (error) {
      setNote(memoryErrorText(error, 'delete'));
      return false;
    }
  };

  const onClearAll = () => {
    // The native Alert outlives the screen: a workspace or project switch can
    // complete while it is open (a connect link confirmed on top of it). The
    // request goes to whichever workspace is active at tap time, so a stale
    // confirmation must clear nothing rather than this project id over there.
    const offeredFor = { workspaceId: getSession().workspaceId, projectId };
    Alert.alert('Clear all memories?', 'The workspace forgets everything it remembers about you in this project. This cannot be undone.', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Clear all',
        style: 'destructive',
        onPress: () => {
          const now = getSession();
          if (projectId === null || now.workspaceId !== offeredFor.workspaceId || now.projectId !== offeredFor.projectId) return;
          clearMemories(projectId)
            .then(() => forget('all'))
            .then(() => setNote(null))
            .catch((error: unknown) => setNote(memoryErrorText(error, 'clear')));
        },
      },
    ]);
  };

  if (!supported) {
    // No discovery document yet (an offline launch; it is re-read on
    // reconnect) means "unknown", not "not offered" (F1).
    const why = workspace === null ? memoryErrorText(new MemoryError('offline')) : 'This workspace does not offer memory to mobile yet.';
    return (
      <MemoryView
        status={{ kind: 'unavailable', message: why }}
        rows={[]}
        banner={null}
        selectable={false}
        onDelete={async () => false}
        onClearAll={() => {}}
        onRetry={() => {}}
      />
    );
  }

  return (
    <MemoryView
      status={memories.isLoading ? { kind: 'loading' } : memories.error ? { kind: 'error', message: memoryErrorText(memories.error) } : { kind: 'ready' }}
      rows={rows}
      banner={banner}
      selectable={selectable}
      onDelete={(memory) => onDelete(memory.id)}
      onClearAll={onClearAll}
      onRetry={() => void memories.refetch()}
      refreshControl={<RefreshControl refreshing={memories.isRefetching} onRefresh={() => void memories.refetch()} tintColor={colors.accent} />}
    />
  );
}
