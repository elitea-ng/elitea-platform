import { describe, expect, it, vi } from 'vitest';

import { buildMessageActions } from './ChatBox.messageActions';

describe('buildMessageActions', () => {
  const actions = { onCopyToClipboard: vi.fn(), onDeleteAnswer: vi.fn(), onRegenerateAnswer: vi.fn(), onSubmitEditedMessage: vi.fn() };

  it('passes every action through for an ordinary conversation', () => {
    expect(buildMessageActions(false, actions)).toBe(actions);
  });

  it('keeps copy and delete but drops regenerate and edit-and-resend when read-only', () => {
    expect(buildMessageActions(true, actions)).toEqual({ onCopyToClipboard: actions.onCopyToClipboard, onDeleteAnswer: actions.onDeleteAnswer });
  });
});
