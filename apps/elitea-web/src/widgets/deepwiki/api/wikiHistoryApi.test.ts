/**
 * Reading a stored wiki chat back.
 *
 * MSW, NOT `vi.mock`, for the reason `wikiChatApi.test.ts` gives: intercepting
 * the request proves the URL, the query and the reading of what comes back,
 * where a mocked client would only prove a function was called.
 */
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { listWikiConversations, loadWikiTranscript } from './wikiHistoryApi';

const BASE = 'http://elitea.test/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});
afterEach(() => {
  resetGeneratedClient();
});

function textGroup(content: string, extra: Record<string, unknown> = {}) {
  return {
    ...extra,
    message_items: [{ item_type: 'text_message', item_details: { content } }],
  };
}

describe('listWikiConversations', () => {
  it('asks for this toolkit’s own hidden deepwiki conversations', async () => {
    let seen: URLSearchParams | undefined;
    server.use(
      http.get(`${BASE}/elitea_core/conversations/prompt_lib/:projectId`, ({ request }) => {
        seen = new URL(request.url).searchParams;
        return HttpResponse.json({ total: 0, rows: [] });
      }),
    );

    await listWikiConversations(7, 42);

    // Each of these five carries a distinct rule, and dropping any one of them
    // is silent: the list simply comes back with the wrong rows in it.
    expect(seen?.get('source')).toBe('deepwiki');
    expect(seen?.get('entity_name')).toBe('toolkit');
    expect(seen?.get('entity_meta_id')).toBe('42');
    // Without this the listing excludes every wiki chat, because they are
    // filed hidden so they do not surface in the ordinary chat list.
    expect(seen?.get('hidden')).toBe('only');
    // Without this one member reads another member's questions: the listing
    // has never read is_private.
    expect(seen?.get('mine')).toBe('true');
  });

  // `eliteaFetch` resolves the ENVELOPE. Reading `rows` off it yields
  // undefined on a 200 and renders as an empty history — issue #132's shape,
  // which has landed twice on this same endpoint family.
  it('reads the rows out of the envelope, not off it', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversations/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          total: 1,
          rows: [{ id: 11, name: 'earlier', meta: { wiki_chat_key: 'chat-1' } }],
        }),
      ),
    );

    const conversations = await listWikiConversations(7, 42);
    expect(conversations).toEqual([
      { id: '11', name: 'earlier', updatedAt: undefined, chatKey: 'chat-1' },
    ]);
  });

  it('reports a conversation with no key rather than inventing one', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversations/prompt_lib/:projectId`, () =>
        HttpResponse.json({ total: 1, rows: [{ id: 12, name: 'no key', meta: {} }] }),
      ),
    );

    const conversations = await listWikiConversations(7, 42);
    expect(conversations[0]?.chatKey).toBeUndefined();
  });
});

describe('loadWikiTranscript', () => {
  it('asks for the transcript oldest-first', async () => {
    let seen: URLSearchParams | undefined;
    server.use(
      http.get(
        `${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`,
        ({ request }) => {
          seen = new URL(request.url).searchParams;
          return HttpResponse.json({ message_groups: [] });
        },
      ),
    );

    await loadWikiTranscript(7, '11');

    // The route defaults to newest-first, and a transcript rendered that way
    // reads backwards — a failure that looks like a rendering bug.
    expect(seen?.get('sort_order')).toBe('asc');
    expect(seen?.get('messages_limit')).toBe('200');
  });

  it('reads a question and its answer apart by the reply that pairs them', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`, () =>
        HttpResponse.json({
          message_groups: [
            textGroup('Where do the pages live?', { meta: { capability: 'research' } }),
            textGroup('In wiki_pages/.', { reply_to_id: 1 }),
          ],
        }),
      ),
    );

    expect(await loadWikiTranscript(7, '11')).toEqual([
      { role: 'user', content: 'Where do the pages live?', capability: 'research' },
      { role: 'assistant', content: 'In wiki_pages/.' },
    ]);
  });

  // Absence is not a claim. Reading a missing `is_error` as false is right;
  // reading a missing one as TRUE would paint every restored answer red, and
  // reading an unknown capability as a real one would mislabel the turn.
  it('marks a failed answer, and only a failed one', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`, () =>
        HttpResponse.json({
          message_groups: [
            textGroup('q', { meta: { capability: 'telepathy' } }),
            textGroup('the clone failed', { reply_to_id: 1, meta: { is_error: true } }),
            textGroup('a fine answer', { reply_to_id: 2 }),
          ],
        }),
      ),
    );

    expect(await loadWikiTranscript(7, '11')).toEqual([
      { role: 'user', content: 'q' },
      { role: 'assistant', content: 'the clone failed', isError: true },
      { role: 'assistant', content: 'a fine answer' },
    ]);
  });

  // A group this drawer cannot render is DROPPED, not rendered as an empty
  // bubble: a wiki turn is one text item and nothing else.
  it('drops a group with nothing it can render', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`, () =>
        HttpResponse.json({
          message_groups: [
            { message_items: [{ item_type: 'attachment_message', item_details: { name: 'x.png' } }] },
            { message_items: [] },
            {},
            textGroup('the one real turn'),
          ],
        }),
      ),
    );

    expect(await loadWikiTranscript(7, '11')).toEqual([
      { role: 'user', content: 'the one real turn' },
    ]);
  });

  it('reads an empty conversation as an empty transcript', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`, () =>
        HttpResponse.json({}),
      ),
    );
    expect(await loadWikiTranscript(7, '11')).toEqual([]);
  });
});
