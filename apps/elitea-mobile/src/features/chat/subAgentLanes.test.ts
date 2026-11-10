import { frameOf } from '@/features/chat/__fixtures__/clientFrames';
import { toChatMessages } from '@/features/chat/messageModel';
import { storedSubAgents, subAgentGroupLabel, subAgentSteps } from '@/features/chat/subAgentLanes';
import { groupByAgent } from '@/features/chat/ToolCalls';
import { INITIAL_TURN, reduceTurn } from '@/features/chat/turnReducer';

const lane = (entry: Record<string, unknown>) => frameOf({ type: 'agent_subagent', response_metadata: { subagent_v1: entry } });

const started = { kind: 'started', agent_id: 'a_1', parent_call_id: 'call-spawn', ordinal: 1, display_name: 'Owl', avatar: 'default:owl' };

describe('cloud sub-agents (client contract 1.47)', () => {
  it('folds a live child into the turn and shows its calls under "<name> · temporary"', () => {
    let turn = INITIAL_TURN;
    for (const entry of [
      started,
      { kind: 'tool_start', agent_id: 'a_1', call_id: 'k1', tool: 'grep', args_summary: 'run_agent' },
      { kind: 'tool_end', agent_id: 'a_1', call_id: 'k1', ok: true, summary: '2 matches' },
      { kind: 'tool_start', agent_id: 'a_1', call_id: 'k2', tool: 'read_file' },
    ]) {
      turn = reduceTurn(turn, lane(entry));
    }
    expect(turn.phase).toBe('streaming');
    expect(turn.content).toBe('');
    expect(turn.tools).toEqual([]);
    expect(turn.unknownFrames).toBe(0);
    const steps = subAgentSteps(turn.subAgents);
    expect(steps.map((s) => [s.name, s.status, s.parentAgent])).toEqual([
      ['grep', 'done', 'Owl · temporary'],
      ['read_file', 'running', 'Owl · temporary'],
    ]);
    expect(groupByAgent(steps).map((g) => g.agent)).toEqual(['Owl · temporary']);
    // Settled without the child's end: it was stopped with the run.
    const stopped = subAgentSteps(turn.subAgents, true);
    expect(stopped[1]).toMatchObject({ status: 'error', error: 'Stopped' });
    expect(stopped[2]).toMatchObject({ name: 'Stopped', status: 'error', parentAgent: 'Owl · temporary' });
  });

  it('ends a finished child with its final answer', () => {
    let turn = INITIAL_TURN;
    for (const entry of [started, { kind: 'finished', agent_id: 'a_1', status: 'completed', result: 'Two callers.' }]) turn = reduceTurn(turn, lane(entry));
    expect(subAgentSteps(turn.subAgents)).toEqual([
      expect.objectContaining({ id: 'a_1:report', name: 'Final answer', status: 'done', output: 'Two callers.', parentAgent: 'Owl · temporary' }),
    ]);
  });

  it('names an unnamed child by its ordinal', () => {
    expect(subAgentGroupLabel({ displayName: undefined, ordinal: 2 })).toBe('Sub-agent 2 · temporary');
  });

  it('reads a reloaded answer’s lanes from meta.subagents_v1', () => {
    const lanes = [{ agent_id: 'a_1', parent_call_id: 'p', ordinal: 1, display_name: 'Owl', status: 'running', items: [{ type: 'tool', call_id: 'k', tool: 't' }] }];
    expect(storedSubAgents({ subagents_v1: lanes }, false)[0]?.status).toBe('cancelled');
    expect(storedSubAgents({ subagents_v1: lanes }, true)[0]?.status).toBe('running');
    expect(storedSubAgents({}, false)).toEqual([]);
    expect(storedSubAgents('garbage', false)).toEqual([]);
    const [answer] = toChatMessages({
      participants: [{ id: 9, entity_name: 'application', meta: { name: 'Agent' } }] as never,
      message_groups: [{ id: 1, uuid: 'u1', author_participant_id: 9, created_at: '2026-11-10 10:00:00', meta: { subagents_v1: lanes }, message_items: [] }] as never,
    });
    expect(answer?.subAgents?.[0]?.displayName).toBe('Owl');
  });
});
