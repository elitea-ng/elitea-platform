import { Platform } from 'react-native';

import { log } from '@/lib/log';
import { registerStoredWipeHook, registerWipeHook } from '@/security/endSession';
import { maySurfaceWorkspace } from '@/features/surfaces/surfaceGate';
import { sameContent, signedOutSnapshot, type SurfaceSnapshot } from '@/features/surfaces/surfaceSnapshot';

/**
 * Writes the widget snapshot to the App Group (expo-widgets keeps the
 * timeline in the group's UserDefaults) and asks WidgetKit to redraw. iOS
 * only; never throws; an unchanged snapshot is not rewritten (WidgetKit
 * budgets reloads).
 *
 * The app does not run in the background (no background execution in this
 * release), so a "running" count could not be corrected once the app is
 * suspended. A snapshot with running answers therefore carries a second
 * timeline entry, `RUNNING_STALE_MS` later, without them: the widget stops
 * claiming an answer is running long after it could have ended.
 */

export const RUNNING_STALE_MS = 10 * 60_000;

export interface WidgetSink {
  updateTimeline(entries: { date: Date; props: SurfaceSnapshot }[]): void;
}

export function timelineFor(snapshot: SurfaceSnapshot): { date: Date; props: SurfaceSnapshot }[] {
  const entries = [{ date: new Date(snapshot.at), props: snapshot }];
  if (snapshot.running > 0) {
    entries.push({ date: new Date(snapshot.at + RUNNING_STALE_MS), props: { ...snapshot, running: 0, at: snapshot.at + RUNNING_STALE_MS } });
  }
  return entries;
}

let sink: WidgetSink | null | undefined;
/** The workspace the stored snapshot belongs to (null: the signed-out snapshot). */
let writtenFor: string | null = null;
let last: SurfaceSnapshot | null = null;

function defaultSink(): WidgetSink | null {
  if (Platform.OS !== 'ios') return null;
  try {
    // Loaded lazily, iOS only: registering the layout writes to the App Group, and there
    // is no widget module on Android or web.
    // eslint-disable-next-line @typescript-eslint/no-require-imports
    return (require('@/features/surfaces/CountsWidget') as typeof import('@/features/surfaces/CountsWidget')).countsWidget;
  } catch (error) {
    log.warn('widgets unavailable', { error });
    return null;
  }
}

function currentSink(): WidgetSink | null {
  if (sink === undefined) sink = defaultSink();
  return sink;
}

/** Test seam. */
export function __setWidgetSinkForTests(next: WidgetSink | null | undefined): void {
  sink = next;
  writtenFor = null;
  last = null;
}

/** Writes `snapshot` for `workspaceId` (null: the signed-out one); see surfaceGate for when a workspace may be written. */
export function publishSurfaceSnapshot(workspaceId: string | null, snapshot: SurfaceSnapshot): void {
  if (!maySurfaceWorkspace(workspaceId)) return;
  if (sameContent(last, snapshot) && writtenFor === workspaceId) return;
  const target = currentSink();
  if (target === null) return;
  try {
    target.updateTimeline(timelineFor(snapshot));
    last = snapshot;
    writtenFor = workspaceId;
  } catch (error) {
    last = null;
    log.warn('widget snapshot not written', { error });
  }
}

/**
 * The signed-out snapshot: no count, title or link of any workspace is left
 * in the App Group. Runs on every wipe (log out, revocation) and on a
 * workspace switch (the next workspace writes its own after its first sync).
 */
export function clearSurfaceSnapshot(now: number = Date.now()): void {
  publishSurfaceSnapshot(null, signedOutSnapshot(now));
}

registerWipeHook('surface_snapshot', () => clearSurfaceSnapshot());
// A workspace removed while another is active: clear only when the stored
// snapshot is that workspace's (it never is after a switch; defensive).
registerStoredWipeHook('surface_snapshot', (workspaceId) => {
  if (writtenFor === workspaceId) clearSurfaceSnapshot();
});
