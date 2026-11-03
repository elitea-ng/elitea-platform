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
});

describe('isContinuedRecord', () => {
  it('is true for a recorded turn whose running status says the host started it', () => {
    const running = (fields: object): AgentEvent => ({ turn_id: 't', seq: 1, kind: 'status', payload: { phase: 'running', ...fields } });
    expect(isContinuedRecord([running({ continued_automatically: true, continuation_index: 1 })])).toBe(true);
    expect(isContinuedRecord([running({})])).toBe(false);
  });
});
