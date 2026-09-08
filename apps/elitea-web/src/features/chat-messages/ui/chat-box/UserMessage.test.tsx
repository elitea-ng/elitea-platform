/**
 * Who a reloaded question is attributed to.
 *
 * `GET /elitea_core/messages/prompt_lib/{project}/{conversation}` — what a
 * conversation reload and a shared-link open both read — answers rows carrying
 * no author identity of ANY kind (measured: `{id, uid, conversation_id, role,
 * content, content_type, metadata, created_at}`). `entities/message`'s
 * normaliser therefore yields `name: ''` and omits `userId` for every one of
 * them, and this component is what decides the caption.
 *
 * These cases exist because the previous answer — substitute the SIGNED-IN
 * user's name whenever the row names nobody — stamped the reader's name on
 * every user bubble of a shared transcript, other people's included. The
 * `userId === undefined` guard could not stop it: the endpoint omits `userId`
 * too, so the guard only ever fired on the live paths (which already carry a
 * name of their own), and the old test for it used a `userId`-without-`name`
 * state this endpoint cannot produce.
 *
 * The trap that keeps the substitution from coming back is deliberately two
 * halves, because a bare "the reader's name is not on screen" assertion would
 * also pass against a re-added query that simply had not resolved yet:
 *  - `GET /social/author` IS stubbed (MSW, per R-M1 — no `vi.mock`), so a
 *    reintroduced fallback would have a real name to render rather than
 *    failing its request and rendering nothing by accident; and
 *  - the row's `QueryClient` cache must stay EMPTY, which React Query settles
 *    synchronously during render — a reintroduced `useGetCurrentAuthor`
 *    registers its observer before `render()` returns, resolved or not.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../../test/setup';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { UserMessage } from './UserMessage';

const BASE = '/api/v2';
/** The person doing the reading — the name that must never be borrowed. */
const READER = 'E2E Chat Driver';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

afterEach(() => {
  resetGeneratedClient();
});

/**
 * Makes the signed-in identity available to answer `GET /social/author`. No
 * assertion here expects it to be requested — it is the payload half of the
 * trap described in the file header.
 */
function stubCurrentAuthor(): void {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/social/author`, () =>
      HttpResponse.json({ id: 'me', name: READER, email: 'driver@example.com' }),
    ),
  );
}

function buildMessage(index: number, overrides: Partial<ChatMessage>): ChatMessage {
  return {
    id: `q${String(index)}`,
    role: 'user',
    name: '',
    content: 'hello',
    createdAt: '2026-08-29T10:00:00Z',
    ...overrides,
  };
}

/** Renders one row per `overrides` entry into a single tree, and hands back its `QueryClient`. */
function renderMessages(...overrides: readonly Partial<ChatMessage>[]): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const messages = overrides.map((override, index) => buildMessage(index, override));
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        {messages.map((message) => (
          <UserMessage key={message.id} message={message} messageId={message.id} />
        ))}
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return queryClient;
}

const renderMessage = (override: Partial<ChatMessage>): QueryClient => renderMessages(override);

describe('UserMessage author caption', () => {
  it('renders no caption at all for the author-less row the persisted endpoint returns', () => {
    stubCurrentAuthor();
    // Exactly what `normaliseUserMessage` produces for a reloaded row: an
    // empty name and NO `userId`.
    const queryClient = renderMessage({ name: '' });

    // The caption line is still there (avatar + timestamp — every production
    // row has one), but it names NOBODY: no author, and in particular not the
    // reader. Asserting on the whole row's textContent would pin the caption's
    // layout rather than its attribution, so the name is asserted directly.
    expect(screen.getByTestId('chat-message-body').textContent).toBe('hello');
    expect(screen.getByTestId('chat-message-header').textContent).not.toContain(READER);
    expect(screen.queryByText(READER)).toBeNull();
    expect(queryClient.getQueryCache().getAll()).toHaveLength(0);
  });

  it('does not caption other people\'s messages with the reader\'s name on a shared-conversation reload', () => {
    stubCurrentAuthor();
    // A shared conversation, reloaded: the reader wrote one of these and a
    // colleague wrote the other, and the endpoint says which for neither.
    const queryClient = renderMessages(
      { content: 'a question the reader asked' },
      { content: 'a question a colleague asked' },
    );

    expect(screen.getByText('a question the reader asked')).toBeTruthy();
    expect(screen.getByText('a question a colleague asked')).toBeTruthy();
    for (const row of screen.getAllByTestId('user-message')) {
      expect(row.textContent).not.toContain(READER);
    }
    expect(queryClient.getQueryCache().getAll()).toHaveLength(0);
  });

  it('keeps a genuinely departed author as such, rather than blanking or re-attributing it', () => {
    stubCurrentAuthor();
    // A row that DID state an author which resolves to nobody — a real fact
    // about the message, and a different case from stating none at all.
    renderMessage({ name: 'User No Longer Available' });

    expect(screen.getByText('User No Longer Available')).toBeTruthy();
    expect(screen.queryByText(READER)).toBeNull();
  });

  it('captions a question that names another user with that user\'s name', () => {
    stubCurrentAuthor();
    renderMessage({ name: 'Bob Reviewer', userId: 'user-7' });

    expect(screen.getByText('Bob Reviewer')).toBeTruthy();
    expect(screen.queryByText(READER)).toBeNull();
  });

  it('still captions the message the reader just sent, which carries its own name', () => {
    stubCurrentAuthor();
    // The shape `buildOptimisticUserMessage` builds on send
    // (`widgets/chat-box/ui/hooks/useChatBoxHandlers.helpers.ts`): the sender
    // is known at that moment, so the row states it and needs no fallback.
    const queryClient = renderMessage({ name: READER, userId: 'me' });

    expect(screen.getByText(READER)).toBeTruthy();
    // ...and it came off the message, not off a re-read of the signed-in user.
    expect(queryClient.getQueryCache().getAll()).toHaveLength(0);
  });
});


/**
 * The caption line itself, which did not exist.
 *
 * Every production transcript row — user and assistant alike — opens with
 * `<24px avatar> <author> to <recipient>` and closes with a right-aligned
 * relative timestamp (measured live on next.elitea.ai at 2000px, and visible
 * in every reference screenshot). This component rendered a right-aligned
 * `primary.main` chat bubble with a bare `caption` name above it and no
 * recipient and no timestamp at all — a layout the product has nowhere.
 *
 * Structural, so it is asserted structurally: the elements have to be
 * PRESENT. A style-only assertion would keep passing if the header were
 * dropped again.
 */
describe('UserMessage caption line', () => {
  it('captions the row with the author, the recipient and the time', () => {
    stubCurrentAuthor();
    renderMessage({
      name: 'Bob Reviewer',
      userId: 'user-7',
      sentTo: { entity_name: 'dummy', entity_meta: {}, meta: {} },
    });

    const header = screen.getByTestId('chat-message-header');
    expect(header.textContent).toContain('Bob Reviewer');
    // `to <recipient>`: a dummy participant resolves to the deployment's
    // system sender name, which defaults to the baseline's 'Elitea'.
    expect(screen.getByTestId('chat-message-recipient').textContent).toBe('Elitea');
    expect(screen.getByTestId('chat-message-time')).toBeTruthy();
    expect(screen.getByTestId('chat-message-avatar')).toBeTruthy();
  });

  it('drops the whole "to ..." clause when the row names no recipient', () => {
    stubCurrentAuthor();
    renderMessage({ name: 'Bob Reviewer', userId: 'user-7' });

    expect(screen.getByTestId('chat-message-header').textContent).toContain('Bob Reviewer');
    expect(screen.queryByTestId('chat-message-recipient')).toBeNull();
  });

  it('shows the copy action at rest, not only on hover', () => {
    stubCurrentAuthor();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage
            message={buildMessage(0, { name: 'Bob Reviewer' })}
            messageId="q0"
            onCopy={() => undefined}
          />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    // The production row's action strip is `visibility: visible` (measured);
    // this one was hard-coded `hidden`, so the icons the reference screenshots
    // show were simply never painted.
    const actions = screen.getByRole('button', { name: 'Copy to clipboard' });
    expect(actions).toBeTruthy();
    expect(actions.closest('.actionButtons')).toBeTruthy();
  });
});
