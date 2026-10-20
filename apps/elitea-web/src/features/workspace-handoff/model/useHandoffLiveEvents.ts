/**
 * Live handoff state changes (ADR-0034). The project SSE stream
 * (GET /events/prompt_lib/{projectId}) will carry one `workspace.handoff`
 * frame per committed state change of a thread, published by elitea-main
 * when the server half of ADR-0034 lands (branch 5, the handoff executor).
 * The frame carries the conversation, its state, epoch and whether a take
 * back waits; nothing else. The server half relays the frame to the
 * thread's readers (its audience) on the project stream, which requires
 * the project's stream permission; not every project viewer receives it.
 *
 * On a frame the Cloud executor notice's read is updated in place and every
 * status read of that thread is invalidated, so the notice, the badges and
 * the composer follow without a poll. Until the stream publishes frames the
 * polls stay the live path; the stream is best-effort either way.
 */
import { type QueryClient, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';

import { useEventSource } from '@/shared/api/sse';
import { getConfig } from '@/shared/config';
import { WORKSPACE_HANDOFF_STATES } from '@/shared/api/workspaceHandoffApi';
import { applyActivityFrame, CONVERSATION_ACTIVITY_EVENT, parseActivityFrame } from '@/shared/lib/conversationActivity';
import type { WorkspaceHandoffState } from '@/shared/api/workspaceHandoffApi';

import { handoffStatusKey, handoffStatusManyKey } from './useHandoffStatus';

/** The SSE event name of a handoff state change. */
export const HANDOFF_EVENT = 'workspace.handoff';

interface HandoffEvent {
  readonly conversation_uuid: string;
  readonly state: WorkspaceHandoffState;
  readonly epoch: number;
  readonly reclaim_pending: boolean;
}

/** The Cloud executor notice's read of one thread (invalidate or update by it). */
export const handoffInfoKey = (projectId: string, conversationUuid: string) => ['workspace-handoff', projectId, conversationUuid.toLowerCase()] as const;

/** Parse one frame strictly; `null` for anything else. */
export function parseHandoffEvent(data: string): HandoffEvent | null {
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof value !== 'object' || value === null) return null;
  const frame = value as Record<string, unknown>;
  const { conversation_uuid: uuid, state, epoch, reclaim_pending: pending } = frame;
  if (typeof uuid !== 'string' || uuid === '' || typeof epoch !== 'number' || !Number.isInteger(epoch) || epoch < 0) return null;
  if (typeof pending !== 'boolean' || !WORKSPACE_HANDOFF_STATES.includes(state as WorkspaceHandoffState)) return null;
  return { conversation_uuid: uuid.toLowerCase(), state: state as WorkspaceHandoffState, epoch, reclaim_pending: pending };
}

/** Apply one change: update the notice's read, refresh the status reads. */
function applyHandoffEvent(queryClient: QueryClient, projectId: string, event: HandoffEvent): void {
  queryClient.setQueryData(handoffInfoKey(projectId, event.conversation_uuid), (previous: unknown) => {
    if (typeof previous !== 'object' || previous === null) {
      // The first frame may land before the notice's first read: seed the read
      // from the frame, or the in-flight fetch would leave the frame lost.
      return { state: event.state, epoch: event.epoch, reclaim_pending: event.reclaim_pending };
    }
    const current = previous as { epoch?: unknown };
    // An older frame (a reordered delivery) never overwrites a newer read.
    if (typeof current.epoch === 'number' && current.epoch > event.epoch) return previous;
    return { ...previous, state: event.state, epoch: event.epoch, reclaim_pending: event.reclaim_pending };
  });
  void queryClient.invalidateQueries({ queryKey: handoffInfoKey(projectId, event.conversation_uuid) });
  void queryClient.invalidateQueries({ queryKey: handoffStatusKey(event.conversation_uuid) });
  void queryClient.invalidateQueries({ queryKey: handoffStatusManyKey() });
  // Local again: the details reads go stale (the frame carries no thread id to narrow the prefix), so the composer and the badge follow.
  if (event.state === 'LOCAL') void queryClient.invalidateQueries({ queryKey: ['conversation', 'details'] });
}

/**
 * Subscribe to one project's handoff changes while `projectId` is set. On the
 * desktop the shared `useEventSource` rides the host's fetch-SSE with the
 * native token; in the browser, the cookie.
 */
export function useHandoffLiveEvents(projectId: string | number | null | undefined): void {
  const queryClient = useQueryClient();
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;
  const project = projectId === null || projectId === undefined || projectId === '' ? null : String(projectId);
  const streamUrl = useMemo(() => (project !== null && serverUrl ? `${serverUrl}/events/prompt_lib/${project}` : null), [project, serverUrl]);
  useEventSource(
    streamUrl,
    {
      [HANDOFF_EVENT]: (event: MessageEvent) => {
        const change = parseHandoffEvent(String(event.data));
        if (change === null || project === null) return;
        applyHandoffEvent(queryClient, project, change);
      },
      // The same stream carries the conversation activity frames (client
      // contract 1.12): the Local work rows of a thread that runs in the cloud
      // follow them without a second connection.
      [CONVERSATION_ACTIVITY_EVENT]: (event: MessageEvent) => {
        const frame = parseActivityFrame(String(event.data));
        if (frame !== null) applyActivityFrame(frame);
      },
    },
    {
      // CLOSED means the stream failed for good (403/500: the browser does not
      // retry it). The polls keep the thread current meanwhile, so say so.
      onError: (_event, readyState) => {
        if (readyState !== 2) return;
        console.warn('The handoff stream is closed; the fallback polls are the live path now.');
      },
    },
  );
}
