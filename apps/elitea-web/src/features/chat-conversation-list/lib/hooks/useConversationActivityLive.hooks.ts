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
 * no list is fetched again. If the stream fails, the rows keep the listing's
 * state, and the listing is read again when the window gets focus.
 *
 * `useMarkConversationReadOnView` marks the open conversation read: when it
 * opens, when the window gets focus or becomes visible, and when a new answer
 * arrives while the person looks at it (`markConversationRead`). The row turns
 * read at once; the server's marker then takes its place.
 */
import { useEffect, useMemo, useRef } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { useEventSource } from '@/shared/api/sse';
import { markConversationRead } from '@/shared/api/generated/chat/chat';
import { getConfig } from '@/shared/config';
import {
  applyActivityFrame,
  confirmReadLocally,
  CONVERSATION_ACTIVITY_EVENT,
  markReadLocally,
  parseActivityFrame,
  useConversationActivityOverlay,
} from '@/shared/lib/conversationActivity';

const FOLDER_LIST_KEY = ['folder', 'list'] as const;

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
  useEventSource(
    streamUrl,
    {
      [CONVERSATION_ACTIVITY_EVENT]: (event: MessageEvent) => {
        const frame = parseActivityFrame(String(event.data));
        if (frame !== null) applyActivityFrame(frame);
      },
    },
    {
      onError: (_event, readyState) => {
        if (readyState !== 2) return;
        console.warn('The conversation activity stream is closed; the list refreshes on focus instead.');
      },
    },
  );

  // The fallback: a missed frame is fixed by the listing, read again on focus.
  useEffect(() => {
    if (streamUrl === null) return undefined;
    const onFocus = (): void => void queryClient.invalidateQueries({ queryKey: FOLDER_LIST_KEY });
    window.addEventListener('focus', onFocus);
    return () => window.removeEventListener('focus', onFocus);
  }, [queryClient, streamUrl]);
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
  // The open row turns unread again when a frame brings a new answer: read it again.
  const unreadHint = useConversationActivityOverlay((state) => (conversationId === undefined ? undefined : state.entries[conversationId]?.unreadHint));
  const lastActivity = useConversationActivityOverlay((state) => (conversationId === undefined ? undefined : state.entries[conversationId]?.lastActivityAt));
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
      markReadLocally(conversationId);
      markConversationRead(projectId, conversationId)
        .then((response) => {
          if (response.status === 200) confirmReadLocally(String(response.data.conversation_id), response.data.read_at);
        })
        .catch(() => undefined)
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
