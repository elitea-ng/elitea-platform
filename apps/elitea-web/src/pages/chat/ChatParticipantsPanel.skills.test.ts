import { describe, expect, it } from 'vitest';

import { activeConversationId, personNameOf } from './ChatParticipantsPanel';

/** The Skills section's two lookups on the active conversation (ADR-0050 amendment 1, A1.3). */
describe('the Skills section of the conversation panel', () => {
  it('addresses the conversation by its uuid, else its numeric id, else not at all', () => {
    expect(activeConversationId({ id: 12, uuid: 'a9fbf3d2-0000-4000-8000-000000000001' })).toBe('a9fbf3d2-0000-4000-8000-000000000001');
    expect(activeConversationId({ id: 12 })).toBe('12');
    expect(activeConversationId({ id: '12', uuid: '' })).toBe('12');
    expect(activeConversationId({})).toBeUndefined();
  });

  it('names the people of the conversation by user id, and no one else', () => {
    const names = personNameOf({
      participants: [
        { entity_name: 'user', entity_meta: { id: 9, name: 'Alex' } },
        { entity_name: 'user', entity_meta: { id: '4', name: 'Dana' } },
        // An agent is not a person: its id must not answer a user id.
        { entity_name: 'application', entity_meta: { id: 5, name: 'Release bot' } },
        { entity_name: 'user', entity_meta: { id: 6 } },
      ],
    });
    expect(names(9)).toBe('Alex');
    expect(names(4)).toBe('Dana');
    expect(names(5)).toBeUndefined();
    expect(names(6)).toBeUndefined();
    expect(names(99)).toBeUndefined();
    expect(personNameOf(undefined)(1)).toBeUndefined();
  });
});
