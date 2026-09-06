/**
 * The drawer, driven through the real network boundary.
 *
 * The slice below it is fully tested in isolation. What only exists HERE is the
 * composition: does a question typed into the box reach the provider, does a
 * polled event become a card, and does the answer render as markdown. Every
 * one of those crosses a boundary that a unit test on either side cannot see —
 * the class of defect this repository keeps meeting, where both halves are
 * correct and the wiring is the bug (#597).
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ComponentProps } from 'react';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { WikiChatDrawer } from './WikiChatDrawer';
import type { WikiChatTarget } from '../api/wikiChatApi';

const BASE = 'http://elitea.test/api/v2';

const TARGET: WikiChatTarget = {
  projectId: 7,
  toolkitId: 42,
  toolkitName: 'wiki',
  toolkitType: 'deepwiki',
  settings: { toolkit_configuration_llm_model: 'gpt-5' },
};

let idCounter = 0;
const newId = () => `id-${String((idCounter += 1))}`;

/** Bodies the invocation endpoint returns, in order, one per poll. */
function servePolls(polls: readonly unknown[]) {
  const queue = [...polls];
  server.use(
    http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, () =>
      HttpResponse.json({ invocation_id: 'inv-1' }),
    ),
    http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, () =>
      HttpResponse.json(queue.shift() ?? { status: 'InProgress' }),
    ),
  );
}

/**
 * The stored conversations this toolkit has, and their transcripts.
 *
 * These are the ORDINARY conversation routes — the drawer's history is not a
 * new endpoint, it is the chat listing asked two more questions
 * (`hidden=only`, `mine=true`). Serving them by default is what keeps every
 * pre-existing test in this file honest: without a handler the query would
 * fail and "the transcript did not load" would look like "there is no
 * transcript".
 */
function serveHistory(
  conversations: readonly { id: string; name: string; chatKey?: string }[],
  transcripts: Readonly<Record<string, readonly unknown[]>> = {},
) {
  server.use(
    http.get(`${BASE}/elitea_core/conversations/prompt_lib/:projectId`, ({ request }) => {
      historyQueries.push(new URL(request.url).searchParams);
      return HttpResponse.json({
        total: conversations.length,
        rows: conversations.map((conversation) => ({
          id: conversation.id,
          name: conversation.name,
          meta: conversation.chatKey ? { wiki_chat_key: conversation.chatKey } : {},
        })),
      });
    }),
    http.get(
      `${BASE}/elitea_core/conversation/prompt_lib/:projectId/:conversationId`,
      ({ params }) =>
        HttpResponse.json({
          message_groups: transcripts[String(params['conversationId'])] ?? [],
        }),
    ),
  );
}

/** The query strings the drawer asked its history with. */
let historyQueries: URLSearchParams[] = [];

/** One stored turn, in the shape the conversation-details route embeds. */
function group(content: string, options: { reply?: boolean; error?: boolean } = {}) {
  return {
    ...(options.reply === true ? { reply_to_id: 1 } : {}),
    ...(options.error === true ? { meta: { is_error: true } } : {}),
    message_items: [{ item_type: 'text_message', item_details: { content } }],
  };
}

beforeEach(() => {
  idCounter = 0;
  historyQueries = [];
  window.localStorage.clear();
  configureGeneratedClient({ baseUrl: BASE });
  serveHistory([]);
});
afterEach(() => {
  resetGeneratedClient();
});

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * Renders the drawer with everything it needs, `overrides` on top.
 *
 * The QueryClientProvider is not optional and is not this test file's
 * boilerplate: the drawer reads its stored conversation through react-query,
 * so a bare `render` throws "No QueryClient set" before a single assertion
 * runs — which is how a drawer test for an unrelated feature (the attachment
 * picker) failed on this branch until it came through here too. One helper,
 * so the next one cannot miss it.
 */
function open(overrides: Partial<ComponentProps<typeof WikiChatDrawer>> = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={client}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <WikiChatDrawer open onClose={vi.fn()} target={TARGET} newId={newId} {...overrides} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('the drawer', () => {
  it('invites a question when there is no conversation yet', () => {
    open();
    expect(screen.getByTestId('wiki-chat-drawer')).toBeInTheDocument();
    expect(screen.getByText(/Ask a question about this repository/)).toBeVisible();
    // Not an empty message list pretending to be a conversation.
    expect(screen.queryByTestId('wiki-chat-messages')).toBeNull();
  });

  it('carries a question to the provider and renders the answer as markdown', async () => {
    const user = userEvent.setup();
    // ONE poll carrying both, which is what the provider returns when a run
    // finishes between two polls. Two polls would need the 2s interval to
    // elapse, and a test that waits on a real timer is a test that flakes.
    servePolls([
      {
        status: 'Completed',
        result: 'The router is in **api/router.go**.',
        custom_events: [
          {
            data: {
              message: JSON.stringify({
                event: 'tool_start',
                data: { id: 't-1', tool: 'search', description: 'Searching the index' },
              }),
            },
          },
        ],
      },
    ]);

    open();
    await user.type(screen.getByLabelText('Question'), 'Where is the router?');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    // The question appears immediately, before anything has come back.
    expect(await screen.findByText('Where is the router?')).toBeVisible();

    // The tool card came off a poll, through the adapter, through the reducer.
    await waitFor(() => expect(screen.getByTestId('wiki-chat-thinking-block')).toBeVisible());

    const answer = await screen.findByTestId('wiki-chat-answer');
    // MARKDOWN, not the literal asterisks: the answer goes through
    // shared/ui/Markdown and `**api/router.go**` is emphasis, not text.
    expect(answer).toHaveTextContent('The router is in api/router.go.');
    expect(answer.querySelector('strong')).not.toBeNull();
  });

  it('shows the thinking log only when it is asked for', async () => {
    const user = userEvent.setup();
    servePolls([
      {
        status: 'Completed',
        result: 'done',
        custom_events: [
          {
            data: {
              message: JSON.stringify({ event: 'thinking', data: { id: 's', message: 'Reading files' } }),
            },
          },
        ],
      },
    ]);

    open();
    await user.type(screen.getByLabelText('Question'), 'q');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    const block = await screen.findByTestId('wiki-chat-thinking-block');
    // Collapsed by default: the log is context, the answer is the content.
    expect(screen.queryByText('Reading files')).toBeNull();

    await user.click(block.querySelector('button')!);
    expect(await screen.findByText('Reading files')).toBeVisible();
  });

  it('renders the answer as it streams in, before the run finishes', async () => {
    // The ANSWER TOKEN channel (issue #701), through the whole composition:
    // the provider's `llm_chunk` events, the adapter, the reducer's
    // accumulator, and the preview the messages list renders. Every one of
    // those was in place before this test except the middle two, and the
    // screen showed a spinner for the whole run.
    const user = userEvent.setup();
    const fragment = (text: string) => ({
      data: { message: JSON.stringify({ event: 'llm_chunk', data: { text } }) },
    });
    // ONE poll, still running: the first poll is immediate, and a second one
    // would need the 2s interval to elapse. What it must show is a partial
    // answer WITHOUT a finished one.
    servePolls([
      {
        status: 'InProgress',
        custom_events: [fragment('The router is in '), fragment('api/router.go')],
      },
    ]);

    open();
    await user.type(screen.getByLabelText('Question'), 'Where is the router?');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    const streaming = await screen.findByTestId('wiki-chat-streaming');
    // The fragments joined, in order, with no separator.
    expect(streaming).toHaveTextContent('The router is in api/router.go');
    // Not an answer: the turn has not settled, and a finished answer here
    // would be the preview being mistaken for one.
    expect(screen.queryByTestId('wiki-chat-answer')).toBeNull();
    // And not a thinking card either — that is the degraded reading this
    // channel exists to avoid.
    expect(screen.queryByText('The router is in ')).toBeNull();
  });

  it('reports a failed invocation instead of spinning', async () => {
    const user = userEvent.setup();
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, () =>
        HttpResponse.json({ error: 'no slots' }, { status: 503 }),
      ),
    );

    open();
    await user.type(screen.getByLabelText('Question'), 'q');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    expect(await screen.findByTestId('wiki-chat-error')).toBeVisible();
    // And the thinking block is GONE, not left running under a question that
    // never reached the provider.
    expect(screen.queryByTestId('wiki-chat-thinking-block')).toBeNull();

    // And the drawer accepts another question: a failure that left `isLoading`
    // set would refuse every later one with no visible reason.
    await user.type(screen.getByLabelText('Question'), 'try again');
    expect(screen.getByRole('button', { name: 'Send' })).toBeEnabled();
  });

  it('renders a research plan only when there is one', async () => {
    const user = userEvent.setup();
    servePolls([
      {
        status: 'Completed',
        result: 'answered',
        custom_events: [
          {
            data: {
              message: JSON.stringify({
                event: 'todo_update',
                data: { items: [{ id: 1, title: 'Read the README', status: 'pending' }] },
              }),
            },
          },
        ],
      },
    ]);

    open();
    // An `ask` run has no plan, and an empty panel would claim it has one.
    expect(screen.queryByTestId('wiki-chat-todos')).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Research' }));
    await user.type(screen.getByLabelText('Question'), 'how does auth work?');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    expect(await screen.findByTestId('wiki-chat-todos')).toBeVisible();
    expect(screen.getByText('Read the README')).toBeVisible();
  });

  it('asks AND POLLS the research tool when the mode says research', async () => {
    // Both halves, because they are two different paths. The poll URL carries
    // the tool segment as well, and polling `ask` for a `deep_research`
    // invocation returns a 404 that reads as a lost invocation: the spinner
    // never stops and nothing is logged.
    const user = userEvent.setup();
    let toolAsked: string | null = null;
    let toolPolled: string | null = null;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, ({ params }) => {
        toolAsked = String(params['tool']);
        return HttpResponse.json({ invocation_id: 'inv-1' });
      }),
      http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, ({ params }) => {
        toolPolled = String(params['tool']);
        return HttpResponse.json({ status: 'InProgress' });
      }),
    );

    open();
    await user.click(screen.getByRole('button', { name: 'Research' }));
    await user.type(screen.getByLabelText('Question'), 'q');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(toolAsked).toBe('deep_research'));
    await waitFor(() => expect(toolPolled).toBe('deep_research'));
  });

  it('reopens in the capability the last ANSWER used, not the toggle', async () => {
    // The toggle records an intention; the answer records what happened.
    window.localStorage.setItem(
      'el.deepwiki.chat.7.42',
      JSON.stringify([
        { role: 'user', content: 'q' },
        { role: 'assistant', content: 'a', capability: 'research' },
      ]),
    );
    window.localStorage.setItem('el.deepwiki.chat.capability.7.42', 'ask');

    open();
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Research' })).toHaveAttribute(
        'aria-pressed',
        'true',
      ),
    );
  });

  it('clears the conversation on request', async () => {
    const user = userEvent.setup();
    window.localStorage.setItem(
      'el.deepwiki.chat.7.42',
      JSON.stringify([{ role: 'user', content: 'an old question' }]),
    );

    open();
    expect(screen.getByText('an old question')).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Clear the conversation' }));
    expect(screen.queryByText('an old question')).toBeNull();
  });

  it('sends the pages the reader attached, with the open version pinned', async () => {
    // THE WIRING, which is the whole reason this test is here rather than in
    // the picker's own file. The picker holds a selection, the drawer folds it
    // into the target, and the API module renders it into `parameters` — three
    // correct halves and no proof any of them are joined. #597 is that defect.
    const user = userEvent.setup();
    let sent: Record<string, unknown> | undefined;
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, async ({ request }) => {
        const body = (await request.json()) as { parameters: Record<string, unknown> };
        sent = body.parameters;
        return HttpResponse.json({ invocation_id: 'inv-1' });
      }),
      http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, () =>
        HttpResponse.json({ status: 'InProgress' }),
      ),
    );

    open({
      target: { ...TARGET, wikiVersionId: 'fixture-1' },
      contextPages: ['wiki_pages/overview/getting-started.md', 'wiki_pages/components/storage.md'],
    });

    await user.click(screen.getByRole('button', { name: 'Attach wiki pages' }));
    await user.click(screen.getByText('components / storage'));
    await user.keyboard('{Escape}');

    await user.type(screen.getByLabelText('Question'), 'Where do notes live?');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => {
      expect(sent).toMatchObject({
        context_paths: ['wiki_pages/components/storage.md'],
        context_wiki_version_id: 'fixture-1',
      });
    });
  });

  it('offers no attachment control for a wiki with no pages', () => {
    // Not a disabled button: a picker with nothing in it reads as a broken
    // feature, and every wiki has pages once one has been generated.
    open();
    expect(screen.queryByRole('button', { name: 'Attach wiki pages' })).toBeNull();
  });
});

/*
 * SERVER-SIDE HISTORY.
 *
 * The conversation used to live in this browser alone. It now lives in the
 * tenant chat tables, written by elitea-main as each turn happens, and the
 * drawer READS it. These are the composition assertions: does the drawer ask
 * the right question of the listing, does what comes back reach the screen,
 * and what becomes of the conversation this browser was already holding.
 */
describe('the drawer restores a conversation from the server', () => {
  it('asks for THIS wiki’s own hidden conversations and nobody else’s', async () => {
    open();
    await waitFor(() => expect(historyQueries).toHaveLength(1));

    const query = historyQueries[0]!;
    // hidden=only, because a wiki chat is filed hidden so it does not surface
    // in the ordinary chat list — asking without it returns nothing at all.
    expect(query.get('hidden')).toBe('only');
    // mine=true, because that listing has never read is_private: without it
    // one member reads another member's questions.
    expect(query.get('mine')).toBe('true');
    expect(query.get('source')).toBe('deepwiki');
    expect(query.get('entity_name')).toBe('toolkit');
    expect(query.get('entity_meta_id')).toBe('42');
  });

  it('renders the stored turns of the conversation this browser is holding', async () => {
    // The key the drawer will mint for this wiki, so the stored conversation
    // is the one it resumes.
    window.localStorage.setItem('el.deepwiki.chat.conversation.7.42', 'chat-key-1');
    serveHistory([{ id: '11', name: 'earlier', chatKey: 'chat-key-1' }], {
      11: [group('Where do the pages live?'), group('In wiki_pages/.', { reply: true })],
    });

    open();

    expect(await screen.findByText('Where do the pages live?')).toBeVisible();
    expect(await screen.findByText('In wiki_pages/.')).toBeVisible();
  });

  /*
   * THE ASSERTION THAT MAKES THIS A FEATURE AND NOT A CACHE.
   *
   * A browser that has never been here mints a key of its own, so it matches
   * no stored conversation. Without adoption a second device would open empty
   * and the history would be visible only to the browser that wrote it —
   * which is exactly the state this change set out to end.
   */
  it('adopts the user’s latest stored conversation on a browser that has never been here', async () => {
    serveHistory([{ id: '11', name: 'from another device', chatKey: 'minted-elsewhere' }], {
      11: [group('asked on the laptop'), group('answered on the laptop', { reply: true })],
    });

    open();

    expect(await screen.findByText('asked on the laptop')).toBeVisible();
    expect(await screen.findByText('answered on the laptop')).toBeVisible();
    // And it keeps the adopted key, so the next question CONTINUES that
    // conversation rather than opening a third.
    await waitFor(() =>
      expect(window.localStorage.getItem('el.deepwiki.chat.conversation.7.42')).toBe(
        'minted-elsewhere',
      ),
    );
  });

  it('does not resume a conversation this browser does not hold the key to', async () => {
    window.localStorage.setItem('el.deepwiki.chat.conversation.7.42', 'chat-key-1');
    serveHistory([{ id: '11', name: 'somebody else’s chat', chatKey: 'chat-key-other' }], {
      11: [group('a question from another conversation')],
    });

    open();
    await waitFor(() => expect(historyQueries).toHaveLength(1));
    await waitFor(() =>
      expect(screen.getByText(/Ask a question about this repository/)).toBeVisible(),
    );
    expect(screen.queryByText('a question from another conversation')).toBeNull();
  });

  // THE MIGRATION, both halves. Nothing is deleted before it is replaced.
  it('keeps a local conversation on screen while the server has none', async () => {
    window.localStorage.setItem(
      'el.deepwiki.chat.7.42',
      JSON.stringify([{ role: 'user', content: 'asked before the server kept history' }]),
    );
    serveHistory([]);

    open();
    await waitFor(() => expect(historyQueries).toHaveLength(1));

    expect(screen.getByText('asked before the server kept history')).toBeVisible();
    expect(window.localStorage.getItem('el.deepwiki.chat.7.42')).not.toBeNull();
  });

  it('retires the local conversation once the server has one', async () => {
    window.localStorage.setItem(
      'el.deepwiki.chat.7.42',
      JSON.stringify([{ role: 'user', content: 'asked before the server kept history' }]),
    );
    window.localStorage.setItem('el.deepwiki.chat.conversation.7.42', 'chat-key-1');
    serveHistory([{ id: '11', name: 'stored', chatKey: 'chat-key-1' }], {
      11: [group('asked after'), group('answered after', { reply: true })],
    });

    open();

    expect(await screen.findByText('answered after')).toBeVisible();
    expect(screen.queryByText('asked before the server kept history')).toBeNull();
    await waitFor(() => expect(window.localStorage.getItem('el.deepwiki.chat.7.42')).toBeNull());
  });

  // "Clear" is now "start a new conversation": the previous one stays stored
  // and readable, and the next question opens a fresh one.
  it('starts a new conversation rather than erasing the stored one', async () => {
    const user = userEvent.setup();
    window.localStorage.setItem('el.deepwiki.chat.conversation.7.42', 'chat-key-1');
    serveHistory([{ id: '11', name: 'stored', chatKey: 'chat-key-1' }], {
      11: [group('an earlier question')],
    });

    open();
    expect(await screen.findByText('an earlier question')).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Clear the conversation' }));

    expect(screen.queryByText('an earlier question')).toBeNull();
    await waitFor(() =>
      expect(window.localStorage.getItem('el.deepwiki.chat.conversation.7.42')).not.toBe(
        'chat-key-1',
      ),
    );
  });

  // The invoke has to CARRY the conversation, or the server files every
  // question into a new one and a reload shows a single turn.
  it('sends the conversation and the toolkit with every question', async () => {
    const user = userEvent.setup();
    const headers: Headers[] = [];
    server.use(
      http.post(`${BASE}/deepwiki/tools/:projectId/:toolkit/:tool/invoke`, ({ request }) => {
        headers.push(request.headers);
        return HttpResponse.json({ invocation_id: 'inv-1' });
      }),
      http.get(`${BASE}/deepwiki/invocations/:projectId/:toolkit/:tool/:invocation`, () =>
        HttpResponse.json({ status: 'InProgress' }),
      ),
    );

    open();
    await user.type(screen.getByPlaceholderText('Ask about this repository'), 'a question');
    await user.click(screen.getByRole('button', { name: 'Send' }));

    await waitFor(() => expect(headers).toHaveLength(1));
    expect(headers[0]!.get('X-Elitea-Wiki-Chat')).toBeTruthy();
    expect(headers[0]!.get('X-Elitea-Wiki-Toolkit')).toBe('42');
  });

});
