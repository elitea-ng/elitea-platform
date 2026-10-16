/**
 * Recorded-stream cover for the pause frames, shaped exactly as the Rust Worker
 * emits them:
 *
 * - each card is its own `agent_hitl_interrupt` progress frame carrying
 *   `hitl_interrupt` and `hitl_interrupts: [that card]`
 *   (`services/elitea-worker-rust/src/agents/events.rs`, the sensitive-tool and
 *   `ask_user` projections);
 * - a nested pause carries `parent_agent_path`/`parent_agent_name`/
 *   `parent_agent_call_id` on the metadata AND on every interrupt
 *   (`overlay_hierarchy_value`);
 * - the run ends with ONE aggregate frame: the first card's metadata with
 *   `hitl_interrupts` = every card (`AgentPauseAccumulator::finish_hitl`), plus
 *   `authorization_requests` for a mixed pause (`finish_mixed`).
 */
import { describe, expect, it } from 'vitest';

import { normaliseAssistantMessage, type MessageGroupWire } from '@/entities/message';

import { applyChatStreamFrame, type ChatStreamContext } from './chatStreamReducer';
import { isTerminalPauseFrame } from './chatStreamInterruptFrames';
import { SocketMessageType } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '4d7f2c1e-0000-4000-8000-000000000001';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-10-16T00:00:00.000Z' };
const DIGEST = `sha256:${'a'.repeat(64)}`;

interface Tier {
  readonly name: string;
  readonly call_id: string;
  readonly sibling_ordinal?: number;
}

function hierarchy(path: readonly Tier[]): Record<string, unknown> {
  const owner = path[path.length - 1];
  return owner ? { parent_agent_path: path, parent_agent_name: owner.name, parent_agent_call_id: owner.call_id } : {};
}

function sensitivePending(interruptId: string, callId: string, path: readonly Tier[] = []): Record<string, unknown> {
  return {
    type: 'hitl',
    interrupt_id: interruptId,
    call_digest: DIGEST,
    guardrail_type: 'sensitive_tool',
    node_name: 'sensitive_tool_guard',
    message: 'Deleting a file requires approval.',
    available_actions: ['approve', 'reject', 'block_with_comment'],
    routes: {},
    tool_call_id: callId,
    tool_name: 'delete_file',
    toolkit_name: 'artifacts',
    toolkit_type: 'artifact',
    action_label: 'Delete file',
    tool_args: { path: 'report.pdf' },
    policy_message: 'Deleting a file requires approval.',
    ...hierarchy(path),
  };
}

function askUserPending(interruptId: string, callId: string, path: readonly Tier[] = []): Record<string, unknown> {
  return {
    type: 'hitl',
    interrupt_id: interruptId,
    call_digest: DIGEST,
    guardrail_type: 'clarifying_question',
    node_name: 'ask_user',
    message: 'Which environment?',
    questions: [{ id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }], multiSelect: false }],
    available_actions: ['answer'],
    routes: {},
    tool_call_id: callId,
    tool_name: 'ask_user',
    toolkit_name: 'ask_user',
    toolkit_type: 'internal',
    tool_args: { questions: [] },
    ...hierarchy(path),
  };
}

function pauseMetadata(cards: readonly Record<string, unknown>[], path: readonly Tier[] = []): Record<string, unknown> {
  const [first] = cards;
  return {
    thread_id: 'thread-root',
    chat_project_id: 7,
    message: first?.['message'],
    hitl_interrupt: first,
    hitl_interrupts: cards,
    node_name: first?.['node_name'],
    available_actions: first?.['available_actions'],
    routes: {},
    edit_state_key: null,
    ...hierarchy(path),
  };
}

function hitlFrame(metadata: Record<string, unknown>): Record<string, unknown> {
  return {
    type: SocketMessageType.AgentHitlInterrupt,
    message_id: MESSAGE_ID,
    content: metadata['message'],
    response_metadata: metadata,
  };
}

function authorizationMetadata(interruptId: string, callId: string): Record<string, unknown> {
  return {
    thread_id: 'thread-root',
    root_thread_id: 'thread-root',
    chat_project_id: 7,
    guardrail_type: 'mcp_auth',
    interrupt_id: interruptId,
    call_digest: DIGEST,
    node_name: 'delegated_authorization_guard',
    message: 'Sign in to SharePoint to continue.',
    available_actions: ['authorize', 'skip'],
    tool_call_id: callId,
    tool_name: 'sharepoint_search',
    toolkit_name: 'SharePoint',
    toolkit_type: 'sharepoint',
    tool_args: {},
    server_url: 'https://sharepoint.example.test',
    resource_metadata_url: 'https://sharepoint.example.test/.well-known/oauth-protected-resource',
    www_authenticate: 'Bearer',
    resource_metadata: {},
    authorization_servers: ['https://login.example.test'],
    resume_strategy: 'root',
  };
}

function assistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: 'assistant',
    name: 'Agent',
    content: '',
    createdAt: '2026-10-16T00:00:00.000Z',
    isStreaming: true,
    isLoading: true,
  };
}

function replay(frames: readonly Record<string, unknown>[], seed: readonly ChatMessage[] = [assistant()]): readonly ChatMessage[] {
  return frames.reduce<readonly ChatMessage[]>((history, frame) => applyChatStreamFrame(history, frame, CONTEXT), seed);
}

function cards(history: readonly ChatMessage[]): readonly Record<string, unknown>[] {
  return (history[0]?.hitlInterrupts ?? []) as readonly Record<string, unknown>[];
}

const PLANNER: Tier = { name: 'Planner', call_id: 'call-planner' };
const RESEARCHER_1: Tier = { name: 'Researcher', call_id: 'call-research-1', sibling_ordinal: 1 };
const RESEARCHER_2: Tier = { name: 'Researcher', call_id: 'call-research-2', sibling_ordinal: 2 };

describe('a nested pause: agent → saved agent → sensitive tool', () => {
  const path = [PLANNER, RESEARCHER_1];
  const card = sensitivePending('int-nested-1', 'call-delete-1', path);

  it('keeps the whole ancestry on the card', () => {
    const history = replay([hitlFrame(pauseMetadata([card], path))]);

    expect(cards(history)).toHaveLength(1);
    expect(cards(history)[0]).toMatchObject({
      interrupt_id: 'int-nested-1',
      parent_agent_name: 'Researcher',
      parent_agent_call_id: 'call-research-1',
      parent_agent_path: [
        { name: 'Planner', call_id: 'call-planner' },
        { name: 'Researcher', call_id: 'call-research-1', sibling_ordinal: 1 },
      ],
    });
  });

  it('groups a reloaded response exactly like the live one', () => {
    const second = askUserPending('int-nested-2', 'call-ask-2', [PLANNER, RESEARCHER_2]);
    const live = cards(replay([hitlFrame(pauseMetadata([card, second], path))]));
    const group = {
      id: 9,
      uuid: MESSAGE_ID,
      role: 'assistant',
      content: '',
      created_at: '2026-10-16 00:00:00',
      meta: { hitl_interrupt: card, hitl_interrupts: [card, second], thread_id: 'thread-root' },
    } as unknown as MessageGroupWire;
    const reloaded = (normaliseAssistantMessage(group, [], undefined).hitlInterrupts ?? []) as readonly Record<
      string,
      unknown
    >[];
    const fields = ['interrupt_id', 'tool_call_id', 'parent_agent_name', 'parent_agent_call_id', 'parent_agent_path', 'resume_strategy'];
    const pick = (entry: Record<string, unknown>) => Object.fromEntries(fields.map((field) => [field, entry[field]]));

    expect(reloaded.map(pick)).toEqual(live.map(pick));
  });
});

describe('two same-name instances paused in parallel', () => {
  const first = sensitivePending('int-par-1', 'call-delete-1', [RESEARCHER_1]);
  const second = sensitivePending('int-par-2', 'call-delete-2', [RESEARCHER_2]);
  const frames = [
    hitlFrame(pauseMetadata([first], [RESEARCHER_1])),
    hitlFrame(pauseMetadata([second], [RESEARCHER_2])),
    hitlFrame(pauseMetadata([first, second], [RESEARCHER_1])),
  ];

  it('keeps both cards through both progress frames and the aggregate, in order', () => {
    const history = replay(frames);

    expect(cards(history).map((entry) => entry['interrupt_id'])).toEqual(['int-par-1', 'int-par-2']);
    expect(cards(history).map((entry) => entry['parent_agent_call_id'])).toEqual(['call-research-1', 'call-research-2']);
    expect(history[0]?.hitlInterrupt).toMatchObject({ interrupt_id: 'int-par-1' });
  });

  it('keeps a queued card queued and hidden when the aggregate re-announces it', () => {
    const afterFirst = replay([frames[0]!]);
    const queued = afterFirst.map((message) => ({
      ...message,
      hitlInterrupts: cards(afterFirst).map((entry) => ({ ...entry, queued: true, hidden: true })),
    }));
    const history = replay([frames[1]!, frames[2]!], queued);

    expect(cards(history)[0]).toMatchObject({ interrupt_id: 'int-par-1', queued: true, hidden: true });
    expect(cards(history)[1]?.['queued']).toBeUndefined();
    // The singular field follows the first card still owed an answer.
    expect(history[0]?.hitlInterrupt).toMatchObject({ interrupt_id: 'int-par-2' });
  });
});

describe('a mixed pause: tool guard + ask_user + delegated authorization', () => {
  const guard = sensitivePending('int-mix-1', 'call-delete-1');
  const ask = askUserPending('int-mix-2', 'call-ask-2');
  const auth = authorizationMetadata('int-mix-3', 'call-sp-3');

  it('collects both HITL cards and the authorization card; only the aggregate is terminal', () => {
    const authProgress = { type: SocketMessageType.McpAuthorizationRequired, message_id: MESSAGE_ID, content: auth['message'], response_metadata: auth };
    const aggregate = hitlFrame({ ...auth, ...pauseMetadata([guard, ask]), authorization_requests: [auth] });
    const history = replay([hitlFrame(pauseMetadata([guard])), hitlFrame(pauseMetadata([ask])), authProgress, aggregate]);

    expect(cards(history).map((entry) => entry['interrupt_id'])).toEqual(['int-mix-1', 'int-mix-2']);
    expect(cards(history)[1]?.['questions']).toHaveLength(1);
    const actions = (history[0]?.toolActions ?? []) as unknown as readonly Record<string, unknown>[];
    expect(actions.some((action) => action['status'] === 'action_required')).toBe(true);
    expect(isTerminalPauseFrame(authProgress)).toBe(false);
    expect(isTerminalPauseFrame({ ...authProgress, response_metadata: { ...auth, authorization_requests: [auth] } })).toBe(true);
    expect(isTerminalPauseFrame(aggregate)).toBe(true);
    expect(history[0]?.isStreaming).toBe(false);
  });
});

describe('the parallel-node aggregate interrupt is reconcile-only', () => {
  const aggregateInterrupt = {
    message: 'Parallel branches paused.',
    schema: 'elitea.graph.parallel-interrupt.v1',
    parallel_node: 'fanout',
    parallel_activation: 'activation-1',
    cards: [{ branch_id: 'b-1', node: 'review', ordinal: 1, interrupt_id: 'int-member-1', tool_call_id: 'call-1' }],
  };

  it('never becomes a card', () => {
    const history = replay([hitlFrame({ message: 'Parallel branches paused.', hitl_interrupt: aggregateInterrupt, hitl_interrupts: [aggregateInterrupt] })]);
    expect(cards(history)).toHaveLength(0);
    expect(history[0]?.hitlInterrupt).toBeUndefined();
  });

  it('keeps only the member cards it names, and creates none', () => {
    const member = sensitivePending('int-member-1', 'call-1');
    const stale = sensitivePending('int-stale-9', 'call-9');
    const history = replay([
      hitlFrame(pauseMetadata([member])),
      hitlFrame(pauseMetadata([stale])),
      hitlFrame({ message: 'Parallel branches paused.', hitl_interrupts: [aggregateInterrupt] }),
    ]);
    expect(cards(history).map((entry) => entry['interrupt_id'])).toEqual(['int-member-1']);
  });

  it('is filtered out of a reloaded response too', () => {
    const member = sensitivePending('int-member-1', 'call-1');
    const group = {
      id: 9,
      uuid: MESSAGE_ID,
      role: 'assistant',
      content: '',
      created_at: '2026-10-16 00:00:00',
      meta: { hitl_interrupt: aggregateInterrupt, hitl_interrupts: [aggregateInterrupt, member] },
    } as unknown as MessageGroupWire;
    const reloaded = normaliseAssistantMessage(group, [], undefined);
    expect((reloaded.hitlInterrupts ?? []).map((entry) => (entry as Record<string, unknown>)['interrupt_id'])).toEqual(['int-member-1']);
    expect(reloaded.hitlInterrupt).toMatchObject({ interrupt_id: 'int-member-1' });
  });
});

describe('streaming state: a parked run is not streaming', () => {
  const card = sensitivePending('int-1', 'call-1');

  it('settles on the terminal pause', () => {
    const frame = hitlFrame(pauseMetadata([card]));
    expect(isTerminalPauseFrame(frame)).toBe(true);
    expect(replay([frame])[0]?.isStreaming).toBe(false);
  });

  it('stays streaming on a progress pause that says so', () => {
    const frame = hitlFrame({ ...pauseMetadata([card]), pause_progress: true });
    expect(isTerminalPauseFrame(frame)).toBe(false);
    expect(replay([frame])[0]?.isStreaming).toBe(true);
    expect(replay([frame])[0]?.isLoading).toBe(false);
  });

  it('stays streaming on a legacy fan-out child pause', () => {
    const frame = hitlFrame({ metadata: { parent_agent_name: 'planner', child_thread_id: 't-child' }, hitl_interrupt: { tool_call_id: 'c-1' } });
    expect(isTerminalPauseFrame(frame)).toBe(false);
    expect(replay([frame])[0]?.isStreaming).toBe(true);
  });
});
