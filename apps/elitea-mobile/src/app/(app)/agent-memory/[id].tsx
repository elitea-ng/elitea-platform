import { Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';
import { Alert, RefreshControl } from 'react-native';

import {
  AGENT_MEMORY_COPY,
  AgentMemoryError,
  agentMemoryErrorText,
  canDeleteEntry,
  deleteAgentMemoryEntry,
  setAgentMemoryOptOut,
  useAgentMemory,
  useAgentMemoryCache,
  type AgentMemoryEntry,
} from '@/api/queries/agentMemory';
import { useDesign } from '@/design/DesignProvider';
import { AgentMemoryView } from '@/features/agentMemory/AgentMemoryView';
import { useAgentMemoryAvailable } from '@/features/agentMemory/useAgentMemoryAvailable';
import { useShareOut } from '@/security/policy';
import { useSelfUserId } from '@/state/selfIdentity';
import { getSession, useSession } from '@/state/session';
import { useWorkspaceInfo } from '@/state/workspaceInfo';

/** The agent's name as passed in the route, cut to something a title can hold. */
function nameParam(value: unknown): string {
  const name = typeof value === 'string' ? value.trim().slice(0, 120) : '';
  return name === '' ? 'This agent' : name;
}

/**
 * What one agent remembers in the selected project (ADR-0050 decision 1b), opened from a
 * conversation's participant sheet ("What it remembers") or a Discover agent. Lists the notes
 * the caller may see, with pull to refresh and paging; deletes one after a confirmation; and the
 * caller's "Remember things about me" switch. Refusals show the server's own sentence. Online
 * only: nothing here is kept on the device.
 */
export default function AgentMemoryScreen() {
  const { colors } = useDesign();
  const { id, name } = useLocalSearchParams<{ id: string; name?: string }>();
  const applicationId = typeof id === 'string' && /^[1-9]\d{0,14}$/.test(id) ? Number(id) : null;
  const agentName = nameParam(name);
  // The agent id is only meaningful in the project (and workspace) it was opened from: pin both at
  // mount. A switch while the screen is open shows "moved" instead of another project's agent
  // under this one's name.
  const [opened] = useState(() => ({ workspaceId: getSession().workspaceId, projectId: getSession().projectId }));
  const projectId = opened.projectId;
  const moved = useSession((s) => s.workspaceId !== opened.workspaceId || s.projectId !== opened.projectId);
  const workspace = useWorkspaceInfo();
  const supported = useAgentMemoryAvailable();
  const memory = useAgentMemory(projectId, applicationId, supported && !moved);
  const cache = useAgentMemoryCache(projectId, applicationId);
  const selfUserId = useSelfUserId();
  const selectable = useShareOut();
  const [note, setNote] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<number | null>(null);
  const [optOutBusy, setOptOutBusy] = useState(false);

  const pages = memory.data?.pages ?? [];
  // Offset paging over a newest-first list: a note saved between two page reads pushes a row
  // already shown into the next page. Keep the first copy of each id.
  const seen = new Set<number>();
  const rows = pages
    .flatMap((page) => page.rows)
    .filter((entry) => {
      if (seen.has(entry.id)) return false;
      seen.add(entry.id);
      return true;
    });
  const first = pages[0];
  const last = pages.at(-1);
  const people = Object.assign({}, ...pages.map((page) => page.people)) as Record<number, string>;
  const canDelete = first?.canDelete === true;
  // A failed refresh keeps the old rows on screen: say so above them. A write's note wins.
  const banner = note ?? (memory.error && rows.length > 0 ? agentMemoryErrorText(memory.error) : null);

  // The native Alert outlives the screen: a workspace or project switch can complete while it is
  // open. A stale confirmation must delete nothing rather than an id over there.
  const stillHere = () => {
    const now = getSession();
    return now.workspaceId === opened.workspaceId && now.projectId === opened.projectId;
  };

  const onDelete = (entry: AgentMemoryEntry) => {
    if (projectId === null || applicationId === null || deleting !== null) return;
    Alert.alert('Delete this note?', `${agentName} forgets it for everyone in this project. This cannot be undone.`, [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Delete',
        style: 'destructive',
        onPress: () => {
          if (!stillHere()) return;
          setDeleting(entry.id);
          deleteAgentMemoryEntry(projectId, applicationId, entry.id)
            .then(() => cache.forget(entry.id))
            .then(() => setNote(null))
            .catch(async (error: unknown) => {
              // Already gone (deleted elsewhere): what the user asked for is true. Drop the row.
              if (error instanceof AgentMemoryError && error.status === 404 && error.code === 'not_found') {
                await cache.forget(entry.id);
                setNote(null);
                return;
              }
              setNote(agentMemoryErrorText(error, AGENT_MEMORY_COPY.delete));
            })
            .finally(() => setDeleting(null));
        },
      },
    ]);
  };

  const applyOptOut = (optedOut: boolean) => {
    if (projectId === null || applicationId === null) return;
    setOptOutBusy(true);
    setAgentMemoryOptOut(projectId, applicationId, optedOut)
      .then(async (stored) => {
        await cache.setOptedOut(stored);
        setNote(null);
        // Opting out deletes the notes about the caller on the server: read the list again.
        if (stored) await cache.refresh();
      })
      .catch((error: unknown) => setNote(agentMemoryErrorText(error, AGENT_MEMORY_COPY.optOut)))
      .finally(() => setOptOutBusy(false));
  };

  const onOptOut = (optedOut: boolean) => {
    if (optOutBusy) return;
    if (!optedOut) {
      applyOptOut(false);
      return;
    }
    Alert.alert(
      `Stop ${agentName} remembering you?`,
      `${agentName} deletes what it remembers about you in this project and saves no more. You can turn this back on.`,
      [
        { text: 'Cancel', style: 'cancel' },
        { text: 'Turn off', style: 'destructive', onPress: () => stillHere() && applyOptOut(true) },
      ],
    );
  };

  const header = <Stack.Screen options={{ title: agentName === 'This agent' ? 'Agent memory' : `${agentName}'s memory` }} />;

  // A pull to refresh or a retry is a fresh look: a failed write's banner no longer applies.
  const refetch = () => {
    setNote(null);
    void memory.refetch();
  };

  if (applicationId === null || projectId === null || !supported || moved) {
    // No discovery document yet (an offline launch) means "unknown", not "not offered".
    const message =
      applicationId === null
        ? 'This agent could not be found.'
        : projectId === null
          ? 'Choose a project first.'
          : moved
            ? 'You switched project or workspace. Go back and open this agent again.'
            : workspace === null
              ? AGENT_MEMORY_COPY.offline
              : AGENT_MEMORY_COPY.notAvailable;
    return (
      <>
        {header}
        <AgentMemoryView
          agentName={agentName}
          status={{ kind: 'unavailable', message }}
          rows={[]}
          total={0}
          people={{}}
          selfUserId={selfUserId}
          canDelete={() => false}
          deleting={null}
          optedOut={false}
          optOutBusy
          banner={null}
          selectable={false}
          loadingMore={false}
          onDelete={() => {}}
          onOptOut={() => {}}
          onEndReached={() => {}}
          onRetry={() => {}}
        />
      </>
    );
  }

  return (
    <>
      {header}
      <AgentMemoryView
        agentName={agentName}
        status={
          memory.isLoading
            ? { kind: 'loading' }
            : memory.error
              ? memory.error instanceof AgentMemoryError && memory.error.reason === 'not_available'
                ? { kind: 'unavailable', message: agentMemoryErrorText(memory.error) }
                : { kind: 'error', message: agentMemoryErrorText(memory.error) }
              : { kind: 'ready' }
        }
        rows={rows}
        total={last?.total ?? rows.length}
        people={people}
        selfUserId={selfUserId}
        canDelete={(entry) => canDeleteEntry(entry, canDelete, selfUserId)}
        deleting={deleting}
        optedOut={first?.optedOut === true}
        optOutBusy={optOutBusy}
        banner={banner}
        selectable={selectable}
        loadingMore={memory.isFetchingNextPage}
        onDelete={onDelete}
        onOptOut={onOptOut}
        onEndReached={() => {
          // After a failed page read, wait for a pull to refresh instead of retrying on every scroll tick.
          if (memory.hasNextPage && !memory.isFetchingNextPage && !memory.isFetchNextPageError) void memory.fetchNextPage();
        }}
        onRetry={refetch}
        refreshControl={<RefreshControl refreshing={memory.isRefetching && !memory.isFetchingNextPage} onRefresh={refetch} tintColor={colors.accent} />}
      />
    </>
  );
}
