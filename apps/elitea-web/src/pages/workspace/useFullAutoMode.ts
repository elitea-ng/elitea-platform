/**
 * Full auto of a thread (IPC.md "Full auto", ADR-0061 amendment of
 * 2026-11-26, issue 3079): the composer's fourth mode, above Bypass. Its turns
 * answer every approval Bypass answers and also the questions asked after the
 * agent read outside content, except a hard-deny list.
 *
 * It is the open thread's choice only: React state of this composer, never
 * stored, so a new thread, another thread or a restart starts without it (the
 * session is keyed per thread, `WorkspaceSessionPage`). Turning it on asks for
 * confirmation EVERY time. It is offered only while the organisation allows
 * it (`full_auto_status`); a forbidden mode is hidden and reads as off.
 */
import { useQuery } from '@tanstack/react-query';
import { useRef, useState } from 'react';

import { fullAutoAllowed } from '@/shared/desktop/fullAutoIpc';

export interface FullAutoMode {
  /** The organisation allows it: the mode is offered. */
  allowed: boolean;
  /** The thread's turns run in Full auto (never while it is not allowed). */
  on: boolean;
  /** A handed choice waits for the organisation's answer (#3144): no first send until it came. */
  settling: boolean;
  /** Turn it on (after the confirmation, every time) or off; `turnedOn` runs once it is on. */
  request: (on: boolean, turnedOn?: () => void) => void;
  /** The confirmation is open. */
  confirming: boolean;
  confirm: () => void;
  cancel: () => void;
}

const FULL_AUTO_STATUS_KEY = ['desktop', 'full-auto-status'] as const;

/**
 * `confirmed`: the person turned it on, and confirmed it, in the folder's new thread whose first message this (worktree)
 * thread was handed (#3144): the thread being created starts in it. Nothing is stored.
 */
export function useFullAutoMode(local: boolean, planMode: boolean, confirmed = false): FullAutoMode {
  const status = useQuery({ queryKey: FULL_AUTO_STATUS_KEY, queryFn: fullAutoAllowed, enabled: local, staleTime: 60_000 });
  const allowed = local && status.data === true;
  const [chosen, setChosen] = useState(confirmed);
  const [confirming, setConfirming] = useState(false);
  // Plan mode (`/plan` included) and a policy that stops allowing it end the choice: coming back asks again. While the
  // policy is read, a handed choice waits (it reads as off, and `settling` holds the first send).
  const known = !local || status.data !== undefined;
  if (chosen && (planMode || (known && !allowed))) setChosen(false);
  // What runs once a confirmed turn-on lands (the composer leaves plan mode and Bypass).
  const pending = useRef<(() => void) | undefined>(undefined);
  return {
    allowed,
    on: allowed && chosen,
    settling: chosen && !known,
    request: (next, turnedOn) => {
      if (!next) {
        setChosen(false);
        return;
      }
      if (!allowed) return;
      pending.current = turnedOn;
      setConfirming(true);
    },
    confirming: allowed && confirming,
    confirm: () => {
      setConfirming(false);
      setChosen(true);
      pending.current?.();
      pending.current = undefined;
    },
    cancel: () => {
      pending.current = undefined;
      setConfirming(false);
    },
  };
}
