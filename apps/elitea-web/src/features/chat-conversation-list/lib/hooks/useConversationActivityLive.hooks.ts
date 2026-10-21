/**
 * The live half of the conversation activity status (client contract 1.12).
 *
 * `useConversationActivityLiveEvents` follows one project's
 * `conversation_activity` frames on the project event stream
 * (GET /events/prompt_lib/{projectId}). The server sends one frame per turn
 * transition (a start, a pause, a finish, a failure, a stop) to the members
 * who can see the conversation, and one frame per read to the reader's other
 * devices. Each frame goes into the shared overlay
 * (`shared/lib/conversationActivity`), which the rows of every list read, so
 * no list is fetched again. The tab holds one connection per project, shared
 * by every list and by the handoff badges (`useSharedEventSource`), and
 * applies each frame once.
 *
 * If the stream drops or fails, frames may be lost: the listing is read
 * again on the next window focus, once for all the lists of the project
 * (and on every focus while the stream stays closed for good). A healthy
 * stream never refetches on focus.
 *
 * `useMarkConversationReadOnView` marks the open conversation read: when it
 * opens, when the window gets focus or becomes visible, and when a new answer
 * arrives while the person looks at it (`markConversationRead`). The row turns
 * read at once; the server's marker then takes its place, and a failed request
 * puts the previous marker back.
 */
import { useEffect, useMemo, useRef } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { useSharedEventSource } from '@/shared/api/sse';
import { markConversationRead } from '@/shared/api/generated/chat/chat';
import { getConfig } from '@/shared/config';
import {
  confirmReadLocally,
  CONVERSATION_ACTIVITY_EVENT,
  markReadLocally,
  onActivityEvent,
  useConversationActivityOverlay,
} from '@/shared/lib/conversationActivity';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;

/**
 * Per stream URL, frames may have been lost: the stream dropped (the browser
 * reconnects) or `closed` for good. `focus` is the focus event that last read
 * the listing again, so the lists of one project refetch once per focus.
 */
const streamGaps = new Map<string, { readonly closed: boolean; readonly focus?: Event }>();

/** Test reset. */
export function resetConversationActivityStreamGaps(): void {
  streamGaps.clear();
}

function useProjectStreamUrl(projectId: string | number | null | undefined): string | null {
  const config = getConfig();
  const serverUrl = config.status === 'ok' ? config.config.vite_server_url : null;
  const project = projectId === null || projectId === undefined || projectId === '' ? null : String(projectId);
  return useMemo(() => (project !== null && serverUrl ? `${serverUrl}/events/prompt_lib/${project}` : null), [project, serverUrl]);
}

/** Follow one project's activity frames while `projectId` is set. */
export function useConversationActivityLiveEvents(projectId: string | number | null | undefined): void {
  const queryClient = useQueryClient();
  const streamUrl = useProjectStreamUrl(projectId);
  useSharedEventSource(
    streamUrl,
    CONVERSATION_ACTIVITY_EVENT,
    { [CONVERSATION_ACTIVITY_EVENT]: onActivityEvent },
    {
      onError: (_event, readyState) => {
        if (streamUrl === null) return;
        const closed = readyState === 2;
        if (closed && streamGaps.get(streamUrl)?.closed !== true) {
          console.warn('The conversation activity stream is closed; the list refreshes on focus instead.');
        }
        streamGaps.set(streamUrl, { closed });
      },
    },
  );

  // The fallback: frames lost to a dropped or closed stream are fixed by the
  // listing, read again on focus.
  useEffect(() => {
    if (streamUrl === null) return undefined;
    const onFocus = (event: Event): void => {
      const gap = streamGaps.get(streamUrl);
      if (gap === undefined || gap.focus === event) return;
      // A dropped stream reconnected by itself: one read covers the gap. A
      // closed one stays a gap, read again on each focus.
      if (gap.closed) streamGaps.set(streamUrl, { closed: true, focus: event });
      else streamGaps.delete(streamUrl);
      void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
    };
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
  }, [queryClient, streamUrl]);
}

type OverlayState = ReturnType<typeof useConversationActivityOverlay.getState>;

/** The overlay entry of a conversation named by its id or its UUID. */
function overlayEntryOf(state: OverlayState, conversationId: string | undefined): OverlayState['entries'][string] | undefined {
  if (conversationId === undefined) return undefined;
  // A route may hand the id over as a number at runtime.
  const id = String(conversationId);
  return state.entries[state.uuids[id.toLowerCase()] ?? id];
}

function isOnScreen(): boolean {
  return document.visibilityState === 'visible' && document.hasFocus();
}

export interface UseMarkConversationReadOnViewParams {
  readonly projectId: string | undefined;
  /** The open conversation (its numeric id or UUID); absent for a new chat. */
  readonly conversationId: string | undefined;
  /** Changes when the open conversation gets a new message (its message count, for example). */
  readonly revision?: unknown;
}

/** Marks the open conversation read while the person can see it. */
export function useMarkConversationReadOnView({ projectId, conversationId, revision }: UseMarkConversationReadOnViewParams): void {
  // The open row turns unread again when a frame brings a new answer: read it
  // again. The route may name the conversation by its UUID: the overlay keys
  // it by its id, which the frames map the UUID to.
  const unreadHint = useConversationActivityOverlay((state) => overlayEntryOf(state, conversationId)?.unreadHint);
  const lastActivity = useConversationActivityOverlay((state) => overlayEntryOf(state, conversationId)?.lastActivityAt);
  // One request at a time; a read asked for meanwhile runs after it, so a new answer is never left unread.
  const inFlight = useRef(false);
  const again = useRef(false);
  const latestRead = useRef<(() => void) | undefined>(undefined);

  useEffect(() => {
    if (projectId === undefined || conversationId === undefined || conversationId === '') return undefined;
    const read = (): void => {
      if (!isOnScreen()) return;
      if (inFlight.current) {
        again.current = true;
        return;
      }
      inFlight.current = true;
      const undo = markReadLocally(conversationId);
      markConversationRead(projectId, conversationId)
        .then((response) => {
          if (response.status === 200) confirmReadLocally(String(response.data.conversation_id), response.data.read_at);
          else undo();
        })
        // Not read on the server: its answers are unread again.
        .catch(undo)
        .finally(() => {
          inFlight.current = false;
          if (again.current) {
            again.current = false;
            // The latest effect's read: the open conversation may have changed meanwhile.
            latestRead.current?.();
          }
        });
    };
    latestRead.current = read;
    read();
    const onVisible = (): void => {
      if (document.visibilityState === 'visible') read();
    };
    window.addEventListener('focus', read);
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      if (latestRead.current === read) latestRead.current = undefined;
      window.removeEventListener('focus', read);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [projectId, conversationId, revision, unreadHint, lastActivity]);
}
