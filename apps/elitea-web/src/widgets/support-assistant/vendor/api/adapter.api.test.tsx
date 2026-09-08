/**
 * `createSupportApi` — the four calls the widget makes over `eliteaFetch`.
 *
 * `eliteaFetch` resolves to the WHOLE envelope (`{data, status, headers}`),
 * not the body — the #132 defect shape this file's own doc comment warns
 * about. Every test here answers with an envelope-shaped body (`{data: …}`)
 * and asserts the ADAPTER unwrapped it, so a call site that stopped going
 * through `unwrap()` would fail here rather than ship silently.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { createSupportApi } from './adapter.api';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('createSupportApi', () => {
  it('getConversations reads the list from the envelope\'s data', async () => {
    server.use(
      http.get(`${BASE}/support_assistant/conversations/`, () =>
        HttpResponse.json({ items: [{ uuid: 'c1' }], total: 1 }),
      ),
    );

    const api = createSupportApi();
    await expect(api.getConversations()).resolves.toEqual({ items: [{ uuid: 'c1' }], total: 1 });
  });

  it('getConversation reads one conversation by id, URL-encoded', async () => {
    let capturedUrl = '';
    server.use(
      http.get(`${BASE}/support_assistant/conversation/:id`, ({ request }) => {
        capturedUrl = request.url;
        return HttpResponse.json({ uuid: 'c/1', message_groups: [] });
      }),
    );

    const api = createSupportApi();
    const conversation = await api.getConversation('c/1');

    expect(conversation).toEqual({ uuid: 'c/1', message_groups: [] });
    expect(capturedUrl).toContain('/support_assistant/conversation/c%2F1');
  });

  it('createConversation POSTs an empty JSON body and reads the envelope back', async () => {
    let capturedBody = '';
    server.use(
      http.post(`${BASE}/support_assistant/conversations/`, async ({ request }) => {
        capturedBody = await request.text();
        return HttpResponse.json({ uuid: 'new-1', name: 'New chat' });
      }),
    );

    const api = createSupportApi();
    await expect(api.createConversation()).resolves.toEqual({ uuid: 'new-1', name: 'New chat' });
    expect(capturedBody).toBe('{}');
  });

  it('startTurn POSTs the question and reads the events_url back', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/support_assistant/predict/:id`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ events_url: '/api/v2/executions/1/e1/events' });
      }),
    );

    const api = createSupportApi();
    const started = await api.startTurn('c1', { content: 'Hello', question_id: 'q1' });

    expect(started).toEqual({ events_url: '/api/v2/executions/1/e1/events' });
    expect(capturedBody).toEqual({ content: 'Hello', question_id: 'q1' });
  });

  it('startTurn carries the page context when the caller supplies one', async () => {
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/support_assistant/predict/:id`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({});
      }),
    );

    const api = createSupportApi();
    await api.startTurn('c1', {
      content: 'Hello',
      question_id: 'q1',
      support_assistant_context: { current_page: '/agents' },
    });

    expect(capturedBody).toEqual({
      content: 'Hello',
      question_id: 'q1',
      support_assistant_context: { current_page: '/agents' },
    });
  });

  // Issue #625 item 2: the attach control's upload call.
  describe('uploadAttachment', () => {
    it('POSTs a multipart file and reads the first entry back', async () => {
      let seenType = '';
      let seenBody = '';
      server.use(
        http.post(`${BASE}/support_assistant/attachments/:id`, async ({ request }) => {
          seenType = request.headers.get('content-type') ?? '';
          // The RAW body, not request.formData(): undici's multipart parser is
          // not available in this environment, and a parse that throws inside a
          // handler surfaces as a 500 rather than as a failed assertion (same
          // workaround features/skills/api/skillIconApi.test.ts uses).
          seenBody = await request.text();
          return HttpResponse.json([{ filepath: '/chat-attachments/c1/notes.txt', file_size: 5 }]);
        }),
      );

      const api = createSupportApi();
      const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
      const uploaded = await api.uploadAttachment('c1', file);

      expect(uploaded).toEqual({ filepath: '/chat-attachments/c1/notes.txt', file_size: 5 });
      expect(seenType).toMatch(/^multipart\/form-data/);
      expect(seenBody).toContain('name="file"');
    });

    it('rejects when the route answers no file (a 200 that stored nothing)', async () => {
      server.use(
        http.post(`${BASE}/support_assistant/attachments/:id`, () => HttpResponse.json([])),
      );

      const api = createSupportApi();
      const file = new File(['hello'], 'notes.txt', { type: 'text/plain' });
      await expect(api.uploadAttachment('c1', file)).rejects.toThrow('the upload answered with no file');
    });
  });
});
