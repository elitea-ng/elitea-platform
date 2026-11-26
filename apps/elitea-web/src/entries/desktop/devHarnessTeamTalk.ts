/**
 * DEV ONLY — the dev harness's (`devHarness.tsx`) side of agents talking to each other (ADR-0061 decisions 3d and 3e,
 * #3064 #3065), read by e2e/desktop/desktop.team-talk.spec.ts, and Settings › Trusted folders (#3062).
 *
 * `playHandOff(stage)` plays, as the host would, a message to a team thread's lead (Reviewer, 88) that hands the floor
 * to Coder (77), and Coder consulting Reviewer, with each agent's `agent_state`:
 *  - `handing`: Reviewer's turn hands off (`floor_handed`; its last state carries `handoff_to`);
 *  - `handed`: Reviewer's turn is done and the host starts Coder's turn (`turn_continued` kind `hand`, with its `hand_id`);
 *  - `consulting`: Coder asks Reviewer (`consult_started`); Coder waits on Reviewer, Reviewer works;
 *  - `done`: Reviewer replies (`consult_finished`), Coder answers and the turn is done.
 * The stages are separate calls so each `agent_state` renders before the next one arrives.
 */
import type { TrustEntry, TrustIpc, TrustStatus } from '@/shared/desktop/trustIpc';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

type Stage = 'handing' | 'handed' | 'consulting' | 'done';

const HAND_ID = '6f1c2a10-0000-4000-8000-000000003064';

function handing(emit: (event: AgentEvent) => void, first: string): void {
  const a = (seq: number) => ({ turn_id: first, seq });
  emit({ ...a(1), kind: 'status', payload: { phase: 'running', floor: { position: 0, targets: 1, participant_id: 88, agent_name: 'Reviewer', executor: 'local', exchange: [] } } });
  emit({ ...a(2), kind: 'text_delta', payload: { text: 'Coder knows the lint setup better: handing this over.' } });
  emit({ ...a(3), kind: 'floor_handed', payload: { from: 88, from_name: 'Reviewer', to: [{ participant_id: 77, name: 'Coder' }], message_excerpt: 'Can you take lint-web? The failure is in the import order.' } });
  emit({ ...a(4), kind: 'agent_state', payload: { participant_id: 88, state: 'success', activity: { handoff_to: [77] } } });
}

function handed(emit: (event: AgentEvent) => void, first: string, next: string): void {
  const a = (seq: number) => ({ turn_id: first, seq });
  const settled = { committed: true, conversation_id: '77', message_ids: [], changed_files: 0 };
  emit({ ...a(5), kind: 'done', payload: { ...settled, continuing: true, floor_next: { participant_id: 77, agent_name: 'Coder', hand_id: HAND_ID } } });
  emit({ ...a(6), kind: 'status', payload: { phase: 'done' } });
  emit({ ...a(7), kind: 'turn_continued', payload: { next_turn_id: next, execution_id: 'exec-hand', continuation_index: 1, kind: 'hand', participant_id: 77, agent_name: 'Coder', executor: 'local', hand_id: HAND_ID } });
  emit({ turn_id: next, seq: 1, kind: 'status', payload: { phase: 'running', floor: { position: 1, targets: 2, participant_id: 77, agent_name: 'Coder', executor: 'local', exchange: [] } } });
}

function consulting(emit: (event: AgentEvent) => void, next: string): void {
  const b = (seq: number) => ({ turn_id: next, seq });
  emit({ ...b(2), kind: 'consult_started', payload: { caller: 77, callee: 88, callee_name: 'Reviewer', question_excerpt: 'Is the import-order rule on for tests too?', local: true } });
  emit({ ...b(3), kind: 'agent_state', payload: { participant_id: 77, state: 'waiting', activity: { waiting_on: 'agent', peer_participant_id: 88 } } });
}

function done(emit: (event: AgentEvent) => void, next: string): void {
  const b = (seq: number) => ({ turn_id: next, seq });
  emit({ ...b(4), kind: 'consult_finished', payload: { caller: 77, callee: 88, callee_name: 'Reviewer', reply_excerpt: 'Yes, tests included.', ok: true, local: true } });
  emit({ ...b(5), kind: 'agent_state', payload: { participant_id: 77, state: 'working', activity: { tool: 'run_command', tool_kind: 'code_execution', program: 'npm' } } });
  emit({ ...b(6), kind: 'text_delta', payload: { text: 'Fixed the import order in lint-web; the job is green.' } });
  emit({ ...b(7), kind: 'done', payload: { committed: true, conversation_id: '77', message_ids: [], changed_files: 1 } });
  emit({ ...b(8), kind: 'status', payload: { phase: 'done' } });
}

/** `__harness.playHandOff(stage)`: the stages above, on the turn the last send started. */
export function teamTalkHarness(emit: (event: AgentEvent) => void, turnId: () => string): { playHandOff: (stage: Stage) => void } {
  let turn = '';
  return {
    playHandOff: (stage) => {
      if (stage === 'handing') {
        turn = turnId();
        handing(emit, turn);
        return;
      }
      const next = `${turn}-hand`;
      if (stage === 'handed') handed(emit, turn, next);
      else if (stage === 'consulting') consulting(emit, next);
      else done(emit, next);
    },
  };
}

const TRUSTED_AT = '2026-11-20T09:00:00Z';

/** Settings › Trusted folders: elitea-platform trusted, docs paused by the policy; `revoked` lists each Revoke. */
export function harnessTrustIpc(): TrustIpc & { revoked: string[] } {
  const base: Omit<TrustStatus, 'workspace_id' | 'state'> = {
    suspended_by: null,
    tampered_file: null,
    allowed_by_policy: true,
    policy_blocks: [],
    inherited_from: null,
    grantable: true,
    ask_every_command: false,
    env_fallback: false,
    platform: 'macos',
    sandbox_available: true,
    granted_at: TRUSTED_AT,
    last_host_command_at: TRUSTED_AT,
    confirmation_version: 1,
  };
  let entries: TrustEntry[] = [
    { ...base, workspace_id: 'w1', state: 'trusted', name: 'elitea-platform', path: '/Users/me/code/elitea-platform' },
    { ...base, workspace_id: 'w2', state: 'suspended', suspended_by: 'policy', name: 'docs', path: '/Users/me/notes/docs' },
  ];
  const revoked: string[] = [];
  const statusOf = (id: string): TrustStatus => entries.find((entry) => entry.workspace_id === id) ?? { ...base, workspace_id: id, state: 'untrusted', granted_at: null, last_host_command_at: null };
  return {
    revoked,
    status: (id) => Promise.resolve(statusOf(id)),
    grant: (id) => Promise.resolve(statusOf(id)),
    revoke: (id) => {
      revoked.push(id);
      entries = entries.filter((entry) => entry.workspace_id !== id);
      return Promise.resolve(statusOf(id));
    },
    list: () => Promise.resolve(entries),
    onEvent: () => Promise.resolve(() => undefined),
  };
}
