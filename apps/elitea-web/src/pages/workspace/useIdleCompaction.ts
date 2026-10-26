/**
 * Auto-compaction of an idle thread (on by default; the Context tab's
 * switch turns it off for the app). After a thread was idle for
 * `IDLE_COMPACT_MS` (the provider's prompt cache has expired by then, so the
 * next call pays for the whole history again), and when its conversation is
 * large enough to gain from a summary, the next message first summarizes
 * the conversation: the same as `/compact` (IPC.md "Context window"). The
 * notice above the composer says so and can cancel it.
 */
import { useEffect, useRef, useSyncExternalStore } from 'react';

import type { ContextMeter } from '@/shared/desktop/workspaceIpc';
import { createStorage } from '@/shared/lib/storage';

/** How long a thread is idle before its next message compacts first. */
export const IDLE_COMPACT_MS = 5 * 60_000;
/** The conversation's share of the request it takes for a summary to gain (the host keeps the newest 20k tokens). */
const MIN_MESSAGE_TOKENS = 40_000;
/** The same, for a host that reports no parts: the whole request. */
const MIN_USED_TOKENS = 60_000;
const CHECK_MS = 30_000;
const OFF_KEY = 'desktop.idleCompaction.off';

const listeners = new Set<() => void>();

function readEnabled(): boolean {
  try {
    return createStorage('local').get(OFF_KEY) !== '1';
  } catch {
    return true;
  }
}

/** Turns idle compaction on or off for the app. */
export function setIdleCompaction(on: boolean): void {
  try {
    const storage = createStorage('local');
    if (on) storage.remove(OFF_KEY);
    else storage.set(OFF_KEY, '1');
  } catch {
    // Storage may be unavailable: nothing is kept.
  }
  for (const listener of listeners) listener();
}

const subscribe = (listener: () => void): (() => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};

/** Whether idle compaction is on (the Context tab's switch). */
export function useIdleCompactionEnabled(): boolean {
  return useSyncExternalStore(subscribe, readEnabled, () => true);
}

/** The conversation is large enough for a summary to gain. */
export function worthCompacting(meter: ContextMeter): boolean {
  if (meter.parts !== undefined) return meter.parts.message_tokens >= MIN_MESSAGE_TOKENS;
  return meter.used_tokens >= MIN_USED_TOKENS;
}

/** When the thread went idle: the end of the last turn this visit, else the last measurement of a recorded one. */
function idleSince(endedAt: number | null, meter: ContextMeter): number | null {
  if (endedAt !== null) return endedAt;
  const measured = meter.measured_at === undefined ? Number.NaN : Date.parse(meter.measured_at);
  return Number.isNaN(measured) ? null : measured;
}

export interface IdleCompactionInput {
  meter: ContextMeter | undefined;
  /** A turn runs or a message is being sent. */
  busy: boolean;
  /** The next message already compacts (`/compact`, or armed before). */
  armed: boolean;
  /** Arm the next message to compact first. */
  arm: () => void;
  /** The thread is a local one (not the cloud copy, not a pipeline's run). */
  local: boolean;
}

export function useIdleCompaction({ meter, busy, armed, arm, local }: IdleCompactionInput): void {
  const enabled = useIdleCompactionEnabled();
  const endedAt = useRef<number | null>(null);
  const wasBusy = useRef(busy);
  // A turn ended (or was cancelled): the thread is idle from now; once it
  // is armed (or cancelled), this idle period does not arm it again.
  const done = useRef(false);
  if (wasBusy.current !== busy) {
    wasBusy.current = busy;
    if (!busy) endedAt.current = Date.now();
    done.current = false;
  }
  const armRef = useRef(arm);
  armRef.current = arm;
  useEffect(() => {
    if (!enabled || !local || busy || armed || meter === undefined || done.current || !worthCompacting(meter)) return undefined;
    const check = (): void => {
      const since = idleSince(endedAt.current, meter);
      if (since === null || Date.now() - since < IDLE_COMPACT_MS || done.current) return;
      done.current = true;
      armRef.current();
    };
    check();
    const id = window.setInterval(check, CHECK_MS);
    return () => window.clearInterval(id);
  }, [enabled, local, busy, armed, meter]);
}
