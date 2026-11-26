/**
 * Agents talking to each other in a Local work thread (ADR-0061 decisions 3d and 3e, #3064 #3065; IPC.md "Hand-offs
 * and consults"): the host's `agent://event` kinds for a hand-off (`floor_handed`, on the handing turn) and a consult
 * (`consult_started`, `consult_finished`, on the calling turn). Excerpts only: the full texts are the committed side
 * messages Main stores.
 */

/** An agent of the thread, as the host names it. */
interface TeamTalkAgent {
  participant_id: number;
  name: string;
}

/** `floor_handed`: the turn's agent handed the floor; the agents named answer after this turn, in order. */
interface FloorHandedPayload {
  from: number;
  from_name: string;
  to: TeamTalkAgent[];
  /** At most 280 characters of the hand-off message. */
  message_excerpt: string;
}

/** `consult_started`: the turn's agent (`caller`) asks another agent (`callee`) and waits for its reply. */
interface ConsultStartedPayload {
  caller: number;
  callee: number;
  callee_name: string;
  /** At most 280 characters of the question. */
  question_excerpt: string;
  /** Runs on this computer with the folder; `false`: a cloud child turn without the folder. */
  local: boolean;
}

/** `consult_finished`: the consulted agent replied (`ok`), or the consult failed or was refused. */
interface ConsultFinishedPayload {
  caller: number;
  callee: number;
  callee_name: string;
  /** At most 280 characters of the reply (or why it failed). */
  reply_excerpt: string;
  ok: boolean;
  local: boolean;
}

/** The team-talk events, as `agent://event` carries them (without the turn id and seq). */
export type TeamTalkEvent =
  | { kind: 'floor_handed'; payload: FloorHandedPayload }
  | { kind: 'consult_started'; payload: ConsultStartedPayload }
  | { kind: 'consult_finished'; payload: ConsultFinishedPayload };
