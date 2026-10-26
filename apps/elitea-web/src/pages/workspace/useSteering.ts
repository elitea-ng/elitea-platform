/**
 * Steering a local turn while it runs (IPC.md "Steering").
 *
 * - While the turn runs, a send queues the text against it (`agent_turn_steer`). The agent gets it at its next
 *   safe point; until then the person can change or remove it (`agent_turn_steer_update`).
 * - When the turn ends (any end), the texts it did not take (`steer_dropped` `turn_ended`) go as the next turn,
 *   joined with a blank line into one message. A text the host refused with `turn_not_running` (the run ended
 *   while it was sent) goes with them.
 * - "Stop and send" cancels the turn, then sends the text as the next turn once the turn ended.
 *
 * - When that next turn does not start, the texts go back into the composer (`unsent`): the person can change them,
 *   then send them again. They stay until a new turn starts.
 *
 * Only the turn this hook saw end sends: a turn that had ended before the composer mounted sends nothing again.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { describeWorkspaceError, type WorkspaceTurn } from '@/features/workspace';
import { toWorkspaceIpcError, type WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

/** The texts that did not start the next turn. A new `id` puts `text` into the input again. */
export interface UnsentSteers {
  id: number;
  text: string;
  /** Send `text` (as the input holds it now) as the next turn; resolves `true` once it started. */
  onRetry: (text: string) => Promise<boolean>;
}

export type PendingSteer = NonNullable<WorkspaceTurn['view']['pendingSteers']>[number];

export interface Steering {
  /** The turn runs: a send queues the text against it. */
  active: boolean;
  /** The messages that wait for the agent, oldest first. */
  pending: readonly PendingSteer[];
  /** The last refusal (a change of a message the agent has already, a full queue), or a next turn that did not start. */
  error: string | null;
  /** The texts that did not start the next turn, until a new turn starts; `onRetry` sends them. */
  unsent: UnsentSteers | null;
  /** Resolves `true` once the text is queued (or kept for the next turn): the composer then clears. */
  steer: (text: string) => Promise<boolean>;
  edit: (steerId: string, text: string) => Promise<void>;
  remove: (steerId: string) => Promise<void>;
  /** Stop the turn, then send `text` (after the texts the turn did not take) as the next turn. */
  stopAndSend: (text: string) => Promise<boolean>;
}

/** The message of the next turn: the parts that are not empty, joined with a blank line. */
export function nextTurnMessage(parts: readonly string[]): string {
  return parts.filter((part) => part.trim() !== '').join('\n\n');
}

const NONE: readonly PendingSteer[] = [];

/** The texts that did not start a next turn, and the turn whose end sent them. */
interface Unsent {
  id: number;
  text: string;
  turnId: string;
}

/** `sendNext` starts a turn with the prompt, as the composer's own send does; it resolves `false` when none started. */
export function useSteering(ipc: WorkspaceIpc, turn: WorkspaceTurn, sendNext: (prompt: string) => Promise<boolean>): Steering {
  const { turnId, busy, view } = turn;
  const ended = view.done !== undefined;
  const active = busy && turnId !== null && !ended;
  const [error, setError] = useState<string | null>(null);
  /** The texts that go with the next turn, besides the ones the turn did not take. */
  const carried = useRef<string[]>([]);
  /** The turn whose end this hook has handled. A turn already ended at mount counts as handled. */
  const handled = useRef<string | null>(ended ? turnId : null);
  const latestSend = useRef(sendNext);
  useEffect(() => {
    latestSend.current = sendNext;
  });
  const [unsent, setUnsent] = useState<Unsent | null>(null);
  const unsentCount = useRef(0);

  /** Send `parts` and the carried texts as the next turn, after the end of `ended`. */
  const flush = useCallback((parts: readonly string[], ended: string) => {
    const message = nextTurnMessage([...parts, ...carried.current]);
    carried.current = [];
    if (message === '') return;
    void latestSend.current(message).then((sent) => {
      if (sent) return;
      unsentCount.current += 1;
      setUnsent({ id: unsentCount.current, text: message, turnId: ended });
    });
  }, []);

  const retry = useCallback(async (text: string): Promise<boolean> => {
    setError(null);
    const sent = await latestSend.current(text);
    if (sent) setUnsent(null);
    return sent;
  }, []);
  // A new turn started (from "Send again" or from Send): the texts are not unsent now.
  const live = unsent !== null && unsent.turnId === turnId ? unsent : null;
  const unsentSteers = useMemo(() => (live === null ? null : { id: live.id, text: live.text, onRetry: retry }), [live, retry]);

  const endedSteers = view.endedSteers;
  useEffect(() => {
    if (turnId === null || !ended || busy || handled.current === turnId) return;
    handled.current = turnId;
    flush((endedSteers ?? []).map((steer) => steer.text), turnId);
  }, [turnId, ended, busy, endedSteers, flush]);

  /** Keep `text` for the next turn; send it now when the turn's end was already handled. */
  const carry = useCallback(
    (text: string) => {
      carried.current = [...carried.current, text];
      if (turnId !== null && handled.current === turnId) flush([], turnId);
    },
    [turnId, flush],
  );

  const steer = useCallback(
    async (text: string): Promise<boolean> => {
      if (turnId === null) return false;
      setError(null);
      try {
        await ipc.steerTurn(turnId, text);
        return true;
      } catch (refusal) {
        if (toWorkspaceIpcError(refusal).code !== 'turn_not_running') {
          setError(describeWorkspaceError(refusal));
          return false;
        }
        carry(text);
        return true;
      }
    },
    [ipc, turnId, carry],
  );

  const update = useCallback(
    async (steerId: string, text: string | null) => {
      if (turnId === null) return;
      setError(null);
      try {
        await ipc.updateSteer(turnId, steerId, text);
      } catch (refusal) {
        setError(describeWorkspaceError(refusal));
      }
    },
    [ipc, turnId],
  );

  const stopAndSend = useCallback(
    async (text: string): Promise<boolean> => {
      setError(null);
      carry(text);
      await turn.cancel();
      return true;
    },
    [carry, turn],
  );

  return {
    active,
    pending: view.pendingSteers ?? NONE,
    error: error ?? (unsentSteers === null ? null : t('workspace.steer.unsent', 'The messages you queued did not start a new turn. They are back in the message box: change them if you want, then send them again.')),
    unsent: unsentSteers,
    steer,
    edit: (steerId, text) => update(steerId, text),
    remove: (steerId) => update(steerId, null),
    stopAndSend,
  };
}
