import { HANDOFF_MAX_AGE_MS, clearHandoffs, handOff, markAgentStarted, markThenDone, readHandoff } from '@/features/intents/intentHandoff';

let mockUuid = 0;
jest.mock('expo-crypto', () => ({ randomUUID: () => `00000000-0000-4000-8000-${String(++mockUuid).padStart(12, '0')}` }));

const HANDOFF = { agent: { projectId: 7, agentId: 3 }, prefill: 'Summarise the week' };

it('only an id the app minted reads back; anything a URL can carry reads nothing', () => {
  const id = handOff(HANDOFF, 1000);
  expect(readHandoff(id, 1000)).toEqual(HANDOFF);
  expect(readHandoff('00000000-0000-4000-8000-999999999999', 1000)).toBeNull();
  expect(readHandoff(undefined, 1000)).toBeNull();
  expect(readHandoff(['x'], 1000)).toBeNull();
});

it('expires, starts its agent once, and is gone after a wipe', () => {
  const id = handOff(HANDOFF, 1000);
  markAgentStarted(id);
  expect(readHandoff(id, 1000)).toEqual({ agent: null, prefill: 'Summarise the week' });
  expect(readHandoff(id, 1000 + HANDOFF_MAX_AGE_MS + 1)).toBeNull();
  const other = handOff(HANDOFF, 2000);
  clearHandoffs();
  expect(readHandoff(other, 2000)).toBeNull();
});

it('carries a Home action ("then") once, and a URL cannot ask for one', () => {
  const id = handOff({ agent: null, prefill: null, then: 'dictate' }, 1000);
  expect(readHandoff(id, 1000)).toEqual({ agent: null, prefill: null, then: 'dictate' });
  markThenDone(id);
  expect(readHandoff(id, 1000)).toEqual({ agent: null, prefill: null });
  expect(readHandoff('dictate', 1000)).toBeNull();
});
