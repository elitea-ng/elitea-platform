import { addressWithChosen, canAddressAgents } from '@/features/chat/peoplePick';

const people = { userIds: [42], everyone: false };

describe('addressWithChosen', () => {
  it('a chip-picked agent keeps the turn when the message mentions people', () => {
    expect(addressWithChosen([], people, 3, [3, 4])).toEqual([3]);
    expect(addressWithChosen([], { userIds: [], everyone: true }, 3, [3, 4])).toEqual([3]);
  });

  it('adds nothing without a deliberate pick (the last answerer is not an address)', () => {
    expect(addressWithChosen([], people, null, [3, 4])).toEqual([]);
  });

  it('a pick that is not an agent of this conversation is not an address (Main would 422)', () => {
    expect(addressWithChosen([], people, 9, [3, 4])).toEqual([]);
  });

  it('an agent named in the message wins, and a message with no mention is untouched', () => {
    expect(addressWithChosen([4], people, 3, [3, 4])).toEqual([4]);
    expect(addressWithChosen([], null, 3, [3, 4])).toEqual([]);
    expect(addressWithChosen([], { userIds: [], everyone: false }, 3, [3, 4])).toEqual([]);
  });
});

describe('canAddressAgents', () => {
  it('addresses the one agent of a mixed conversation, not of a one-person chat', () => {
    expect(canAddressAgents(1, 2)).toBe(true);
    expect(canAddressAgents(1, 0)).toBe(false);
    expect(canAddressAgents(2, 0)).toBe(true);
    expect(canAddressAgents(0, 3)).toBe(false);
  });
});
