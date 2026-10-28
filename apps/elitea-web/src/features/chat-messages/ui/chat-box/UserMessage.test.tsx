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
import { fireEvent, render, screen } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

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

/**
 * Renders one row per `overrides` entry into a single tree, and hands back its
 * `QueryClient`. Rows render as someone ELSE's question (`isOwn={false}`):
 * that is the only side that carries an author caption, and attribution is
 * what most cases here are about.
 */
function renderMessages(...overrides: readonly Partial<ChatMessage>[]): QueryClient {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const messages = overrides.map((override, index) => buildMessage(index, override));
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        {messages.map((message) => (
          <UserMessage key={message.id} message={message} messageId={message.id} isOwn={false} />
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

    // No caption names anybody: no author, and in particular not the reader.
    expect(screen.getByTestId('chat-message-body').textContent).toBe('hello');
    expect(screen.queryByTestId('chat-message-author')).toBeNull();
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

});


/**
 * The row is drawn as a chat, not as the baseline's caption line
 * (`<avatar> <author> to <recipient>` over every bubble, the time hard right):
 * the reader's own question is a right-hand bubble with no name at all,
 * anyone else's a left-hand bubble under only their name, and the time rides
 * at the end of the action row. Asserted structurally — a style-only check
 * would keep passing if the caption line came back.
 */
describe('UserMessage chat layout', () => {
  function renderOne(isOwn: boolean, override: Partial<ChatMessage>, handlers: { readonly onCopy?: () => void } = {}): void {
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage message={buildMessage(0, override)} messageId="q0" isOwn={isOwn} {...handlers} />
        </ThemeProvider>
      </QueryClientProvider>,
    );
  }

  it("draws the reader's own question on the right, with no name, avatar or recipient", () => {
    stubCurrentAuthor();
    renderOne(true, { name: READER, userId: 'me', sentTo: { entity_name: 'dummy', entity_meta: {}, meta: {} } });

    expect(screen.getByTestId('user-message')).toHaveStyle({ alignItems: 'flex-end' });
    expect(screen.getByTestId('chat-message-body').textContent).toBe('hello');
    expect(screen.queryByText(READER)).toBeNull();
    expect(screen.queryByTestId('chat-message-author')).toBeNull();
    expect(screen.queryByTestId('chat-message-avatar')).toBeNull();
    expect(screen.queryByText('Elitea')).toBeNull();
  });

  it("draws someone else's question on the left under only their name", () => {
    stubCurrentAuthor();
    renderOne(false, { name: 'Bob Reviewer', userId: 'user-7', sentTo: { entity_name: 'dummy', entity_meta: {}, meta: {} } });

    expect(screen.getByTestId('user-message')).toHaveStyle({ alignItems: 'flex-start' });
    expect(screen.getByTestId('chat-message-author').textContent).toBe('Bob Reviewer');
    // No "to <recipient>" clause and no avatar: the name is the whole caption.
    expect(screen.queryByText('Elitea')).toBeNull();
    expect(screen.queryByTestId('chat-message-avatar')).toBeNull();
  });

  it('puts the time at the end of the action row, not over the bubble', () => {
    stubCurrentAuthor();
    renderOne(true, { name: READER }, { onCopy: () => undefined });

    const row = screen.getByTestId('chat-message-time').closest('[data-reply-actions]');
    expect(row).toBeTruthy();
    expect(row?.lastElementChild?.contains(screen.getByTestId('chat-message-time'))).toBe(true);
    expect(screen.getByTestId('chat-message-body').contains(screen.getByTestId('chat-message-time'))).toBe(false);
  });

  it('draws its actions as a quiet row under the bubble, not inside it', () => {
    stubCurrentAuthor();
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage
            message={buildMessage(0, { name: 'Bob Reviewer' })}
            messageId="q0"
            onCopy={() => undefined}
            onSubmit={() => undefined}
            onFork={() => undefined}
            onDelete={() => undefined}
          />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    const copy = screen.getByRole('button', { name: 'Copy to clipboard' });
    const row = copy.closest('[data-reply-actions]');
    expect(row).toBeTruthy();
    // Outside the bubble, still inside the message (its hover reveals the row).
    expect(screen.getByTestId('chat-message-body').contains(copy)).toBe(false);
    expect(screen.getByTestId('user-message').contains(row)).toBe(true);
    const names = Array.from(row?.querySelectorAll('button') ?? []).map((button) => button.getAttribute('aria-label'));
    expect(names).toEqual(['Copy to clipboard', 'Edit the message and regenerate answer', 'Fork from here', 'Delete']);
    // Faded, not hidden: every control stays focusable.
    for (const button of row?.querySelectorAll('button') ?? []) expect(button).not.toBeDisabled();
    expect(screen.getByTestId('message-fork')).toBeTruthy();
  });

  it('renders no action row when no action is offered and there is no time to show', () => {
    stubCurrentAuthor();
    renderMessage({ name: 'Bob Reviewer', createdAt: '' });
    expect(document.querySelector('[data-reply-actions]')).toBeNull();
  });
});

/*
 * A17 (ELITEA-2870). A question that was typed while a previous turn was still
 * running is captioned "Sent while running", and the caption has to survive a
 * reload — which is why the flag rides on the normalised `ChatMessage` rather
 * than on some live send-time state. The negative case matters just as much:
 * an ORDINARY question must not carry it, or the caption says nothing.
 */
describe('UserMessage interjection caption', () => {
  it('captions a question delivered from the waiting queue', () => {
    renderMessage({ name: 'Bob Reviewer', interjected: true });
    expect(screen.getByTestId('chat-message-interjected')).toHaveTextContent('Sent while running');
  });

  it('leaves an ordinary question uncaptioned', () => {
    renderMessage({ name: 'Bob Reviewer' });
    expect(screen.queryByTestId('chat-message-interjected')).toBeNull();
  });
});

/**
 * Issues programme, package C-chat — the edit-and-resubmit flow, judged
 * against three CLOSED legacy defects that all targeted this exact
 * component. Rendered directly (not through a live conversation) because
 * seeding a STORED message with an attachment needs a real model turn,
 * which this wave's stack does not offer — `UserMessage` is the entire
 * surface those three bugs lived in, so exercising it directly is a faithful
 * reproduction, not a weaker one.
 */
describe('UserMessage edit-and-resubmit (elitea_issues #5140, #5221, #5138, #5137)', () => {
  const attachmentItem = {
    item_type: 'attachment_message',
    item_details: { name: 'photo.png', content: { type: 'image_url', image_url: { url: 'https://example.test/photo.png' } } },
  };
  const questionItem = { uuid: 'q-uuid-1', item_type: 'text_message', item_details: { content: 'What is the capital of France?' } };

  /* elitea_issues: #5140, #5221 — entering edit mode on a message that has an attachment
   * keeps the attachment visible (not lost), and its remove control is wired to a real
   * callback (not a no-op). */
  it('#5140/#5221: an attachment stays visible in edit mode and its remove control calls back', () => {
    const onRemoveAttachment = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const message = buildMessage(0, {
      content: 'What is the capital of France?',
      messageItems: [questionItem, attachmentItem] as unknown as ChatMessage['messageItems'],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage
            message={message}
            messageId={message.id}
            onSubmit={vi.fn()}
            onRemoveAttachment={onRemoveAttachment}
          />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    // Attachment visible before edit.
    expect(screen.getByText('photo.png')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('button', { name: 'Edit the message and regenerate answer' }));

    // Still visible IN edit mode — #5140's regression was that it vanished here.
    expect(screen.getByText('photo.png')).toBeInTheDocument();
  });

  /* elitea_issues: #5138 — Save and apply is disabled once the field is cleared to empty,
   * so an empty message can never reach the LLM. */
  it('#5138: Save and apply is disabled for an empty edited message', () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const message = buildMessage(0, {
      content: 'What is the capital of France?',
      messageItems: [questionItem] as unknown as ChatMessage['messageItems'],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage message={message} messageId={message.id} onSubmit={vi.fn()} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Edit the message and regenerate answer' }));
    const saveButton = screen.getByRole('button', { name: 'Save and apply' });
    // UNCHANGED IS ENABLED (issue 980): this test used to assert the opposite here,
    // which is the behaviour that made the retry onetest ELITEA-0540 describes
    // unreachable. What #5138 is about is the EMPTY field, asserted below.
    expect(saveButton).toBeEnabled();

    const textbox = screen.getByRole('textbox');
    fireEvent.change(textbox, { target: { value: '' } });
    expect(saveButton).toBeDisabled();

    fireEvent.change(textbox, { target: { value: '   ' } });
    expect(saveButton, 'whitespace-only content must not count as a real edit').toBeDisabled();
  });

  /* elitea_issues: #5137 — Save and apply hands the caller the EDITED text, not the
   * message's original (pre-edit) content; the caller regenerates off whatever
   * `onSubmit` receives, so this is the whole fix surface for "regenerates using OLD
   * content". */
  it('#5137: Save and apply submits the NEW edited text, not the original', () => {
    const onSubmit = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const message = buildMessage(0, {
      content: 'What is the capital of France?',
      messageItems: [questionItem] as unknown as ChatMessage['messageItems'],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage message={message} messageId={message.id} onSubmit={onSubmit} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Edit the message and regenerate answer' }));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'What is the capital of Germany?' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and apply' }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const [, updatedItems] = onSubmit.mock.calls[0] as [string, ReadonlyArray<{ content: string }>];
    expect(updatedItems[0]?.content).toBe('What is the capital of Germany?');
    expect(updatedItems[0]?.content).not.toBe('What is the capital of France?');
  });
  /* onetest: ELITEA-0540, issue 980 — "Save and apply" on an UNCHANGED question is a
   * retry, and must reach the caller. It was disabled on `value ===
   * resolvedContent`, so re-running a question after a bad answer could not be
   * done from the editor at all. */
  it('issue 980: an unchanged question can still be saved, and submits its text', () => {
    const onSubmit = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const message = buildMessage(0, {
      content: 'What is the capital of France?',
      messageItems: [questionItem] as unknown as ChatMessage['messageItems'],
    });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage message={message} messageId={message.id} onSubmit={onSubmit} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Edit the message and regenerate answer' }));
    const saveButton = screen.getByRole('button', { name: 'Save and apply' });
    expect(saveButton, 'a retry of the same question must be reachable').toBeEnabled();
    fireEvent.click(saveButton);

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const [, updatedItems] = onSubmit.mock.calls[0] as [string, ReadonlyArray<{ content: string }>];
    expect(updatedItems[0]?.content).toBe('What is the capital of France?');
  });

  /* onetest: ELITEA-0545, issue 980 — a message with NO stored `text_message` item
   * (the ordinary shape of a question the browser is still holding from the
   * send that created it) must still submit its edited text. It used to submit
   * an EMPTY array, which `handleSubmitEditedMessage` drops silently: the
   * editor closed, the bubble updated locally, and no request was made. */
  it('issue 980: a message with no stored question item still submits its edited text', () => {
    const onSubmit = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const message = buildMessage(0, { content: 'What is the capital of France?' });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <UserMessage message={message} messageId={message.id} onSubmit={onSubmit} />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.click(screen.getByRole('button', { name: 'Edit the message and regenerate answer' }));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'What is the capital of Spain?' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save and apply' }));

    expect(onSubmit).toHaveBeenCalledTimes(1);
    const [, updatedItems] = onSubmit.mock.calls[0] as [
      string,
      ReadonlyArray<{ content: string; item_type: string; uuid?: string }>,
    ];
    expect(updatedItems, 'the caller must be given something to send').toHaveLength(1);
    expect(updatedItems[0]?.content).toBe('What is the capital of Spain?');
    expect(updatedItems[0]?.item_type).toBe('text_message');
    // No `uuid` is invented for an item that does not exist: the field names
    // WHICH stored row to rewrite.
    expect(updatedItems[0]?.uuid).toBeUndefined();
  });
});
