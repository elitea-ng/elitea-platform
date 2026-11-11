/**
 * DEV ONLY — the dev harness's (`devHarness.tsx`) side of several agents in
 * one thread (ADR-0050 decision 7f): the second agent (Reviewer) and the
 * participant ids a send's add gets (`floorAnswer`), and on `__harness`
 * (`floorHarness`) the bodies the adds sent (`participantPosts`, read by
 * e2e/desktop/desktop.multi-agent.spec.ts) and `playFloor()`, a message
 * addressed to Coder (local) and Reviewer (cloud) as the host plays it.
 */
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

/** The participant id each added entity gets: the user 7, Coder (application 5) 77, Reviewer (application 6) 88. */
const PARTICIPANT_IDS: Record<string, number> = { user: 7, 'application:5': 77, 'application:6': 88 };

interface AddedRow {
  entity_name: string;
  entity_meta?: { id?: number };
  entity_settings?: Record<string, unknown>;
}

/** Every `POST participants` body, as sent. */
const PARTICIPANT_POSTS: AddedRow[][] = [];

const REVIEWER = { id: '6', name: 'Reviewer', description: '', versions: [{ id: '10', name: 'base', status: 'published', agent_type: 'openai' }] };

const json = (body: unknown): Response => new Response(JSON.stringify(body), { status: 200, headers: { 'content-type': 'application/json' } });

/** The API answers several agents in a thread need; `undefined`: the harness's own table answers. */
export function floorAnswer(method: string, path: string, body: unknown): Response | undefined {
  if (method === 'POST' && /\/elitea_core\/participants\//.test(path)) return json(addedParticipants(body));
  if (method === 'GET' && path.endsWith('/elitea_core/application/prompt_lib/42/6')) return json(REVIEWER);
  return undefined;
}

/** `POST participants`' answer: the added participants, in request order, with the ids a thread's `addressed_to` uses. */
function addedParticipants(body: unknown): unknown[] {
  if (typeof body !== 'string') return [];
  try {
    const rows = JSON.parse(body) as AddedRow[];
    PARTICIPANT_POSTS.push(rows);
    return rows.map((row, index) => {
      const key = row.entity_name === 'user' ? 'user' : `${row.entity_name}:${String(row.entity_meta?.id ?? '')}`;
      return { id: PARTICIPANT_IDS[key] ?? 100 + index, entity_name: row.entity_name, entity_meta: row.entity_meta ?? {}, entity_settings: row.entity_settings ?? {} };
    });
  } catch {
    return [];
  }
}

/**
 * A message addressed to Coder (local) and Reviewer (cloud), as the host plays it: Coder's turn on this computer, its
 * `done` naming the next agent, then the host starting Reviewer's turn in the cloud (`turn_continued` kind `floor`).
 */
function playFloor(emit: (event: AgentEvent) => void, first: string): void {
  const next = `${first}-floor`;
  const on = (id: string) => (seq: number) => ({ turn_id: id, seq });
  const a = on(first);
  const b = on(next);
  const floor = (position: number, name: string, executor: 'local' | 'cloud') => ({ position, targets: 2, participant_id: position === 0 ? 77 : 88, agent_name: name, executor });
  const settled = { committed: false, conversation_id: '77', message_ids: [], changed_files: 0 };
  emit({ ...a(1), kind: 'status', payload: { phase: 'running', project_instructions: ['AGENTS.md'], floor: floor(0, 'Coder', 'local') } });
  emit({ ...a(2), kind: 'text_delta', payload: { text: 'I fixed the flaky test in src/main.rs.' } });
  emit({ ...a(3), kind: 'done', payload: { ...settled, continuing: true, floor_next: { participant_id: 88, agent_name: 'Reviewer', position: 1 } } });
  emit({ ...a(4), kind: 'status', payload: { phase: 'done' } });
  emit({ ...a(5), kind: 'turn_continued', payload: { next_turn_id: next, execution_id: 'exec-floor', continuation_index: 1, kind: 'floor', participant_id: 88, agent_name: 'Reviewer', executor: 'cloud' } });
  emit({ ...b(1), kind: 'status', payload: { phase: 'running', floor: floor(1, 'Reviewer', 'cloud') } });
  emit({ ...b(2), kind: 'text_delta', payload: { text: 'Reviewed from the conversation: the fix looks right.' } });
  emit({ ...b(3), kind: 'done', payload: settled });
  emit({ ...b(4), kind: 'status', payload: { phase: 'done' } });
}

const AGENT_NAMES: Record<number, { name: string; executor: 'local' | 'cloud' }> = { 77: { name: 'Coder', executor: 'local' }, 88: { name: 'Reviewer', executor: 'cloud' } };

/**
 * A message of a thread whose lead is `lead` (`meta.floor.policy: lead`), as Main routes it and the host follows it
 * (ADR-0050 decision 7c, #1917): `addressed` (the start's `addressed_to`) answers on the first agent it names; an
 * unaddressed message goes to the lead, which the host starts in place of the composer's agent (`floor.followed`).
 */
function playRouted(emit: (event: AgentEvent) => void, turn: string, addressed: readonly number[], composer: number, lead: number): void {
  const participant = addressed[0] ?? lead;
  const agent = AGENT_NAMES[participant] ?? { name: 'Agent', executor: 'local' as const };
  const followed = addressed.length === 0 && participant !== composer;
  const at = (seq: number) => ({ turn_id: turn, seq });
  const floor = { position: 0, targets: Math.max(1, addressed.length), participant_id: participant, agent_name: agent.name, executor: agent.executor, ...(followed ? { followed: true as const } : {}) };
  emit({ ...at(1), kind: 'status', payload: { phase: 'running', floor } });
  emit({ ...at(2), kind: 'text_delta', payload: { text: `${agent.name} here: on it.` } });
  emit({ ...at(3), kind: 'done', payload: { committed: false, conversation_id: '77', message_ids: [], changed_files: 0 } });
  emit({ ...at(4), kind: 'status', payload: { phase: 'done' } });
}

/** The participant id of the composer's agent of a start (Coder 77, Reviewer 88). */
const composerOf = (applicationId: unknown): number => (applicationId === 6 ? 88 : 77);

/**
 * What `__harness` gains: the adds' bodies, `playFloor()` on the next turn id, and `playRouted(lead)` (the last start,
 * routed in a thread whose lead is the participant `lead`).
 */
export function floorHarness(
  emit: (event: AgentEvent) => void,
  turnId: () => string,
  lastStart: () => { addressed_to?: number[] | undefined; application_id: number } | undefined,
): { participantPosts: AddedRow[][]; playFloor: () => void; playRouted: (lead: number) => void } {
  return {
    participantPosts: PARTICIPANT_POSTS,
    playFloor: () => playFloor(emit, turnId()),
    playRouted: (lead) => {
      const start = lastStart();
      playRouted(emit, turnId(), start?.addressed_to ?? [], composerOf(start?.application_id), lead);
    },
  };
}
