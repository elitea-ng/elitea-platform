import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { TurnView } from '@/features/workspace/model/turnReducer';
import type { AgentEvent, StoredTurn } from '@/shared/desktop/workspaceIpc';

import type { TurnModel } from '@/shared/desktop/turnModel';

import { agentModel, chatModels, latestTurnModel, resolveModelName, shownModel, useModelChoice } from './useTurnModel';

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

  it('shows a latest turn’s model only when it was a kept choice, else the selected agent’s own', () => {
    const own = { name: 'agent-model', effort: 'low' as const, pinned: false };
    const kept: TurnModel = { name: 'chosen-model', reasoning_effort: 'high', chosen: true, pinned: false };
    const agents: TurnModel = { name: 'other-agents-model', chosen: false, pinned: true };
    expect(shownModel(undefined, kept, own)).toEqual({ current: 'chosen-model', effort: 'high' });
    expect(shownModel(undefined, agents, own)).toEqual({ current: 'agent-model', effort: 'low' });
    expect(shownModel(undefined, undefined, own)).toEqual({ current: 'agent-model', effort: 'low' });
    expect(shownModel({ name: 'picked' }, kept, own)).toEqual({ current: 'picked', effort: undefined });
  });

  it('matches a /model name to the catalogue’s spelling, any case, and refuses unknown names and an unloaded list', () => {
    const models = [{ name: 'GPT-5', supportsReasoning: true }];
    expect(resolveModelName(models, 'gpt-5')).toEqual({ name: 'GPT-5' });
    expect(resolveModelName(models, 'gpt')).toEqual({ notice: 'This project has no chat model named “gpt”.' });
    expect(resolveModelName(undefined, 'GPT-5')).toEqual({ notice: 'The models are still loading. Try again in a moment.' });
  });
});

describe('useModelChoice', () => {
  const models = [{ name: 'GPT-5', supportsReasoning: true }];

  it('sets a listed /model name in the catalogue’s spelling', () => {
    const { result } = renderHook(() => useModelChoice(models, null));
    act(() => result.current.command('gpt-5'));
    expect(result.current.choice).toEqual({ name: 'GPT-5' });
    expect(result.current.notice).toBeNull();
  });

  it('keeps the choice and says why when /model names no listed model', () => {
    const { result } = renderHook(() => useModelChoice(models, null));
    act(() => result.current.choose({ name: 'GPT-5', reasoning_effort: 'low' }));
    act(() => result.current.command('nope'));
    expect(result.current.choice).toEqual({ name: 'GPT-5', reasoning_effort: 'low' });
    expect(result.current.notice).toBe('This project has no chat model named “nope”.');
    act(() => result.current.dismissNotice());
    expect(result.current.notice).toBeNull();
  });

  it('refuses /model while the models are still loading', () => {
    const { result } = renderHook(() => useModelChoice(undefined, null));
    act(() => result.current.command('GPT-5'));
    expect(result.current.choice).toBeUndefined();
    expect(result.current.notice).toBe('The models are still loading. Try again in a moment.');
  });

  it('drops the choice when the host refuses a start model_unavailable, and only then', () => {
    const { result, rerender } = renderHook(({ code }: { code: string | null }) => useModelChoice(models, code), { initialProps: { code: null as string | null } });
    act(() => result.current.choose({ name: 'GPT-5' }));
    rerender({ code: 'workspace_busy' });
    expect(result.current.choice).toEqual({ name: 'GPT-5' });
    rerender({ code: null });
    rerender({ code: 'model_unavailable' });
    expect(result.current.choice).toBeUndefined();
  });
});
