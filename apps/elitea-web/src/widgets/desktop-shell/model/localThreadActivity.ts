/**
 * The activity status of the sidebar's Local work threads (client contract
 * 1.12 glyphs, desktop state). A Local work turn runs on this machine, so its
 * state comes from the host, through the activity feed (`agent://event`):
 *
 * - a turn of the thread that is not finished runs; one that waits on an
 *   approval (`approval_request`) needs the person;
 * - a turn of the thread's cloud copy (handoff) runs or waits the same way
 *   (`cloudRuns`), and the server's frames for it (the shared overlay) count
 *   too.
 *
 * Unread is tracked here, on this machine: a thread whose turn finished
 * while it was not on screen is unread until the person opens it. The marks
 * live in the `el.` namespace (logout's sweep clears them); without storage
 * they last for the window's life.
 */
import { useEffect, useRef } from "react";

import { create, type StoreApi, type UseBoundStore } from "zustand";

import { createStorage } from "@/shared/lib/storage";
import {
  resolveRowActivity,
  useConversationActivityOverlay,
  type RowActivity,
} from "@/shared/lib/conversationActivity";

import type {
  ActivityFeedState,
  TrackedCloudRun,
  TrackedTurn,
} from "./activityFeed.store";
import { useActivityFeed } from "./activityFeed.store";

const UNREAD_KEY = "desktop.localWork.unread";

/**
 * A read mark stops mattering once its finish is no longer recent: the
 * glyph shows the grey check only for a day (`activityGlyph`).
 */
const READ_MARK_TTL_MS = 24 * 60 * 60 * 1000;

/** At most this many marks are kept: the newest finishes. */
const MAX_MARKS = 200;

/** Per thread: when its last turn finished (epoch ms), and whether the person saw it. */
interface ThreadMark {
  readonly finishedAt: number;
  readonly unread: boolean;
}

type Marks = Readonly<Record<string, ThreadMark>>;

function isMark(raw: unknown): raw is ThreadMark {
  if (typeof raw !== "object" || raw === null) return false;
  const { finishedAt, unread } = raw as Record<string, unknown>;
  return typeof finishedAt === "number" && typeof unread === "boolean";
}

function readMarks(): Marks {
  try {
    const raw = createStorage("local").getJSON(UNREAD_KEY, (value: unknown) => {
      if (typeof value !== "object" || value === null)
        throw new Error("not marks");
      return value as Record<string, unknown>;
    });
    return Object.fromEntries(
      Object.entries(raw ?? {}).filter((entry): entry is [string, ThreadMark] =>
        isMark(entry[1]),
      ),
    );
  } catch {
    return {};
  }
}

function writeMarks(marks: Marks): void {
  try {
    createStorage("local").setJSON(UNREAD_KEY, marks);
  } catch {
    // Storage unavailable: the marks last for this window.
  }
}

/**
 * The marks worth keeping: an unread one, or a read one while its finish is
 * recent; the newest `MAX_MARKS` of them. A thread that is no longer listed
 * stops being marked when its mark ages out, or when the cap drops it.
 */
function pruneMarks(marks: Marks, now: number = Date.now()): Marks {
  const kept = Object.entries(marks)
    .filter(([, mark]) => mark.unread || now - mark.finishedAt < READ_MARK_TTL_MS)
    .sort(([, a], [, b]) => b.finishedAt - a.finishedAt)
    .slice(0, MAX_MARKS);
  return Object.fromEntries(kept);
}

interface LocalThreadMarksState {
  readonly marks: Marks;
  readonly finished: (threadId: string, at: number, seen: boolean) => void;
  readonly read: (threadId: string) => void;
}

type MarksStore = UseBoundStore<StoreApi<LocalThreadMarksState>>;

/** A fresh marks store, read from storage (tests build their own). */
function createLocalThreadMarksStore(): MarksStore {
  return create<LocalThreadMarksState>((set) => ({
    marks: pruneMarks(readMarks()),
    finished: (threadId, at, seen) =>
      set((state) => {
        const marks = pruneMarks({
          ...state.marks,
          [threadId]: { finishedAt: at, unread: !seen },
        });
        writeMarks(marks);
        return { marks };
      }),
    read: (threadId) =>
      set((state) => {
        const mark = state.marks[threadId];
        if (mark === undefined || !mark.unread) return {};
        const marks = pruneMarks({
          ...state.marks,
          [threadId]: { ...mark, unread: false },
        });
        writeMarks(marks);
        return { marks };
      }),
  }));
}

let marksInstance: MarksStore | undefined;

function marksStore(): MarksStore {
  marksInstance ??= createLocalThreadMarksStore();
  return marksInstance;
}

function useMarksHook<T>(selector: (state: LocalThreadMarksState) => T): T {
  return marksStore()(selector);
}

/** The lazily-constructed marks store, with the zustand `getState` surface. */
export const useLocalThreadMarks = Object.assign(useMarksHook, {
  getState: (): LocalThreadMarksState => marksStore().getState(),
  /** Tests: start again from storage. */
  reset: (): void => {
    marksInstance = undefined;
  },
});

function isOnScreen(): boolean {
  return document.visibilityState === "visible" && document.hasFocus();
}

/**
 * Marks the threads whose turns finish while they are not on screen, and
 * clears the mark of the open thread. Mounted once, by the Local work section.
 */
export function useLocalThreadUnreadTracking(openThreadId: string): void {
  const open = useRef(openThreadId);
  open.current = openThreadId;

  useEffect(() => {
    let finished = new Set(
      Object.values(useActivityFeed.getState().turns)
        .filter((turn) => turn.finished)
        .map((turn) => turn.turnId),
    );
    return useActivityFeed.subscribe((state) => {
      const now = new Set<string>();
      for (const turn of Object.values(state.turns)) {
        if (!turn.finished) continue;
        now.add(turn.turnId);
        if (finished.has(turn.turnId) || turn.conversationId === null) continue;
        const seen = turn.conversationId === open.current && isOnScreen();
        useLocalThreadMarks
          .getState()
          .finished(turn.conversationId, Date.now(), seen);
      }
      finished = now;
    });
  }, []);

  useEffect(() => {
    if (openThreadId === "") return undefined;
    const read = (): void => {
      if (isOnScreen()) useLocalThreadMarks.getState().read(openThreadId);
    };
    read();
    window.addEventListener("focus", read);
    document.addEventListener("visibilitychange", read);
    return () => {
      window.removeEventListener("focus", read);
      document.removeEventListener("visibilitychange", read);
    };
  }, [openThreadId]);
}

function turnOf(
  turns: ActivityFeedState["turns"],
  threadId: string,
): TrackedTurn | undefined {
  return Object.values(turns)
    .filter((turn) => turn.conversationId === threadId && !turn.finished)
    .sort((a, b) => b.startedAt - a.startedAt)[0];
}

/** A Local work thread's activity, from the host's turns, its cloud copy and the local marks. Pure. */
export function localThreadActivity(
  threadId: string,
  uuid: string | undefined,
  turns: ActivityFeedState["turns"],
  cloudRuns: Readonly<Record<string, TrackedCloudRun>>,
  mark: ThreadMark | undefined,
): RowActivity {
  const turn = turnOf(turns, threadId);
  const cloud = uuid === undefined ? undefined : cloudRuns[uuid];
  const server = resolveRowActivity({
    id: threadId,
    ...(uuid !== undefined ? { uuid } : {}),
  });
  const lastActivityAt =
    mark === undefined
      ? server.lastActivityAt
      : new Date(mark.finishedAt).toISOString();
  return {
    state: liveState(turn, cloud, server),
    lastActivityAt,
    unread: mark?.unread === true || server.unread,
  };
}

/** The host's turn first, then the cloud copy's stream, then the server's frames. */
function liveState(
  turn: TrackedTurn | undefined,
  cloud: TrackedCloudRun | undefined,
  server: RowActivity,
): RowActivity["state"] {
  const waiting =
    (turn?.approvals.length ?? 0) > 0 || (cloud?.waiting ?? null) !== null;
  if (waiting || server.state === "needs_you") return "needs_you";
  const running = turn !== undefined || cloud?.running === true;
  return running || server.state === "running" ? "running" : "idle";
}

/**
 * How many of a folder's threads need the person, following the feed, the
 * overlay and the marks. It reads no thread list: the threads that can need
 * the person are those with a live host turn or cloud run in the folder, and
 * those the server's frames know by the folder's remembered UUIDs. `threads`
 * adds the folder's listed threads when the caller already holds them (an
 * open folder), for a frame keyed by the thread's id alone.
 */
export function useLocalNeedsYouCount(
  workspaceId: string,
  uuids: Readonly<Record<string, string>>,
  threads: readonly { readonly id: string }[] = [],
): number {
  const turns = useActivityFeed((state) => state.turns);
  const cloudRuns = useActivityFeed((state) => state.cloudRuns);
  const marks = useLocalThreadMarks((state) => state.marks);
  useConversationActivityOverlay((state) => state.entries);
  const candidates = new Set<string>([
    ...threads.map((thread) => thread.id),
    ...Object.keys(uuids),
  ]);
  for (const turn of Object.values(turns)) {
    if (turn.workspaceId === workspaceId && turn.conversationId !== null)
      candidates.add(turn.conversationId);
  }
  for (const run of Object.values(cloudRuns)) {
    if (run.workspaceId === workspaceId && run.conversationId !== null)
      candidates.add(run.conversationId);
  }
  return [...candidates].filter(
    (threadId) =>
      localThreadActivity(
        threadId,
        uuids[threadId],
        turns,
        cloudRuns,
        marks[threadId],
      ).state === "needs_you",
  ).length;
}

/** `localThreadActivity` for one row, following the feed, the overlay and the marks. */
export function useLocalThreadActivity(
  threadId: string,
  uuid: string | undefined,
): RowActivity {
  const turns = useActivityFeed((state) => state.turns);
  const cloudRuns = useActivityFeed((state) => state.cloudRuns);
  const mark = useLocalThreadMarks((state) => state.marks[threadId]);
  useConversationActivityOverlay((state) => state.entries);
  return localThreadActivity(threadId, uuid, turns, cloudRuns, mark);
}
