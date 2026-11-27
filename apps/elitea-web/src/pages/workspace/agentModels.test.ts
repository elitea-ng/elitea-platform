/**
 * Each agent's model in a desktop thread (#3145): the host keeps a choice per agent session; a turn without an agent
 * (one agent, or a thread from before) is the lead's, else the single agent's; choosing the agent's own model clears
 * the badge; every send carries every agent's choice.
 */
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { TurnModel } from '@/shared/desktop/turnModel';

import { agentModelsOf, keptModels, turnModelRecords, useAgentModelStore } from './agentModels';
import type { ThreadAgent } from './useEnsureConversation';

const CODER: ThreadAgent = { participantId: 11, applicationId: 5, name: 'Coder', versionId: 9, cloud: false };
const REVIEWER: ThreadAgent = { participantId: 12, applicationId: 6, name: 'Reviewer', versionId: 10, cloud: false };
const chosen = (name: string, extra: Partial<TurnModel> = {}): TurnModel => ({ name, chosen: true, pinned: false, ...extra });
const own = (name: string): TurnModel => ({ name, chosen: false, pinned: false });

describe('keptModels', () => {
  it('keeps each agent’s latest choice, and an own model clears it', () => {
    const kept = keptModels(
      [
        { participantId: 11, model: chosen('gpt-5', { reasoning_effort: 'none' }) },
        { participantId: 12, model: chosen('mini') },
        { participantId: 12, model: own('agent-model') },
        { participantId: 99, model: chosen('stranger') },
        { participantId: 11 },
      ],
      [CODER, REVIEWER],
      6,
    );
    expect([...kept]).toEqual([[5, { name: 'gpt-5', reasoning_effort: 'off' }]]);
  });

  it('reads a turn without an agent (a thread-wide choice from before) as the lead’s, or the single agent’s', () => {
    expect([...keptModels([{ model: chosen('gpt-5') }], [CODER, REVIEWER], 6)]).toEqual([[6, { name: 'gpt-5' }]]);
    expect([...keptModels([{ model: chosen('gpt-5') }], [CODER], 5)]).toEqual([[5, { name: 'gpt-5' }]]);
    expect([...keptModels([{ model: chosen('gpt-5') }], [], 0)]).toEqual([]);
  });

  it('takes the recorded turns not shown live, then this visit’s, oldest first', () => {
    const view = (participantId: number, model: TurnModel) => ({ phase: null, items: [], approvals: [], agent: { name: '', participantId, executor: 'local' as const, position: 0, targets: 1 }, model });
    const records = turnModelRecords(
      { turnId: 't3', view: view(12, chosen('now')), earlier: [{ turnId: 't2', view: view(11, chosen('earlier')) }] },
      [{ turnId: 't1', participantId: 11, model: chosen('old') }, { turnId: 't2', participantId: 11, model: chosen('stale copy') }],
    );
    expect(records.map((record) => record.model?.name)).toEqual(['old', 'earlier', 'now']);
  });
});

describe('agentModelsOf', () => {
  it('shows a choice made here over the kept one, none for the agent’s own model, and sends every choice', () => {
    const { result } = renderHook(() => useAgentModelStore());
    const kept = new Map([[6, { name: 'mini' }]]);
    act(() => agentModelsOf(result.current, kept).choose(5, { name: 'gpt-5', reasoning_effort: 'high' }, 'agent-model'));
    let models = agentModelsOf(result.current, kept);
    expect(models.overrideOf(5)).toEqual({ name: 'gpt-5', reasoning_effort: 'high' });
    expect(models.overrideOf(6)).toEqual({ name: 'mini' });
    expect(models.entries()).toEqual([
      { application_id: 6, name: 'mini' },
      { application_id: 5, name: 'gpt-5', reasoning_effort: 'high' },
    ]);

    // Back to the agent's own model: no badge, and the host is told so (it then keeps no choice).
    act(() => models.choose(6, { name: 'agent-6-model' }, 'agent-6-model'));
    models = agentModelsOf(result.current, kept);
    expect(models.overrideOf(6)).toBeUndefined();
    expect(models.entries()).toContainEqual({ application_id: 6, name: 'agent-6-model' });

    // A start refused `model_unavailable` drops what was chosen here; the kept choices stay the host's.
    act(() => models.dropPending());
    models = agentModelsOf(result.current, kept);
    expect(models.overrideOf(5)).toBeUndefined();
    expect(models.entries()).toEqual([{ application_id: 6, name: 'mini' }]);
  });

  it('says which agent’s model menu to open, once', () => {
    const { result } = renderHook(() => useAgentModelStore());
    act(() => agentModelsOf(result.current, new Map()).edit(5));
    expect(agentModelsOf(result.current, new Map()).editing).toBe(5);
    act(() => agentModelsOf(result.current, new Map()).edit(null));
    expect(agentModelsOf(result.current, new Map()).editing).toBeNull();
  });
});
