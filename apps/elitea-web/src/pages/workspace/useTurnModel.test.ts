import { describe, expect, it } from 'vitest';

import type { TurnView } from '@/features/workspace/model/turnReducer';
import type { AgentEvent, StoredTurn } from '@/shared/desktop/workspaceIpc';

import { agentModel, chatModels, latestTurnModel } from './useTurnModel';

const EMPTY: TurnView = { phase: null, items: [], approvals: [] };
const running = (seq: number, name: string, chosen = true): AgentEvent => ({
  turn_id: 't',
  seq,
  kind: 'status',
  payload: { phase: 'running', model: { name, chosen, pinned: false } },
});
const stored = (events: AgentEvent[]): StoredTurn => ({
  turn_id: 't',
  conversation_id: '77',
  conversation_uuid: null,
  prompt: 'p',
  mentions: [],
  started_at: 0,
  finished_at: 1,
  events,
  changes: null,
  events_truncated: false,
  state: 'done',
  live: false,
});

describe('useTurnModel helpers', () => {
  it('keeps the catalogue’s chat models only, the first of each name', () => {
    const body = {
      items: [
        { name: 'gpt-5', project_id: 42, supports_reasoning: true, description: 'Thinks' },
        { name: 'embed', project_id: 42, type: 'embedding_model' },
        { name: 'gpt-5', project_id: 1, supports_reasoning: false },
        { name: 'mini', project_id: 1, type: 'llm_model' },
        { name: '' },
      ],
    };
    expect(chatModels(body)).toEqual([
      { name: 'gpt-5', description: 'Thinks', supportsReasoning: true },
      { name: 'mini', description: undefined, supportsReasoning: false },
    ]);
    expect(chatModels(undefined)).toEqual([]);
    expect(chatModels({ data: { items: [{ name: 'x' }] }, status: 200, headers: new Headers() })).toEqual([{ name: 'x', description: undefined, supportsReasoning: false }]);
  });

  it('reads the agent version’s own model, effort and pin', () => {
    expect(agentModel({ llm_settings: { model_name: 'gpt-4o', reasoning_effort: 'low', pin_model: true } })).toEqual({ name: 'gpt-4o', effort: 'low', pinned: true });
    expect(agentModel({ llm_settings: { model_name: '', reasoning_effort: 'extreme' } })).toEqual({ name: null, effort: undefined, pinned: false });
    expect(agentModel(undefined)).toEqual({ name: null, effort: undefined, pinned: false });
  });

  it('takes the latest turn’s model: this session’s first, else the newest recorded one', () => {
    const recorded = { kind: 'local' as const, turns: [stored([running(1, 'old')]), stored([running(1, 'newer'), running(5, 'newest')]), stored([])] };
    expect(latestTurnModel({ view: EMPTY, earlier: [] }, recorded)?.name).toBe('newest');
    expect(latestTurnModel({ view: EMPTY, earlier: [{ turnId: 'a', view: { ...EMPTY, model: { name: 'live', chosen: false, pinned: false } } }] }, recorded)?.name).toBe('live');
    expect(latestTurnModel({ view: EMPTY, earlier: [] }, { kind: 'none' })).toBeUndefined();
  });
});
