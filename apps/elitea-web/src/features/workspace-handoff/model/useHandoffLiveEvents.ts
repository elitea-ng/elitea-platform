/**
 * Live handoff state changes (ADR-0034): elitea-main publishes one
 * `workspace.handoff` frame on the project SSE stream
 * (GET /elitea_core/events/prompt_lib/{projectId}) for every committed state
 * change of a thread, and relays it only to the thread's readers. A frame
 * carries the conversation, its state, epoch and whether a take back waits;
 * nothing else.
 *
 * On a frame the Cloud executor notice's read is updated in place and every
 * status read of that thread (the page's and the desktop sidebar's batched
 * one) is invalidated, so the notice, the badges and the composer follow
 * without a poll. The polls stay, slowed to {@link HANDOFF_FALLBACK_POLL_MS},
 * for a frame the best-effort stream lost.
 */
import { type QueryClient, useQueryClient } from '@tanstack/react-query';
import { useMemo } from 'react';

import { useEventSource } from '@/shared/api/sse';
import { getConfig } from '@/shared/config';

import { handoffStatusKey, handoffStatusManyKey } from './useHandoffStatus';

/** The SSE event name of a handoff state change. */
export const HANDOFF_EVENT = 'workspace.handoff';

/** How often the reads still poll, now that the stream carries the changes. */
export const HANDOFF_FALLBACK_POLL_MS = 60_000;

const STATES = ['LOCAL', 'HANDING_OFF', 'CLOUD_IDLE', 'CLOUD_RUNNING', 'RECLAIMING'] as const;

export interface HandoffEvent {
  readonly conversation_uuid: string;
  readonly state: (typeof STATES)[number];
  readonly epoch: number;
  readonly reclaim_pending: boolean;
}

/** The Cloud executor notice's read of one thread (invalidate or update by it). */
export const handoffInfoKey = (projectId: string, conversationUuid: string) => ['workspace-handoff', projectId, conversationUuid] as const;

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
  if (typeof pending !== 'boolean' || !STATES.includes(state as HandoffEvent['state'])) return null;
  return { conversation_uuid: uuid.toLowerCase(), state: state as HandoffEvent['state'], epoch, reclaim_pending: pending };
}

/** Apply one change: update the notice's read, refresh the status reads. */
export function applyHandoffEvent(queryClient: QueryClient, projectId: string, event: HandoffEvent): void {
  queryClient.setQueryData(handoffInfoKey(projectId, event.conversation_uuid), (previous: unknown) => {
    if (typeof previous !== 'object' || previous === null) return previous;
    const current = previous as { epoch?: unknown };
    // An older frame (a reordered delivery) never overwrites a newer read.
    if (typeof current.epoch === 'number' && current.epoch > event.epoch) return previous;
    return { ...previous, state: event.state, epoch: event.epoch, reclaim_pending: event.reclaim_pending };
  });
  void queryClient.invalidateQueries({ queryKey: handoffInfoKey(projectId, event.conversation_uuid) });
  void queryClient.invalidateQueries({ queryKey: handoffStatusKey(event.conversation_uuid) });
  void queryClient.invalidateQueries({ queryKey: handoffStatusManyKey() });
  // Local again: the composer and the badge read the conversation.
  if (event.state === 'LOCAL') void queryClient.invalidateQueries({ queryKey: ['conversation'] });
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
  const streamUrl = useMemo(() => (project !== null && serverUrl ? `${serverUrl}/elitea_core/events/prompt_lib/${project}` : null), [project, serverUrl]);
  useEventSource(streamUrl, {
    [HANDOFF_EVENT]: (event: MessageEvent) => {
      const change = parseHandoffEvent(String(event.data));
      if (change === null || project === null) return;
      applyHandoffEvent(queryClient, project, change);
    },
  });
}
