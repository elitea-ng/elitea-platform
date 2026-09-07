/**
 * `parseConversationMessages` and `generateUUID` — the two pure conversions
 * this widget makes over the raw stored transcript.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';

import { generateUUID, parseConversationMessages } from './conversation.utils';
import type { TRawConversation } from '../types';

describe('parseConversationMessages', () => {
  it('reads a USER message from "sent_to" being set', () => {
    const conversation: TRawConversation = {
      message_groups: [
        { uuid: 'g1', sent_to: 42, message_items: [{ item_type: 'text_message', content: 'Question' }] },
      ],
    };
    expect(parseConversationMessages(conversation)).toEqual([
      expect.objectContaining({ role: 'user', content: 'Question', id: 'g1' }),
    ]);
  });

  /*
   * THE KEY THIS SERVICE ACTUALLY SENDS.
   *
   * `repos.ConversationsRepo.ListMessageGroups` writes `sent_to_id` and never
   * `sent_to`, so the reader saw no addressee and made every replayed message
   * an assistant message. A reopened conversation showed the user their own
   * questions in the answer bubble.
   */
  it('reads a USER message from the "sent_to_id" this service sends', () => {
    const conversation: TRawConversation = {
      message_groups: [
        { uuid: 'g1', sent_to_id: 3, message_items: [{ item_type: 'text_message', content: 'Question' }] },
      ],
    };
    expect(parseConversationMessages(conversation)[0]?.role).toBe('user');
  });

  it('reads an ASSISTANT message when "sent_to" is absent', () => {
    const conversation: TRawConversation = {
      message_groups: [{ uuid: 'g1', message_items: [{ item_type: 'text_message', content: 'Answer' }] }],
    };
    expect(parseConversationMessages(conversation)[0]?.role).toBe('assistant');
  });

  it('reads content from item_details.content, falling back to content', () => {
    const conversation: TRawConversation = {
      message_groups: [
        { uuid: 'g1', message_items: [{ item_type: 'text_message', item_details: { content: 'Nested' } }] },
      ],
    };
    expect(parseConversationMessages(conversation)[0]?.content).toBe('Nested');
  });

  it('accepts the legacy "text" item type too', () => {
    const conversation: TRawConversation = {
      message_groups: [{ uuid: 'g1', message_items: [{ type: 'text', content: 'Legacy shape' }] }],
    };
    expect(parseConversationMessages(conversation)[0]?.content).toBe('Legacy shape');
  });

  it('reads an EMPTY string when a group carries no text item', () => {
    const conversation: TRawConversation = {
      message_groups: [{ uuid: 'g1', message_items: [{ item_type: 'tool_call', content: 'ignored' }] }],
    };
    expect(parseConversationMessages(conversation)[0]?.content).toBe('');
  });

  it('falls back to "id" and then to an empty string when "uuid" is absent', () => {
    const withId: TRawConversation = { message_groups: [{ id: 'fallback-id', message_items: [] }] };
    expect(parseConversationMessages(withId)[0]?.id).toBe('fallback-id');

    const withNeither: TRawConversation = { message_groups: [{ message_items: [] }] };
    expect(parseConversationMessages(withNeither)[0]?.id).toBe('');
  });

  it('reads no messages at all when there are no groups', () => {
    expect(parseConversationMessages({})).toEqual([]);
  });

  describe('timestamps', () => {
    it('reads a SECONDS epoch and scales it to milliseconds', () => {
      const conversation: TRawConversation = {
        message_groups: [{ uuid: 'g1', created_at_ts: 1_700_000_000, message_items: [] }],
      };
      expect(parseConversationMessages(conversation)[0]?.timestamp).toBe(1_700_000_000_000);
    });

    it('leaves a MILLISECONDS epoch unscaled', () => {
      const conversation: TRawConversation = {
        message_groups: [{ uuid: 'g1', created_at_ts: 1_700_000_000_000, message_items: [] }],
      };
      expect(parseConversationMessages(conversation)[0]?.timestamp).toBe(1_700_000_000_000);
    });

    it('parses an ISO "created_at" string', () => {
      const conversation: TRawConversation = {
        message_groups: [{ uuid: 'g1', created_at: '2026-01-01T00:00:00Z', message_items: [] }],
      };
      expect(parseConversationMessages(conversation)[0]?.timestamp).toBe(
        new Date('2026-01-01T00:00:00Z').getTime(),
      );
    });

    it('reads ZERO for a date string that does not parse', () => {
      const conversation: TRawConversation = {
        message_groups: [{ uuid: 'g1', created_at: 'not-a-date', message_items: [] }],
      };
      expect(parseConversationMessages(conversation)[0]?.timestamp).toBe(0);
    });

    it('reads ZERO when neither field is present', () => {
      const conversation: TRawConversation = { message_groups: [{ uuid: 'g1', message_items: [] }] };
      expect(parseConversationMessages(conversation)[0]?.timestamp).toBe(0);
    });
  });
});

describe('generateUUID', () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('uses crypto.randomUUID when the runtime has it', () => {
    const spy = vi.spyOn(crypto, 'randomUUID').mockReturnValue('11111111-1111-4111-8111-111111111111');
    expect(generateUUID()).toBe('11111111-1111-4111-8111-111111111111');
    spy.mockRestore();
  });

  it('falls back to a hand-rolled v4-shaped id without crypto.randomUUID', () => {
    const original = crypto.randomUUID.bind(crypto);
    // Simulates an older runtime that lacks the method.
    delete (crypto as { randomUUID?: unknown }).randomUUID;

    const id = generateUUID();
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);

    (crypto as { randomUUID: typeof original }).randomUUID = original;
  });
});
