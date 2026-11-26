/**
 * Steering (IPC.md "Steering"): the messages the person sends while a local turn runs. Kept beside
 * `workspaceIpc.ts`: the two commands and the three `agent://event` kinds of the queue.
 */

/** Why a queued message does not wait any more: the person removed it, or the turn ended before a safe point. */
type SteerDropReason = 'removed' | 'turn_ended';

/** The queue's events, as `agent://event` carries them (without the turn id and seq). */
export type SteeringEvent =
  /** The message waits for the next safe point; sent again with the next `revision` after a change. */
  | { kind: 'steer_queued'; payload: { steer_id: string; text: string; revision: number } }
  /** The agent loop took it: a user message of the turn from this point. */
  | { kind: 'steer_injected'; payload: { steer_id: string; text: string } }
  | { kind: 'steer_dropped'; payload: { steer_id: string; text: string; reason: SteerDropReason } };

/** The steering half of `WorkspaceIpc`. */
export interface SteeringCommands {
  /** `agent_turn_steer`: queue `text` against the running turn; resolves with its id. Rejects `turn_not_running` once the run ended. */
  steerTurn(turnId: string, text: string): Promise<string>;
  /** `agent_turn_steer_update`: change a waiting message, or remove it (`text` `null`). Rejects `steer_not_pending` once the agent has it. */
  updateSteer(turnId: string, steerId: string, text: string | null): Promise<void>;
}

type Invoke = <T>(command: string, args?: Record<string, unknown>) => Promise<T>;

export function steeringCommands(invoke: Invoke): SteeringCommands {
  return {
    steerTurn: async (turnId, text) => (await invoke<{ steer_id: string }>('agent_turn_steer', { turn_id: turnId, text })).steer_id,
    updateSteer: (turnId, steerId, text) => invoke<void>('agent_turn_steer_update', { turn_id: turnId, steer_id: steerId, text }),
  };
}

/**
 * Watching and stepping into an exchange (ADR-0061 decision 2c, IPC.md "Several agents in a thread"): the host keeps
 * the routed order of a person's message; these only tell it to stop the rest or to drop one agent of it.
 */
export interface ExchangeCommands {
  /** `agent_exchange_stop`: cancel the running turn of the exchange and start none of the agents still queued. */
  stopExchange(turnId: string): Promise<void>;
  /**
   * `agent_exchange_skip`: drop `participantId` from the agents still queued; resolves with the ones that remain.
   * `handId`: the hand-off that queued it (ADR-0061 decision 3d), so the host skips that hand-off with Main.
   */
  skipInExchange(turnId: string, participantId: number, handId?: string): Promise<number[]>;
}

export function exchangeCommands(invoke: Invoke): ExchangeCommands {
  return {
    stopExchange: (turnId) => invoke<void>('agent_exchange_stop', { turn_id: turnId }),
    skipInExchange: async (turnId, participantId, handId) =>
      (await invoke<{ remaining: number[] }>('agent_exchange_skip', { turn_id: turnId, participant_id: participantId, ...(handId === undefined ? {} : { hand_id: handId }) })).remaining,
  };
}
