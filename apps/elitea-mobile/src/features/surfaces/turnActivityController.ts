import type { AppStateStatus } from 'react-native';

import type { LiveTurn } from '@/features/chat/turnController';
import { surfaceUrl } from '@/features/surfaces/surfaceLinks';
import {
  activityPhase,
  activityProps,
  isFinalPhase,
  type TurnActivityPhase,
  type TurnActivityProps,
} from '@/features/surfaces/turnActivityModel';
import { log } from '@/lib/log';
import type { NotificationPreview } from '@/security/policy';

/**
 * Starts, updates and ends the Live Activity of a running answer. LOCAL only
 * (no push): the app runs it from the turn's own stream while it is alive.
 *
 *  - Started once a turn has been running for `START_AFTER_MS` (short
 *    answers never get one), only while the app is in the foreground.
 *  - Updated when what it shows changes (phase, title, approximate), not on
 *    every streamed frame.
 *  - Ended when the turn finishes, fails, stops or pauses for the user (it
 *    lingers on the lock screen for a while), and at once when the turn
 *    leaves memory (thread closed, wipe, switch), lingering ones included.
 *  - When the app goes to the background the stream pauses (no background
 *    execution in this release): the activity is marked APPROXIMATE and
 *    stale, and comes back to the live state when the app is active again.
 *  - A new process ends whatever a previous one left running.
 */

export const START_AFTER_MS = 5000;
/** At most this many at once (iOS also caps an app's activities). */
export const MAX_ACTIVITIES = 2;
/** How long a finished activity stays on the lock screen. */
export const DONE_LINGER_MS = 2 * 60_000;
/** A pause waits for the user: it stays longer. */
export const WAITING_LINGER_MS = 15 * 60_000;

export type DismissalPolicy = 'default' | 'immediate' | { after: Date };

export interface ActivityHandle {
  update(props: TurnActivityProps, staleDate?: Date): Promise<void>;
  end(dismissalPolicy?: DismissalPolicy, props?: TurnActivityProps, contentDate?: Date): Promise<void>;
}

export interface ActivityFactory {
  start(props: TurnActivityProps, url?: string, staleDate?: Date): ActivityHandle;
  getInstances(): ActivityHandle[];
}

export type TurnHandle = { getSnapshot(): LiveTurn; subscribe(listener: () => void): () => void };

export interface TurnSource {
  list(): [string, TurnHandle][];
  subscribe(listener: () => void): () => void;
}

export interface TurnActivityDeps {
  factory: ActivityFactory | null;
  turns: TurnSource;
  appState: { current(): AppStateStatus; subscribe(listener: (state: AppStateStatus) => void): () => void };
  preview: { get(): NotificationPreview; subscribe(listener: () => void): () => void };
  workspaceId: () => string | null;
  /** The conversation's title (shown only when previews allow it); key is `<projectId>:<uuid>`. */
  titleFor: (key: string) => string | null;
  now?: () => number;
  setTimer?: (fn: () => void, ms: number) => unknown;
  clearTimer?: (handle: unknown) => void;
}

interface Tracked {
  controller: TurnHandle;
  firstSeen: number;
  /** False after the turn finished: a restart of the same turn counts from then. */
  wasActive: boolean;
  timer: unknown;
  activity: ActivityHandle | null;
  /**
   * The ended activity still shown until its dismissal date (a finished
   * answer, a pause). Kept so a wipe, a switch or the turn leaving memory
   * can still take it off the lock screen at once.
   */
  lingering: ActivityHandle | null;
  /** When the system takes the lingering activity off by itself. */
  lingerUntil: number;
  shown: string | null;
  unsubscribe: () => void;
}

export class TurnActivityController {
  private readonly tracked = new Map<string, Tracked>();
  private readonly offs: (() => void)[] = [];
  private readonly now: () => number;
  private readonly setTimer: (fn: () => void, ms: number) => unknown;
  private readonly clearTimer: (handle: unknown) => void;
  private foreground: boolean;

  constructor(private readonly deps: TurnActivityDeps) {
    this.now = deps.now ?? Date.now;
    this.setTimer = deps.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
    this.clearTimer = deps.clearTimer ?? ((h) => clearTimeout(h as ReturnType<typeof setTimeout>));
    // 'inactive' (Control Center, the Face ID sheet) is not a suspend; a locked app counts as background.
    this.foreground = deps.appState.current() !== 'background';
  }

  /** Ends leftovers of a previous process and follows the live turns. Returns a stop function. */
  start(): () => void {
    this.endLeftovers();
    this.offs.push(this.deps.turns.subscribe(() => this.syncRegistry()));
    this.offs.push(
      this.deps.appState.subscribe((state) => {
        const foreground = state !== 'background';
        if (foreground === this.foreground) return;
        this.foreground = foreground;
        for (const key of this.tracked.keys()) this.refresh(key);
      }),
    );
    this.offs.push(this.deps.preview.subscribe(() => this.tracked.forEach((_, key) => this.refresh(key))));
    this.syncRegistry();
    return () => this.stop();
  }

  stop(): void {
    for (const off of this.offs.splice(0)) off();
    this.endAll();
  }

  /** Ends every activity at once (wipe, switch, sign-out): nothing outlives the session. */
  endAll(): void {
    for (const key of [...this.tracked.keys()]) this.forget(key, 'immediate');
    // And whatever the system still shows that is not tracked (belt and braces).
    this.endLeftovers();
  }

  get activeCount(): number {
    let n = 0;
    const now = this.now();
    // A lingering one is still on the lock screen: it counts until its dismissal date.
    for (const t of this.tracked.values()) if (t.activity || (t.lingering && now < t.lingerUntil)) n++;
    return n;
  }

  private endLeftovers(): void {
    const factory = this.deps.factory;
    if (!factory) return;
    try {
      for (const leftover of factory.getInstances()) void leftover.end('immediate').catch(() => undefined);
    } catch (error) {
      log.warn('live activities not listed', { error });
    }
  }

  private syncRegistry(): void {
    const live = new Map(this.deps.turns.list());
    for (const key of [...this.tracked.keys()]) {
      if (!live.has(key)) this.forget(key, 'immediate');
    }
    for (const [key, controller] of live) {
      const known = this.tracked.get(key);
      if (known?.controller === controller) continue;
      // A new turn in the same conversation (a continuation, a regeneration).
      if (known) this.forget(key, 'immediate');
      const unsubscribe = controller.subscribe(() => this.refresh(key));
      this.tracked.set(key, { controller, firstSeen: this.now(), wasActive: true, timer: null, activity: null, lingering: null, lingerUntil: 0, shown: null, unsubscribe });
      this.refresh(key);
    }
  }

  private snapshotOf(key: string): LiveTurn | null {
    return this.tracked.get(key)?.controller.getSnapshot() ?? null;
  }

  private propsFor(key: string, phase: TurnActivityPhase, t: Tracked): TurnActivityProps {
    return activityProps({
      phase,
      preview: this.deps.preview.get(),
      conversationTitle: this.deps.titleFor(key),
      approximate: !this.foreground,
      startedAt: t.firstSeen,
    });
  }

  private refresh(key: string): void {
    const t = this.tracked.get(key);
    if (!t) return;
    const live = this.snapshotOf(key);
    const phase = live ? activityPhase(live) : null;
    if (phase === null) {
      this.forget(key, 'immediate');
      return;
    }
    if (isFinalPhase(phase)) {
      t.wasActive = false;
      this.finish(key, t, phase);
      return;
    }
    if (!t.wasActive) {
      // The same turn runs again (Reconnect, a retry): its own five seconds.
      t.wasActive = true;
      t.firstSeen = this.now();
    }
    // What lingers ("Your agent needs you") is no longer true: off at once, one per conversation.
    this.endLingering(t);
    if (!t.activity) {
      if (t.timer === null && this.foreground) {
        const wait = Math.max(0, t.firstSeen + START_AFTER_MS - this.now());
        t.timer = this.setTimer(() => {
          t.timer = null;
          this.maybeStart(key);
        }, wait);
      }
      return;
    }
    this.show(t, this.propsFor(key, phase, t));
  }

  private maybeStart(key: string): void {
    const t = this.tracked.get(key);
    const factory = this.deps.factory;
    const workspaceId = this.deps.workspaceId();
    if (!t || t.activity || !factory || workspaceId === null || !this.foreground) return;
    if (this.activeCount >= MAX_ACTIVITIES) return;
    const live = this.snapshotOf(key);
    const phase = live ? activityPhase(live) : null;
    if (phase === null || isFinalPhase(phase)) return;
    const props = this.propsFor(key, phase, t);
    const split = key.indexOf(':');
    const uuid = key.slice(split + 1);
    // The key's project: a tap after a project change must not open the thread under another project.
    const projectId = split > 0 ? Number(key.slice(0, split)) : Number.NaN;
    try {
      t.activity = factory.start(
        props,
        surfaceUrl({ kind: 'conversation', ref: uuid, projectId: Number.isSafeInteger(projectId) ? projectId : null }, workspaceId),
      );
      t.shown = JSON.stringify(props);
    } catch (error) {
      // Live Activities turned off in Settings, or the system refused one.
      log.info('live activity not started', { error });
    }
  }

  private show(t: Tracked, props: TurnActivityProps): void {
    const key = JSON.stringify(props);
    if (!t.activity || key === t.shown) return;
    t.shown = key;
    // Approximate: stale at once, so the system de-emphasises it too.
    const stale = props.approximate ? new Date(this.now()) : undefined;
    t.activity.update(props, stale).catch((error: unknown) => log.warn('live activity not updated', { error }));
  }

  private finish(key: string, t: Tracked, phase: TurnActivityPhase): void {
    if (t.timer !== null) {
      this.clearTimer(t.timer);
      t.timer = null;
    }
    const activity = t.activity;
    t.activity = null;
    if (!activity) return;
    const linger = phase === 'waiting' ? WAITING_LINGER_MS : DONE_LINGER_MS;
    t.lingering = activity;
    t.lingerUntil = this.now() + linger;
    activity
      .end({ after: new Date(this.now() + linger) }, this.propsFor(key, phase, t), new Date(this.now()))
      .catch((error: unknown) => log.warn('live activity not ended', { error }));
  }

  private endLingering(t: Tracked): void {
    const lingering = t.lingering;
    if (!lingering) return;
    t.lingering = null;
    lingering.end('immediate').catch((error: unknown) => log.warn('live activity not ended', { error }));
  }

  private forget(key: string, policy: DismissalPolicy): void {
    const t = this.tracked.get(key);
    if (!t) return;
    this.tracked.delete(key);
    t.unsubscribe();
    if (t.timer !== null) this.clearTimer(t.timer);
    for (const handle of [t.activity, t.lingering]) {
      if (handle) handle.end(policy).catch((error: unknown) => log.warn('live activity not ended', { error }));
    }
  }
}
