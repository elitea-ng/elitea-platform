/**
 * Turn-taking on the client (ADR-0050 decisions 7c to 7f). Main decides who
 * answers; a client only says who a message is addressed to, shows the
 * conversation's floor, and folds agent-to-agent talk into one chip. These
 * are the rules every client shares, so web, desktop and mobile agree.
 */

import { MESSAGE_CLASS, type MessageClass } from './messageClass';

/** The conversation meta `floor` policies (Main's `turntaking.Policy`). */
export const FLOOR_POLICY = {
  /** The addressed agents answer, in order; else the sticky target. */
  Addressed: 'addressed',
  /** The lead agent answers a message that addresses no one. */
  Lead: 'lead',
} as const;

export type FloorPolicy = (typeof FLOOR_POLICY)[keyof typeof FLOOR_POLICY];

/** A conversation's floor, read from `meta.floor`. */
export interface Floor {
  readonly policy: FloorPolicy;
  /** The lead's participant id; 0 when there is none. */
  readonly leadParticipantId: number;
}

export const DEFAULT_FLOOR: Floor = { policy: FLOOR_POLICY.Addressed, leadParticipantId: 0 };

/** The most agents one message may address (Main's `turntaking.MaxAddressed`). */
export const MAX_ADDRESSED = 16;

function positiveId(value: unknown): number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value > 0 ? value : 0;
}

/**
 * The floor of a conversation meta. An absent or malformed floor reads as the
 * default, as Main reads it for routing.
 */
export function floorOf(meta: unknown): Floor {
  if (meta === null || typeof meta !== 'object') return DEFAULT_FLOOR;
  const raw = (meta as { floor?: unknown }).floor;
  if (raw === null || typeof raw !== 'object') return DEFAULT_FLOOR;
  const { policy, lead_participant_id: lead } = raw as { policy?: unknown; lead_participant_id?: unknown };
  if (policy !== FLOOR_POLICY.Lead) return DEFAULT_FLOOR;
  const leadParticipantId = positiveId(lead);
  return leadParticipantId > 0 ? { policy: FLOOR_POLICY.Lead, leadParticipantId } : DEFAULT_FLOOR;
}

/**
 * The conversation meta with its floor set. Updating a conversation replaces
 * its whole meta, so every other key is kept. `leadParticipantId` 0 clears the
 * lead (the default floor, written as no `floor` key at all).
 */
export function metaWithLead(meta: unknown, leadParticipantId: number): Record<string, unknown> {
  const base: Record<string, unknown> = meta !== null && typeof meta === 'object' ? { ...(meta as Record<string, unknown>) } : {};
  const lead = positiveId(leadParticipantId);
  if (lead === 0) {
    delete base.floor;
    return base;
  }
  base.floor = { policy: FLOOR_POLICY.Lead, lead_participant_id: lead };
  return base;
}

/** An agent of the conversation a message can address. */
export interface AddressableAgent {
  readonly participantId: number;
  /** The name the composer shows and a mention spells. */
  readonly name: string;
}

/**
 * The `addressed_to` of a message: the agents its text mentions (`@Name` or
 * `#Name`, a whole name), in the order the text first names them,
 * deduplicated and capped. It is read from the text alone, so a message that
 * waits in a queue keeps its own addressing, and a mention deleted again
 * addresses no one. An empty list addresses no one, and the request then
 * omits the field. Of two agents whose names overlap, the longer name wins
 * where both match.
 */
export function addressedTo(text: string, agents: readonly AddressableAgent[]): number[] {
  const found: { at: number; id: number }[] = [];
  const longestFirst = [...agents].sort((a, b) => b.name.length - a.name.length);
  const taken: [number, number][] = [];
  for (const agent of longestFirst) {
    const id = positiveId(agent.participantId);
    if (id === 0 || agent.name === '' || found.some((entry) => entry.id === id)) continue;
    const at = firstMention(text, agent.name, taken);
    if (at < 0) continue;
    taken.push([at, at + 1 + agent.name.length]);
    found.push({ at, id });
  }
  return found
    .sort((a, b) => a.at - b.at)
    .map((entry) => entry.id)
    .slice(0, MAX_ADDRESSED);
}

/**
 * Who answers a message first, as Main routes it (`turntaking.Route`): the first agent it addresses, else the lead
 * under policy `lead`, else `fallback` (the sticky or composer's target; 0 for none). Only the conversation's `agents`
 * count: a lead no longer among them is no lead. The one copy of the rule the clients share (#1932). 0 when nobody
 * answers yet (the person has to pick).
 */
export function firstAnswerer(addressed: readonly number[], floor: Floor, agents: readonly AddressableAgent[], fallback = 0): number {
  const known = (id: number): boolean => id > 0 && agents.some((agent) => agent.participantId === id);
  const first = addressed[0] ?? 0;
  if (known(first)) return first;
  if (floor.policy === FLOOR_POLICY.Lead && known(floor.leadParticipantId)) return floor.leadParticipantId;
  return known(fallback) ? fallback : 0;
}

/** Where the text first mentions `name` outside the `taken` spans; -1 when it does not. */
function firstMention(text: string, name: string, taken: readonly [number, number][]): number {
  let first = -1;
  for (const sigil of ['@', '#']) {
    for (let at = text.indexOf(sigil + name); at >= 0; at = text.indexOf(sigil + name, at + 1)) {
      const before = at === 0 ? '' : (text[at - 1] ?? '');
      const after = text[at + 1 + name.length] ?? '';
      const whole = (before === '' || !isWordChar(before)) && (after === '' || !isWordChar(after));
      const inside = taken.some(([from, to]) => at >= from && at < to);
      if (whole && !inside) {
        if (first < 0 || at < first) first = at;
        break;
      }
    }
  }
  return first;
}

function isWordChar(char: string): boolean {
  return /[\p{L}\p{N}_]/u.test(char);
}

/** A transcript row as far as side-talk folding needs it. */
export interface FoldableRow {
  readonly messageClass: MessageClass;
}

/** One entry of a folded transcript: a row as is, or a run of side talk. */
export type FoldedEntry<Row extends FoldableRow> =
  | { readonly kind: 'row'; readonly row: Row }
  | { readonly kind: 'side_talk'; readonly key: string; readonly rows: readonly Row[] };

/**
 * The transcript with each run of consecutive agent-to-agent rows folded into
 * one "side conversation (n)" entry (decision 7e). Rows keep their order; the
 * key of a run is the key of its first row, so it is stable while the run
 * grows.
 */
export function foldSideTalk<Row extends FoldableRow>(rows: readonly Row[], keyOf: (row: Row) => string): FoldedEntry<Row>[] {
  const out: FoldedEntry<Row>[] = [];
  let run: Row[] = [];
  const flush = () => {
    if (run.length > 0) out.push({ kind: 'side_talk', key: `side:${keyOf(run[0] as Row)}`, rows: run });
    run = [];
  };
  for (const row of rows) {
    if (row.messageClass === MESSAGE_CLASS.AgentToAgent) {
      run.push(row);
      continue;
    }
    flush();
    out.push({ kind: 'row', row });
  }
  flush();
  return out;
}

/**
 * The compact "A → B" label of a side-talk row: the author's name and the
 * names of the agents it is addressed to.
 */
export function sideTalkLabel(author: string, addressees: readonly string[]): string {
  const to = addressees.filter((name) => name !== '');
  return to.length === 0 ? author : `${author} → ${to.join(', ')}`;
}
