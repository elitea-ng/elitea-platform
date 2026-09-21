/**
 * #972 — the conversation must SAY that its agent was withdrawn, and the
 * composer must stop accepting.
 *
 * Two assertions, at the two layers that own them: the notice renders only
 * when the flag is set, and `deriveChatBoxInputState` refuses Send for the same
 * flag. The e2e journey
 * (`e2e/journeys/agents/agents.unpublished-conversation.spec.ts`) proves the
 * flag is really computed from a withdrawn version against a live stack;
 * these pin the two halves it cannot tell apart on a failure.
 */
import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { ChatBoxWithdrawnNotice } from './ChatBoxWithdrawnNotice';
import { deriveChatBoxInputState } from './ChatBox.helpers';

const quietFlags = {
  isLoadingConversation: false,
  isFetchingParticipantDetails: false,
  isUploadingAttachments: false,
  isUpdatingInternalToolsConfig: false,
  isConversationSending: false,
  isStreaming: false,
  hasChatInput: true,
  isProcessingSymbols: false,
  hasPendingHitlInterrupt: false,
  isActiveParticipantBroken: false,
};

describe('a conversation whose agent was withdrawn', () => {
  it('renders nothing while the agent is live', () => {
    const { container } = render(<ChatBoxWithdrawnNotice withdrawn={false} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('tells the reader, as an alert', () => {
    render(<ChatBoxWithdrawnNotice withdrawn />);
    // `role="alert"`, not a silent caption: the state appears without any
    // action of the reader's — somebody else withdrew the agent.
    const notice = screen.getByRole('alert');
    expect(notice).toHaveTextContent(/no longer available/i);
  });

  it('leaves Send enabled for a live agent', () => {
    expect(deriveChatBoxInputState(quietFlags).disabledSend).toBe(false);
  });

  it('refuses Send once the agent is withdrawn', () => {
    // The half that protects the user: a withdrawn agent cannot answer, so the
    // conversation must not accept a message the sender would never get a
    // reply to.
    expect(
      deriveChatBoxInputState({ ...quietFlags, isActiveParticipantWithdrawn: true }).disabledSend,
    ).toBe(true);
  });
});
