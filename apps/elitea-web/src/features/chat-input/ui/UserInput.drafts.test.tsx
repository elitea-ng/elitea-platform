import { act, fireEvent, screen } from '@testing-library/react';
import { useEffect, useState } from 'react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { clearNamespace } from '@/shared/lib/storage';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { MAX_DRAFTS, carryDraft, readDraft, writeDraft } from '@/shared/lib/composerDrafts';
import { UserInput } from './UserInput';

const textarea = (): HTMLTextAreaElement => screen.getByTestId('chat-message-input');

function Composer({ draftKey }: { readonly draftKey: string }): React.JSX.Element {
  return <UserInput slots={{}} slotProps={{ field: { draftKey } }} callbacks={{ onSend: () => undefined }} />;
}

beforeEach(() => clearNamespace());
afterEach(() => vi.useRealTimers());

describe('composer drafts', () => {
  it('keeps the unsent text of each conversation across a switch and back', async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<Composer draftKey="chat:1:a" />);
    await user.type(textarea(), 'half a thought');
    rerender(<Composer draftKey="chat:1:b" />);
    // B has nothing of its own: A's text does not follow.
    expect(textarea()).toHaveValue('');
    await user.type(textarea(), 'for b');
    rerender(<Composer draftKey="chat:1:a" />);
    expect(textarea()).toHaveValue('half a thought');
    expect(readDraft('chat:1:b')).toBe('for b');
  });

  it('comes back after the composer is gone (another page, a restart)', async () => {
    const user = userEvent.setup();
    const first = renderWithTheme(<Composer draftKey="chat:1:a" />);
    await user.type(textarea(), 'still here');
    first.unmount();
    renderWithTheme(<Composer draftKey="chat:1:a" />);
    expect(textarea()).toHaveValue('still here');
  });

  it('drops the draft once the text is sent', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    renderWithTheme(<Composer draftKey="chat:1:a" />);
    await user.type(textarea(), 'send me');
    act(() => {
      vi.advanceTimersByTime(400);
    });
    expect(readDraft('chat:1:a')).toBe('send me');
    fireEvent.keyDown(textarea(), { key: 'Enter' });
    act(() => {
      vi.advanceTimersByTime(400);
    });
    expect(readDraft('chat:1:a')).toBe('');
  });

  it("carries a new chat's text to the conversation it becomes", async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<Composer draftKey="chat:1:new" />);
    await user.type(textarea(), 'about the agent');
    carryDraft('chat:1:c9', 'about the agent');
    rerender(<Composer draftKey="chat:1:c9" />);
    expect(textarea()).toHaveValue('about the agent');
    expect(readDraft('chat:1:new')).toBe('');
  });

  it("does not hand a new chat's text to another conversation the person opens", async () => {
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<Composer draftKey="chat:1:new" />);
    await user.type(textarea(), 'not for c1');
    rerender(<Composer draftKey="chat:1:c1" />);
    expect(textarea()).toHaveValue('');
    expect(readDraft('chat:1:new')).toBe('not for c1');
  });

  it('starts a blank new chat when Create switches the key and remounts the chat in one go', async () => {
    // Create: the conversation's key turns into the new chat's, and the parent remounts the chat
    // before the emptied input renders. The leaving composer must not save its text as the new chat's.
    let create = (): void => undefined;
    function Page(): React.JSX.Element {
      const [draftKey, setDraftKey] = useState('chat:1:a');
      const [token, setToken] = useState(0);
      create = () => setDraftKey('chat:1:new');
      useEffect(() => {
        if (draftKey === 'chat:1:new') setToken(1);
      }, [draftKey]);
      return <Composer key={token} draftKey={draftKey} />;
    }
    const user = userEvent.setup();
    renderWithTheme(<Page />);
    await user.type(textarea(), 'left behind');
    act(() => {
      create();
    });
    expect(textarea()).toHaveValue('');
    expect(readDraft('chat:1:new')).toBe('');
    expect(readDraft('chat:1:a')).toBe('left behind');
  });

  it('keeps only the newest drafts', () => {
    for (let index = 0; index <= MAX_DRAFTS; index += 1) {
      vi.setSystemTime(1_000 + index);
      writeDraft(`chat:1:${index}`, `text ${index}`);
    }
    expect(readDraft('chat:1:0')).toBe('');
    expect(readDraft(`chat:1:${MAX_DRAFTS}`)).toBe(`text ${MAX_DRAFTS}`);
  });
});
