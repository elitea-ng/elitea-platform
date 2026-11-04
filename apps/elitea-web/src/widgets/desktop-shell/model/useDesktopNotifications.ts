/**
 * The desktop's ONE platform notifications subscription. The frame holds it
 * (through `ReclaimRequestToast`, which the frame always mounts) and it feeds
 * everything that shows a notification:
 *
 *  - the sidebar's bell — its unread dot (`useDesktopBell`);
 *  - the "Take back requested" banner — the latest unread rows;
 *  - native OS notices — one per notification that arrives while the window
 *    is in the background (the host skips them while it is focused), unless
 *    Settings › Notifications turned "Show system notifications" off (the
 *    host drops a `run_finished` that ran on this computer — it noticed that
 *    chain itself — and any row it already showed);
 *  - the Dock badge — the unread count, beside the approvals waiting;
 *  - the cloud handoff — a fresh `run_finished` of a cloud run goes to the
 *    host (`handoff_run_finished`), notices on or off, so a thread sent with
 *    "Take back when done" comes back as soon as its run completes (ADR-0045
 *    decision 8; the host also catches up by itself every few minutes).
 *
 * It listens like the web bell does: the stream signals, the list says what;
 * once the stream's reconnects are spent, the list is polled slowly — in the
 * background too, since a native notice is shown only while the window is
 * NOT focused (a poll paused there would announce nothing). The rows already
 * unread when the app starts are not announced again.
 *
 * The Dock badge is the host's and outlives this hook, so it is set back to
 * zero when the feed ends: signed out (no personal project) or unmounted (the
 * frame leaves, a deployment switch).
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import { NOTIFICATIONS_QUERY_ROOT, systemNoticesEnabled, systemNoticeText, useNotificationsList, useNotificationsSSE, type NormalizedNotification } from '@/features/notifications';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';
import { tauriHandoffIpc, type HandoffIpc } from '@/shared/desktop/handoffIpc';
import { platformNotice, type PlatformNotice } from '@/shared/desktop/platformNotice';
import { personalProjectIdOf } from '@/shared/lib/personalProject';

const SLOW_POLL_MS = 60_000;
/** A burst (a reconnect after a long sleep) shows this many notices, not one per row. */
const MAX_NOTICES_AT_ONCE = 3;

interface BellState {
  readonly projectId: string | undefined;
  readonly hasUnread: boolean;
}

type BellStore = UseBoundStore<StoreApi<BellState>>;

let bellStore: BellStore | undefined;

function resolveBellStore(): BellStore {
  bellStore ??= create<BellState>(() => ({ projectId: undefined, hasUnread: false }));
  return bellStore;
}

/** What the sidebar's bell shows. */
export function useDesktopBell(): BellState {
  return resolveBellStore()((state) => state);
}

export interface DesktopNotificationsFeed {
  readonly projectId: string | undefined;
  /** The latest unread notifications, newest first. */
  readonly rows: readonly NormalizedNotification[];
}

/** What the feed tells the cloud handoff host. */
type HandoffRunFinished = Pick<HandoffIpc, 'runFinished'>;

/** A cloud run's `run_finished`, forwarded to the handoff host whatever the notice setting says. */
function forwardCloudRun(row: NormalizedNotification, handoff: HandoffRunFinished | undefined): void {
  const uuid = row.meta?.conversationUuid;
  if (handoff === undefined || row.eventType !== 'run_finished' || row.meta?.origin !== 'cloud' || uuid === undefined || uuid === '') return;
  void handoff.runFinished(uuid, row.meta.origin, row.meta.outcome).catch(() => undefined);
}

/** Announces the rows not seen before, natively; the first read only marks what is there. */
function useNativeNotices(rows: readonly NormalizedNotification[] | undefined, host: PlatformNotice | undefined, handoff: HandoffRunFinished | undefined): void {
  const seen = useRef<Set<string> | null>(null);
  useEffect(() => {
    if (rows === undefined) return;
    if (seen.current === null) {
      seen.current = new Set(rows.map((row) => row.id));
      return;
    }
    const known = seen.current;
    const fresh = rows.filter((row) => !known.has(row.id));
    for (const row of fresh) {
      known.add(row.id);
      forwardCloudRun(row, handoff);
    }
    if (host === undefined || !systemNoticesEnabled()) return;
    for (const row of fresh.slice(0, MAX_NOTICES_AT_ONCE)) {
      const { title, body } = systemNoticeText(row);
      void host.show(title, body, { id: row.id, eventType: String(row.eventType), origin: row.meta?.origin }).catch(() => undefined);
    }
  }, [rows, host, handoff]);
}

function usePersonalProjectId(): string | undefined {
  const author = useGetCurrentAuthor();
  return personalProjectIdOf(unwrapBody(author.data));
}

/** `notices` and `handoff` stand in for the host (tests); by default the desktop host's, none in a plain browser tab. */
export function useDesktopNotifications(notices?: PlatformNotice, handoff?: HandoffRunFinished): DesktopNotificationsFeed {
  const host = useMemo(() => notices ?? platformNotice(), [notices]);
  const handoffHost = useMemo(() => handoff ?? tauriHandoffIpc(), [handoff]);
  const queryClient = useQueryClient();
  const projectId = usePersonalProjectId();
  const [pushed, setPushed] = useState(false);
  const onNotify = useCallback(() => {
    setPushed(true);
    void queryClient.invalidateQueries({ queryKey: NOTIFICATIONS_QUERY_ROOT });
  }, [queryClient]);
  const { streamDead } = useNotificationsSSE(projectId, onNotify);
  const list = useNotificationsList(
    { projectId: projectId ?? '', pageSize: 10, params: { only_new: true } },
    { enabled: projectId !== undefined, refetchInterval: streamDead ? SLOW_POLL_MS : false, refetchIntervalInBackground: streamDead },
  );
  // Every read is authoritative: a push only lights the dot until the list says otherwise.
  const { data, dataUpdatedAt } = list;
  useEffect(() => {
    if (data !== undefined) setPushed(false);
  }, [data, dataUpdatedAt]);
  const unread = projectId === undefined ? 0 : (data?.total ?? 0);
  const hasUnread = pushed || unread > 0;
  useEffect(() => resolveBellStore().setState({ projectId, hasUnread }), [projectId, hasUnread]);
  useEffect(() => {
    void host?.setUnread(unread).catch(() => undefined);
  }, [host, unread]);
  // The badge is the host's: clear it when this feed goes away.
  useEffect(() => () => void host?.setUnread(0).catch(() => undefined), [host]);
  useNativeNotices(data?.rows, host, handoffHost);
  return { projectId, rows: data?.rows ?? [] };
}
