import { describe, expect, it } from 'vitest';

import { readAgentContexts } from './agentContexts';

describe('readAgentContexts', () => {
  it('keys each agent meter by its participant id', () => {
    const meters = readAgentContexts({
      agent_contexts: [
        { participant_id: 7, current_tokens: 64000, max_tokens: 128000, idle_compacted: false, active: true },
        { participant_id: 9, current_tokens: 9000, max_tokens: 200000, idle_compacted: true, active: false },
      ],
    });
    expect(meters.get(7)).toMatchObject({ percentage: 50, active: true, idleCompacted: false, tokensDisplay: '64 000 / 128 000' });
    expect(meters.get(9)).toMatchObject({ percentage: 5, idleCompacted: true, active: false });
  });

  it('leaves out what it cannot read and caps an over-budget meter', () => {
    const meters = readAgentContexts({
      agent_contexts: [
        { participant_id: 0, current_tokens: 1, max_tokens: 2 },
        { participant_id: 3, current_tokens: 1, max_tokens: 0 },
        { participant_id: 4, current_tokens: '1', max_tokens: 2 },
        { participant_id: 5, current_tokens: 300, max_tokens: 200 },
      ],
    });
    expect([...meters.keys()]).toEqual([5]);
    expect(meters.get(5)?.percentage).toBe(100);
    expect(readAgentContexts({}).size).toBe(0);
    expect(readAgentContexts(undefined).size).toBe(0);
  });
});
