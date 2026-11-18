import { act, fireEvent, screen } from '@testing-library/react';
import { useEffect, useState } from 'react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { clearNamespace } from '@/shared/lib/storage';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { MAX_DRAFTS, carryDraft, readDraft, rememberDraftAlias, writeDraft } from '@/shared/lib/composerDrafts';
import { UserInput } from './UserInput';

const textarea = (): HTMLTextAreaElement => screen.getByTestId('chat-message-input');

function Composer({ draftKey }: { readonly draftKey: string | undefined }): React.JSX.Element {
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

  it('keeps text typed before the draft key exists, instead of discarding it when the key arrives (#2221)', async () => {
    // The chat page has no key until its project resolves; a fast typist's text must survive the key arriving.
    const user = userEvent.setup();
    const { rerender } = renderWithTheme(<Composer draftKey={undefined} />);
    await user.type(textarea(), 'typed first');
    rerender(<Composer draftKey="chat:1:new" />);
    expect(textarea()).toHaveValue('typed first');
    rerender(<Composer draftKey="chat:1:new" />);
    act(() => {
      rerender(<Composer draftKey="chat:1:other" />);
    });
    expect(readDraft('chat:1:new')).toBe('typed first');
  });

  it('still restores the saved draft when the composer opens empty and the key arrives', () => {
    writeDraft('chat:1:a', 'saved earlier');
    const { rerender } = renderWithTheme(<Composer draftKey={undefined} />);
    rerender(<Composer draftKey="chat:1:a" />);
    expect(textarea()).toHaveValue('saved earlier');
  });

  it('a uuid deep link keeps text typed before the conversation loads, and shares its draft with the numeric id (#2229)', async () => {
    const user = userEvent.setup();
    // The URL carries the uuid; the key is the uuid's from the first render and the conversation's numeric id arrives on load.
    const { rerender, unmount } = renderWithTheme(<Composer draftKey="chat:1:u-abc" />);
    await user.type(textarea(), 'typed before load');
    act(() => rememberDraftAlias('chat:1:u-abc', 'chat:1:12'));
    rerender(<Composer draftKey="chat:1:u-abc" />);
    expect(textarea()).toHaveValue('typed before load');
    unmount();
    expect(readDraft('chat:1:12')).toBe('typed before load');
    expect(readDraft('chat:1:u-abc')).toBe('typed before load');
    // A numeric deep link to the same conversation finds the text.
    renderWithTheme(<Composer draftKey="chat:1:12" />);
    expect(textarea()).toHaveValue('typed before load');
  });

  it('a draft saved under the numeric id shows on an open uuid link once the id pair is registered (#2231)', () => {
    writeDraft('chat:1:12', 'saved before the alias existed');
    renderWithTheme(<Composer draftKey="chat:1:u-abc" />);
    expect(textarea()).toHaveValue('');
    act(() => rememberDraftAlias('chat:1:u-abc', 'chat:1:12'));
    expect(textarea()).toHaveValue('saved before the alias existed');
  });

  it('registering the id pair does not bring an old draft back into a box the person emptied (#2231)', async () => {
    const user = userEvent.setup();
    writeDraft('chat:1:14', 'old numeric draft');
    renderWithTheme(<Composer draftKey="chat:1:u-ghi" />);
    await user.type(textarea(), 'sent soon');
    await user.clear(textarea());
    act(() => rememberDraftAlias('chat:1:u-ghi', 'chat:1:14'));
    expect(textarea()).toHaveValue('');
  });

  it('registering the id pair leaves text typed meanwhile alone (#2231)', async () => {
    const user = userEvent.setup();
    writeDraft('chat:1:13', 'older numeric draft');
    renderWithTheme(<Composer draftKey="chat:1:u-def" />);
    await user.type(textarea(), 'typing now');
    act(() => rememberDraftAlias('chat:1:u-def', 'chat:1:13'));
    expect(textarea()).toHaveValue('typing now');
  });

  it('a numeric deep link and a later uuid one share the draft, and an older uuid draft moves to the numeric id (#2229)', () => {
    writeDraft('chat:1:u-old', 'saved by the uuid form');
    writeDraft('chat:1:13', 'kept numeric');
    rememberDraftAlias('chat:1:u-old', 'chat:1:12');
    rememberDraftAlias('chat:1:u-13', 'chat:1:13');
    expect(readDraft('chat:1:12')).toBe('saved by the uuid form');
    renderWithTheme(<Composer draftKey="chat:1:u-13" />);
    expect(textarea()).toHaveValue('kept numeric');
  });

  it('registering the alias keeps the newer text when both forms hold a draft (#2229)', () => {
    vi.useFakeTimers();
    vi.setSystemTime(1_000);
    writeDraft('chat:1:12', 'older numeric');
    vi.setSystemTime(2_000);
    writeDraft('chat:1:u-abc', 'typed just now');
    rememberDraftAlias('chat:1:u-abc', 'chat:1:12');
    expect(readDraft('chat:1:12')).toBe('typed just now');
  });

  it('the logout sweep clears drafts and the id aliases (#2229)', () => {
    writeDraft('chat:1:12', 'private');
    rememberDraftAlias('chat:1:u-abc', 'chat:1:12');
    expect(Object.keys(window.localStorage).filter((key) => key.includes('composer'))).toEqual(expect.arrayContaining(['el.composer.drafts', 'el.composer.draftAliases']));
    clearNamespace();
    expect(Object.keys(window.localStorage).filter((key) => key.includes('composer'))).toEqual([]);
    expect(readDraft('chat:1:u-abc')).toBe('');
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
