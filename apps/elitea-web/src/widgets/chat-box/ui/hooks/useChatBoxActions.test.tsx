import { act, renderHook, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { useChatBoxActions } from './useChatBoxActions';
import type { UseChatBoxActionsParams } from './useChatBoxActions';

type OnConversationCreated = NonNullable<UseChatBoxActionsParams['onConversationCreated']>;

function renderActions(
  sendQuestion: ReturnType<typeof vi.fn>,
  onConversationCreated: OnConversationCreated,
) {
  return renderHook(() =>
    useChatBoxActions({
      chatInputRef: { current: { reset: vi.fn(), setValue: vi.fn() } },
      data: {
        hasPendingHitlInterrupt: false,
        attachments: { state: { attachments: [], onClearAttachments: vi.fn() } },
      } as never,
      state: {
        isActiveParticipantBroken: false,
        isMentioningEveryone: false,
        selectedUsers: [],
        users: [],
        setIsMentioningEveryone: vi.fn(),
        setSelectedUsers: vi.fn(),
        slash: { resetSlash: vi.fn() },
      } as never,
      handlers: { sendQuestion } as never,
      deleteAlert: {} as never,
      messages: [],
      isAgentsPage: false,
      readAloudStop: vi.fn(),
      onConversationCreated,
    }),
  );
}

describe('useChatBoxActions new-conversation promotion', () => {
  it('publishes the created conversation after the first turn starts', async () => {
    const createdConversation = { id: '503', uuid: 'conversation-uuid' };
    const sendQuestion = vi.fn().mockResolvedValue({ success: true, createdConversation });
    const onConversationCreated = vi.fn<OnConversationCreated>();
    const { result } = renderActions(sendQuestion, onConversationCreated);

    act(() => result.current.handleSend('first turn'));

    await waitFor(() => expect(onConversationCreated).toHaveBeenCalledWith(createdConversation));
  });

  it('publishes nothing when a failed turn created nothing', async () => {
    const sendQuestion = vi.fn().mockResolvedValue({ success: false });
    const onConversationCreated = vi.fn<OnConversationCreated>();
    const { result } = renderActions(sendQuestion, onConversationCreated);

    act(() => result.current.handleSend('failed turn'));

    await waitFor(() => expect(sendQuestion).toHaveBeenCalledTimes(1));
    expect(onConversationCreated).not.toHaveBeenCalled();
  });

  /**
   * The gate used to be `result.success && result.createdConversation`, which
   * is the wrong conjunction: the conversation is POSTed before any transport
   * is tried, so a refused turn still leaves a committed row. Dropping it there
   * left the route on `/chat` and the conversation rail's cached listing stale
   * — a conversation in the database and on no screen.
   */
  it('publishes the conversation a failed turn nevertheless created', async () => {
    const createdConversation = { id: '504', uuid: 'conversation-uuid-504' };
    const sendQuestion = vi.fn().mockResolvedValue({ success: false, createdConversation });
    const onConversationCreated = vi.fn<OnConversationCreated>();
    const { result } = renderActions(sendQuestion, onConversationCreated);

    act(() => result.current.handleSend('refused turn'));

    await waitFor(() => expect(onConversationCreated).toHaveBeenCalledWith(createdConversation));
  });
});

/**
 * issue 974: the read-out stops when the answer being read stops being the answer.
 *
 * `readAloudStop` was called from `handleClear` alone, so deleting the message
 * that was being read, regenerating it, or asking the next question all left
 * the old voice reading text the transcript had already replaced — over the
 * top of the new turn. The reference SPA stops TTS in all four places
 * (`onSendMessage`, `onRegenerateAnswer`, `useDeleteMessageAlert`'s
 * `onStopTTS`, and the clear handler); upstream bug
 * EliteaAI/elitea_issues#4995 is the delete half.
 */
describe('issue 974: read-aloud stops when the answer it is reading goes away', () => {
  function renderWithStop(readAloudStop: () => void, extras: {
    regenerateAnswer?: (messageId: string) => void;
    openDialog?: (messageId: string) => void;
  } = {}) {
    return renderHook(() =>
      useChatBoxActions({
        chatInputRef: { current: { reset: vi.fn(), setValue: vi.fn() } },
        data: {
          hasPendingHitlInterrupt: false,
          attachments: { state: { attachments: [], onClearAttachments: vi.fn() } },
        } as never,
        state: {
          isActiveParticipantBroken: false,
          isMentioningEveryone: false,
          selectedUsers: [],
          users: [],
          setIsMentioningEveryone: vi.fn(),
          setSelectedUsers: vi.fn(),
          slash: { resetSlash: vi.fn() },
        } as never,
        handlers: {
          sendQuestion: vi.fn().mockResolvedValue({ success: true }),
          regenerateAnswer: extras.regenerateAnswer ?? vi.fn(),
        } as never,
        deleteAlert: { openDialog: extras.openDialog ?? vi.fn() } as never,
        messages: [],
        isAgentsPage: false,
        readAloudStop,
        onConversationCreated: vi.fn(),
      }),
    );
  }

  it('stops the voice when the answer is regenerated', () => {
    const readAloudStop = vi.fn<() => void>();
    const regenerateAnswer = vi.fn<(messageId: string) => void>();
    const { result } = renderWithStop(readAloudStop, { regenerateAnswer });

    act(() => result.current.handleRegenerate('message-1'));

    expect(readAloudStop, 'the replaced answer must stop being read').toHaveBeenCalledTimes(1);
    expect(regenerateAnswer).toHaveBeenCalledWith('message-1');
  });

  it('stops the voice when the answer is being deleted', () => {
    const readAloudStop = vi.fn<() => void>();
    const openDialog = vi.fn<(messageId: string) => void>();
    const { result } = renderWithStop(readAloudStop, { openDialog });

    act(() => result.current.handleDeleteAnswer('message-2'));

    expect(readAloudStop).toHaveBeenCalledTimes(1);
    expect(openDialog, 'the confirmation still opens').toHaveBeenCalledWith('message-2');
  });

  it('stops the voice when the next question is sent', () => {
    const readAloudStop = vi.fn<() => void>();
    const { result } = renderWithStop(readAloudStop);

    act(() => result.current.handleSend('the next question'));

    expect(readAloudStop).toHaveBeenCalledTimes(1);
  });
});

/**
 * issue 980 / onetest ELITEA-0540: an UNCHANGED save is a retry, and a retry
 * must not ask the platform to rewrite the question.
 *
 * The regeneration contract refuses a non-empty `updated_items` outright
 * (`!emptyJSONArray(body.UpdatedItems)`, internal/api/v2/agentexecution/
 * route.go), so sending the same text back as an "edit" collects a 400 and the
 * retry the case describes never runs.
 */
describe('issue 980: an unchanged save retries rather than rewrites', () => {
  function renderWithMessages(regenerateAnswer: (id: string, items?: unknown) => void) {
    return renderHook(() =>
      useChatBoxActions({
        chatInputRef: { current: { reset: vi.fn(), setValue: vi.fn() } },
        data: {
          hasPendingHitlInterrupt: false,
          setChatHistory: vi.fn(),
          attachments: { state: { attachments: [], onClearAttachments: vi.fn() } },
        } as never,
        state: {
          isActiveParticipantBroken: false,
          isMentioningEveryone: false,
          selectedUsers: [],
          users: [],
          setIsMentioningEveryone: vi.fn(),
          setSelectedUsers: vi.fn(),
          slash: { resetSlash: vi.fn() },
        } as never,
        handlers: { regenerateAnswer, sendQuestion: vi.fn() } as never,
        deleteAlert: {} as never,
        messages: [
          { id: 'q1', role: 'user', content: 'the question' },
          { id: 'a1', role: 'assistant', content: 'the answer', questionId: 'q1' },
        ] as never,
        isAgentsPage: false,
        readAloudStop: vi.fn(),
        onConversationCreated: vi.fn(),
      }),
    );
  }

  it('sends NO updated items when the text is unchanged', () => {
    const regenerateAnswer = vi.fn<(id: string, items?: unknown) => void>();
    const { result } = renderWithMessages(regenerateAnswer);

    act(() =>
      result.current.handleSubmitEditedMessage('q1', [
        { content: 'the question', item_type: 'text_message' },
      ]),
    );

    expect(regenerateAnswer).toHaveBeenCalledTimes(1);
    expect(regenerateAnswer.mock.calls[0]?.[0]).toBe('a1');
    expect(
      regenerateAnswer.mock.calls[0]?.length,
      'a retry passes the answer id alone — anything else asks for a rewrite the contract refuses',
    ).toBe(1);
  });

  it('sends the updated items when the text really changed', () => {
    const regenerateAnswer = vi.fn<(id: string, items?: unknown) => void>();
    const { result } = renderWithMessages(regenerateAnswer);

    act(() =>
      result.current.handleSubmitEditedMessage('q1', [
        { content: 'a different question', item_type: 'text_message' },
      ]),
    );

    expect(regenerateAnswer).toHaveBeenCalledTimes(1);
    expect(regenerateAnswer.mock.calls[0]?.[1], 'a real edit must carry its new text').toEqual([
      { content: 'a different question', item_type: 'text_message' },
    ]);
  });
});
