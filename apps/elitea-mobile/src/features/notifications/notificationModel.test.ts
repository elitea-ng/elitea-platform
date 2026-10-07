import { notificationsPage } from '@/api/__fixtures__/settings13';
import {
  MAX_FILTER_TYPES,
  applyFilter,
  describeNotification,
  filterOptions,
  mentionConversationRef,
} from '@/features/notifications/notificationModel';

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

describe('the type filter (client contract 1.3 inbox)', () => {
  const rows = notificationsPage.rows;

  it('offers All plus one chip per type, labelled for people', () => {
    expect(filterOptions(rows)).toEqual([
      { value: 'all', label: 'All', count: 3 },
      { value: 'agent_finished', label: 'Agent finished', count: 1 },
      { value: 'chat_user_mentioned', label: 'Mentions', count: 1 },
      { value: 'moderation_approved', label: 'Moderation approved', count: 1 },
    ]);
  });

  it('offers no chips when every row has the same type', () => {
    expect(filterOptions([rows[0], rows[0]])).toEqual([]);
    expect(filterOptions([])).toEqual([]);
  });

  it('ranks the most frequent types first and keeps at most MAX_FILTER_TYPES', () => {
    const many = ['a', 'b', 'b', 'c', 'd', 'e', 'f', 'g', 'g', 'g'].map((event_type) => ({ event_type }));
    const options = filterOptions(many);
    expect(options).toHaveLength(MAX_FILTER_TYPES + 1);
    expect(options.slice(1, 3).map((o) => o.value)).toEqual(['g', 'b']);
  });

  it('filters by type; All keeps everything', () => {
    expect(applyFilter(rows, 'chat_user_mentioned').map((r) => r.id)).toEqual([9]);
    expect(applyFilter(rows, 'all')).toHaveLength(3);
  });
});
