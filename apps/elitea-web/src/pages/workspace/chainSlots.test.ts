import { describe, expect, it } from 'vitest';

import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import { isContinuedRecord, turnSlots } from './chainSlots';

describe('turnSlots', () => {
  it('pairs the person’s prompts with the turns they started, and marks the turns the host continued', () => {
    const slots = turnSlots([
      { turnId: 'a', view: {} },
      { turnId: 'b', view: { continuedTo: { nextTurnId: 'c', index: 1 } } },
      // Continued: known from the turn before it, before its own status arrived.
      { turnId: 'c', view: {} },
      { turnId: 'd', view: { continuation: 2 } },
      { turnId: 'e', view: {} },
    ]);
    expect(slots).toEqual([{ prompt: 0 }, { prompt: 1 }, { continuation: 1 }, { continuation: 2 }, { prompt: 2 }]);
  });

  it('gives a later agent of a message addressed to several no prompt of its own (ADR-0050 decision 7f)', () => {
    const agent = (position: number, name: string) => ({ name, participantId: 70 + position, executor: 'local' as const, position, targets: 2, followed: false });
    const slots = turnSlots([
      { turnId: 'a', view: { agent: agent(0, 'Coder'), continuedTo: { nextTurnId: 'b', index: 1, kind: 'floor', agentName: 'Reviewer' } } },
      // Known from the turn before it, before its own status arrived.
      { turnId: 'b', view: {} },
      { turnId: 'c', view: { agent: agent(1, 'Reviewer') } },
      { turnId: 'd', view: {} },
    ]);
    expect(slots).toEqual([{ prompt: 0 }, { floor: 'Reviewer' }, { floor: 'Reviewer' }, { prompt: 1 }]);
  });
});

describe('isContinuedRecord', () => {
  it('is true for a recorded turn whose running status says the host started it', () => {
    const running = (fields: object): AgentEvent => ({ turn_id: 't', seq: 1, kind: 'status', payload: { phase: 'running', ...fields } });
    expect(isContinuedRecord([running({ continued_automatically: true, continuation_index: 1 })])).toBe(true);
    expect(isContinuedRecord([running({})])).toBe(false);
    // A later agent's recorded turn holds the floor note, not the person's message.
    expect(isContinuedRecord([running({ floor: { position: 1, targets: 2, participant_id: 88, agent_name: 'Reviewer', executor: 'cloud' } })])).toBe(true);
    expect(isContinuedRecord([running({ floor: { position: 0, targets: 2, participant_id: 77, agent_name: 'Coder', executor: 'local' } })])).toBe(false);
  });
});
