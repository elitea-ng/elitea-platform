import { describeNotification, mentionConversationRef } from '@/features/notifications/notificationModel';

// meta exactly as the server writes it for client contract 1.1
// (repos/chat_mention_notification.go, documented on the Notification schema).
const typedMeta = {
  conversation_id: '6F1C2B9E-0A7D-4C3E-9B1A-2D4E6F8A0B1C',
  message_id: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f',
  project_id: 7,
  sender_user_id: 9,
  sender: { id: 9, name: 'Alice Smith' },
};

// What a pre-Go deployment wrote: numeric conversation id, initiator_name, no sender.
const legacyMeta = { conversation_id: 1234, message_id: '0b7e5d3a-1c2f-4e6a-8b9d-0a1b2c3d4e5f', initiator_name: 'Bob' };

describe('describeNotification', () => {
  it('names the sender of a typed mention and links its conversation (uuid, lower-cased)', () => {
    expect(describeNotification({ event_type: 'chat_user_mentioned', meta: typedMeta })).toEqual({
      title: 'Alice Smith mentioned you',
      conversationRef: '6f1c2b9e-0a7d-4c3e-9b1a-2d4e6f8a0b1c',
    });
  });

  it('reads a legacy mention row: initiator_name and a numeric conversation id', () => {
    expect(describeNotification({ event_type: 'chat_user_mentioned', meta: legacyMeta })).toEqual({
      title: 'Bob mentioned you',
      conversationRef: '1234',
    });
  });

  it('copes with an unresolved sender and a missing or bad conversation id', () => {
    expect(describeNotification({ event_type: 'chat_user_mentioned', meta: { sender: { id: 9, name: '' } } })).toEqual({
      title: 'You were mentioned',
      conversationRef: null,
    });
    expect(mentionConversationRef({ conversation_id: '../../x' })).toBeNull();
    expect(mentionConversationRef({ conversation_id: -4 })).toBeNull();
    expect(mentionConversationRef(null)).toBeNull();
  });

  it('keeps a sender name to one bounded line', () => {
    const view = describeNotification({ event_type: 'chat_user_mentioned', meta: { sender: { id: 1, name: `A\n${'b'.repeat(200)}` } } });
    expect(view.title).toBe(`A ${'b'.repeat(78)} mentioned you`);
  });

  it('leaves other notifications as they were, without a link', () => {
    expect(describeNotification({ event_type: 'agent_finished', meta: {} })).toEqual({ title: 'Agent finished', conversationRef: null });
  });
});
