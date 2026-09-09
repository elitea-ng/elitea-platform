/**
 * Pins the `projectId` threading `ChatMessageList` owes each `UserMessage`
 * row's attachment cards.
 *
 * The row-level behaviour is pinned by `UserMessage.attachments.test.tsx`;
 * this case exists because the original defect was WIRING, not behaviour —
 * `ChatMessageList` mounted `UserMessage` with neither `projectId` nor an
 * error surface, so every storage-backed download refused silently even
 * though both endpoints of the chain were individually correct. A test of
 * either endpoint alone cannot see that (same class as `ChatBoxInputSlots.
 * test.tsx`'s slot-builder rationale), so this one drives the download
 * through the LIST and asserts the artifact fetch really happens.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { artifactContentOk } from '@/test/msw/handlers/artifacts';
import type { CapturedArtifactsRequest } from '@/test/msw/handlers/artifacts';

import { server } from '../../../../test/setup';

import type { MessageGroupWire, MessageParticipantWire } from '@/entities/message';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';
import { convertMessagesToChatHistory } from '../../lib/convertMessagesToChatHistory';
import { ChatMessageList } from './ChatMessageList';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const globals = globalThis as unknown as Record<string, unknown>;

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
  vi.restoreAllMocks();
});

const USER_MESSAGE_WITH_ATTACHMENT = {
  id: 'q1',
  role: 'user',
  name: 'Alice',
  content: 'here is the file',
  createdAt: '2026-08-29T10:00:00Z',
  messageItems: [
    {
      item_type: 'attachment_message',
      uuid: 'att-1',
      item_details: {
        name: 'report.pdf',
        filepath: '/my-bucket/folder/report.pdf',
        bucket: 'my-bucket',
        attachment_type: 'document',
      },
    },
  ],
} as unknown as ChatMessage;

describe('ChatMessageList attachment download threading', () => {
  it('threads projectId to the user row so a storage-backed download really fetches', async () => {
    globals['elitea_ui_config'] = {
      vite_server_url: '/api/v2',
      vite_base_uri: '/',
      vite_public_project_id: '1',
    };
    resetConfigForTests();
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/social/author`, () => HttpResponse.json({ id: 'me', name: 'Alice' })));
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    // jsdom implements neither smooth scrolling nor layout; the list's
    // bottom-anchor effect calls this unconditionally on mount.
    Element.prototype.scrollIntoView = vi.fn();
    const sink: CapturedArtifactsRequest[] = [];
    server.use(artifactContentOk(sink));

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <ChatMessageList chatHistory={[USER_MESSAGE_WITH_ATTACHMENT]} projectId="p1" />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    fireEvent.mouseEnter(screen.getByTestId('chat-artifact-file-card'));
    fireEvent.click(screen.getByLabelText('Download attachment'));

    await waitFor(() => {
      expect(sink.length).toBe(1);
    });
    const request = sink[0];
    if (request === undefined) throw new Error('unreachable');
    expect(new URL(request.url).pathname).toBe('/api/v2/artifacts/objects/p1/my-bucket/folder/report.pdf');
  });
});

/**
 * The question-edit affordance (`isEligibleForEdit` → `UserMessage`'s
 * `onSubmit`/`onEdit` → the "Edit the message and regenerate answer" button)
 * on a PERSISTED, reloaded question — the case a live send/reload actually
 * exercises. It is offered only when the reader authored the question, which
 * `ChatMessageList` decides by comparing the current-author id (`userId`)
 * against the message's `userId`.
 *
 * These build the transcript the way the real path does — a
 * conversation-details payload (participants + message_groups) run through
 * `convertMessagesToChatHistory`/`normaliseUserMessage` — rather than
 * hand-stamping `ChatMessage.userId`, so the wire truth is exercised end to
 * end: the participants payload states the author id as a NUMBER
 * (`entity_meta.id`, measured on the live stack), and `userOptionalFields`
 * wraps it in `String()`, so `message.userId` reaches the comparison as a
 * string. The current-author id (`GET /social/author` → `useGetCurrentAuthor`)
 * is an unvalidated envelope; the last case pins that a numeric spelling of it
 * still resolves the match — the string-vs-number trap a bare `===` would miss.
 */
const EDIT_LABEL = 'Edit the message and regenerate answer';

/**
 * A persisted question + answer, authored by the user whose `entity_meta.id`
 * is `authorUserId` (stated as a NUMBER, as the live participants payload
 * does). `undefined` builds the unattributed case: the participant states no
 * `entity_meta` at all, so `userOptionalFields` omits `userId` and the
 * question reaches the list stating no author.
 */
function buildPersistedTranscript(authorUserId: number | undefined): readonly ChatMessage[] {
  const participants = [
    {
      id: 1,
      entity_name: 'user',
      ...(authorUserId === undefined ? {} : { entity_meta: { id: authorUserId, project_id: 90106 } }),
      meta: { user_name: 'Reader' },
    },
    { id: 229, entity_name: 'application', entity_meta: { id: '274', name: 'agent' }, meta: { name: 'agent' } },
  ] as unknown as MessageParticipantWire[];
  const messageGroups = [
    {
      id: 1069,
      uuid: 'q-uuid',
      author_participant_id: 1,
      sent_to_id: 229,
      content: 'my own question',
      created_at: '2026-08-29T10:00:00Z',
      message_items: [{ id: 1, item_details: { content: 'my own question' } }],
    },
    { id: 1070, uuid: 'a-uuid', author_participant_id: 229, reply_to_id: 1069, content: 'the answer', created_at: '2026-08-29T10:00:01Z' },
  ] as unknown as MessageGroupWire[];
  return convertMessagesToChatHistory(messageGroups, participants);
}

function renderList(chatHistory: readonly ChatMessage[], userId: string): ReturnType<typeof render> {
  // jsdom implements neither smooth scrolling nor layout; the list's
  // bottom-anchor effect calls this unconditionally on mount.
  Element.prototype.scrollIntoView = vi.fn();
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <ChatMessageList
          chatHistory={chatHistory}
          projectId="90106"
          userId={userId}
          messageActions={{ onSubmitEditedMessage: () => undefined, onDeleteAnswer: () => undefined }}
        />
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

describe('ChatMessageList question-edit eligibility', () => {
  it("offers the edit control on the reader's OWN reloaded question (author id a number in the payload, current author id a string)", () => {
    const chatHistory = buildPersistedTranscript(6);
    // normalise turned the numeric entity_meta.id into a string on the message.
    expect(chatHistory[0]?.userId).toBe('6');
    // useGetCurrentAuthor states the id as a string.
    renderList(chatHistory, '6');
    expect(screen.getByLabelText(EDIT_LABEL)).toBeInTheDocument();
  });

  it('withholds the edit control when the question was authored by SOMEONE ELSE', () => {
    const chatHistory = buildPersistedTranscript(7);
    renderList(chatHistory, '6');
    expect(screen.queryByLabelText(EDIT_LABEL)).not.toBeInTheDocument();
  });

  it('still offers the edit control when the current author id arrives as a NUMBER (string-vs-number robustness)', () => {
    const chatHistory = buildPersistedTranscript(6);
    // The /social/author envelope is unvalidated; a bare `===` against the
    // string message.userId would fail on a numeric id, so this pins the
    // String()-normalised comparison.
    renderList(chatHistory, 6 as unknown as string);
    expect(screen.getByLabelText(EDIT_LABEL)).toBeInTheDocument();
  });

  it("keeps edit AND delete while the reader is UNRESOLVED — ChatBox states '' , not undefined", () => {
    // The unstated-reader escape hatch ("no reader stated ⇒ keep the
    // pre-identity behaviour"). `ChatBox` renders `userId={userId ?? ''}`, so
    // a reader whose `useGetCurrentAuthor` query is merely PENDING — or has
    // failed outright — arrives here as the empty string, never `undefined`.
    // Gating on `userId === undefined` alone made the hatch unreachable and
    // silently refused the reader both controls on their own message.
    const chatHistory = buildPersistedTranscript(6);
    renderList(chatHistory, '');
    expect(screen.getByLabelText(EDIT_LABEL)).toBeInTheDocument();
    // The AI answer's delete goes through `canDeleteAiMessage`, i.e. the
    // question's author — the same hatch, on the other control.
    expect(screen.getByLabelText('Delete')).toBeInTheDocument();
  });

  it('withholds edit AND delete on a question that states NO author, when the reader IS known', () => {
    // The other half of the asymmetry: an unstated AUTHOR must not be treated
    // as "everyone's", or every unattributed row in a shared conversation
    // becomes editable and deletable by whoever opens it.
    const chatHistory = buildPersistedTranscript(undefined);
    expect(chatHistory[0]?.userId).toBeUndefined();
    renderList(chatHistory, '6');
    expect(screen.queryByLabelText(EDIT_LABEL)).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Delete')).not.toBeInTheDocument();
  });
});

/**
 * The "create canvas from a text selection" affordance on the conversation's
 * LAST message — a finding from the embedded-docs screenshot unit: the
 * control never rendered there, though it worked fine on any earlier message.
 *
 * ── ROOT CAUSE ──────────────────────────────────────────────────────────
 * `ChatMessageList`'s per-message `messageIsStreaming` used to OR the page-
 * level `isStreaming` flag into the LAST message's own state unconditionally
 * (`isLastMessage && isStreaming`). That page-level flag is itself an OR of
 * three sources (`ChatBox.tsx`'s own comment on it: composer Stop, persisted-
 * history catch-up, the live transport), and none of them is guaranteed to
 * flip back to `false` the instant the LAST message's own turn ends — the
 * persisted-history source in particular is read off a conversation-messages
 * fetch a finished turn does not itself invalidate. So a page that had
 * genuinely finished answering could still read `isStreaming: true` at the
 * page level, and the formula piped that straight onto the one message a
 * reader was looking at, permanently hiding "Create canvas" (and disabling
 * regenerate) there — while every earlier, non-last message read its own
 * (correctly settled) `isStreaming` and worked fine.
 *
 * The fixture below reproduces exactly that split: the LAST message states
 * its own `isStreaming` as settled (here, simply absent — the normal shape
 * for a historically-loaded row, see `entities/message`'s
 * `assistantStreamingFields`) and already carries a real answer, while the
 * PAGE-LEVEL `isStreaming` prop is `true`, standing in for the stuck flag.
 */
function selectInAnswer(text: string): void {
  const item = screen.getByTestId('answer-text-item');
  const walker = document.createTreeWalker(item, NodeFilter.SHOW_TEXT);
  let node = walker.nextNode();
  while (node !== null) {
    const offset = (node.textContent ?? '').indexOf(text);
    if (offset >= 0) {
      const range = document.createRange();
      range.setStart(node, offset);
      range.setEnd(node, offset + text.length);
      const selection = document.getSelection();
      selection?.removeAllRanges();
      selection?.addRange(range);
      fireEvent.mouseUp(document);
      return;
    }
    node = walker.nextNode();
  }
  throw new Error(`the rendered answer holds no text node containing ${text}`);
}

describe('ChatMessageList: canvas-from-selection on the LAST message', () => {
  it('offers "Create canvas" on the last message even while the page-level isStreaming flag is stuck true', () => {
    const answerText = 'AUTOTEST select this settled answer please';
    const chatHistory: readonly ChatMessage[] = [
      {
        id: 'q1',
        role: 'user',
        name: 'Reader',
        content: 'make me an answer',
        createdAt: '2026-09-09T10:00:00Z',
      },
      {
        id: 'a1',
        role: 'assistant',
        name: 'agent',
        content: answerText,
        createdAt: '2026-09-09T10:00:01Z',
        // Deliberately NOT `isStreaming: false` — a historically-loaded
        // message states no `isStreaming` at all (see module doc), which is
        // exactly the shape that used to fall through to the page-level flag.
      },
    ] as unknown as readonly ChatMessage[];

    Element.prototype.scrollIntoView = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    const onCreateFromSelection = vi.fn();
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <ChatMessageList
            chatHistory={chatHistory}
            projectId="90106"
            userId="6"
            // The stuck/stale page-level flag this bug is about.
            isStreaming
            canvas={{ onCreateFromSelection }}
          />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    expect(screen.getByTestId('application-answer')).toHaveTextContent(answerText);
    selectInAnswer('settled answer');

    expect(
      screen.getByTestId('canvas-create-from-selection'),
      'a last message that already carries an answer must not stay blocked by a page-level isStreaming flag',
    ).toBeVisible();
  });

  it('still withholds it while THIS message is genuinely mid-stream (no content yet)', () => {
    const chatHistory: readonly ChatMessage[] = [
      { id: 'q1', role: 'user', name: 'Reader', content: 'make me an answer', createdAt: '2026-09-09T10:00:00Z' },
      { id: 'a1', role: 'assistant', name: 'agent', content: '', createdAt: '2026-09-09T10:00:01Z', isStreaming: true },
    ] as unknown as readonly ChatMessage[];

    Element.prototype.scrollIntoView = vi.fn();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
    render(
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <ChatMessageList
            chatHistory={chatHistory}
            projectId="90106"
            userId="6"
            isStreaming
            canvas={{ onCreateFromSelection: vi.fn() }}
          />
        </ThemeProvider>
      </QueryClientProvider>,
    );

    // No answer text exists yet to select, and the loading affordance shows
    // instead — this pins that the fix narrows the fallback rather than
    // deleting it.
    expect(screen.queryByTestId('answer-text-item')).not.toBeInTheDocument();
  });
});
