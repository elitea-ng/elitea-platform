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

/** The thread's lead (`setLead`): its participant id, 0 for none. */
let LEAD = 0;

/**
 * The conversation the sends created (77), once it has a lead: its two agents and `meta.floor` (ADR-0050 decision 8:
 * a saved team's thread has a lead).
 */
function ledThread(): unknown {
  const agent = (id: number, application: number, name: string, version: number, cloud: boolean) => ({
    id,
    entity_name: 'application',
    entity_meta: { id: application, name, project_id: 42 },
    entity_settings: { version_id: version, ...(cloud ? { executor: 'cloud' } : {}) },
  });
  return {
    id: 77,
    uuid: 'c0ffee00-0000-4000-8000-000000000077',
    name: 'fix the flaky test',
    meta: { floor: { policy: 'lead', lead_participant_id: LEAD } },
    participants: [{ id: 7, entity_name: 'user', entity_meta: { id: 3 } }, agent(77, 5, 'Coder', 9, false), agent(88, 6, 'Reviewer', 10, true)],
  };
}

/** The API answers several agents in a thread need; `undefined`: the harness's own table answers. */
export function floorAnswer(method: string, path: string, body: unknown): Response | undefined {
  if (method === 'POST' && /\/elitea_core\/participants\//.test(path)) return json(addedParticipants(body));
  if (method === 'GET' && path.endsWith('/elitea_core/application/prompt_lib/42/6')) return json(REVIEWER);
  if (LEAD > 0 && method === 'GET' && path.endsWith('/elitea_core/conversation/prompt_lib/42/77')) return json(ledThread());
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

/** What `__harness` gains: the adds' bodies, `playFloor()` on the next turn id, and `setLead(id)` (the thread's lead from now on). */
export function floorHarness(emit: (event: AgentEvent) => void, turnId: () => string): { participantPosts: AddedRow[][]; playFloor: () => void; setLead: (participantId: number) => void } {
  // A new harness starts with no lead and no adds: nothing carries over from an earlier one in the same page (#1932).
  LEAD = 0;
  PARTICIPANT_POSTS.length = 0;
  return {
    participantPosts: PARTICIPANT_POSTS,
    playFloor: () => playFloor(emit, turnId()),
    setLead: (participantId) => {
      LEAD = participantId;
    },
  };
}
