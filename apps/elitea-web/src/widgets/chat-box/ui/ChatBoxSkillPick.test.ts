import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { startSkillMention } from './ChatBoxSkillPick';

/** A text area's handle: the text and caret are the test's, `insertTextAtCursor` edits them like the real one. */
function fakeHandle(initial: string, caret: number) {
  let text = initial;
  let position = caret;
  return {
    focus: vi.fn(),
    getInputContent: () => text,
    getCursorPosition: () => position,
    replaceRange: vi.fn(),
    insertTextAtCursor: vi.fn((insert: string) => {
      text = text.slice(0, position) + insert + text.slice(position);
      position += insert.length;
    }),
  };
}

describe('startSkillMention (#2017)', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it('inserts "~" into an empty composer and tells the mention hooks after the commit', () => {
    const handle = fakeHandle('', 0);
    const onInputChange = vi.fn();

    startSkillMention(handle, onInputChange);

    expect(handle.focus).toHaveBeenCalledTimes(1);
    expect(handle.insertTextAtCursor).toHaveBeenCalledWith('~');
    expect(onInputChange).not.toHaveBeenCalled();
    vi.runAllTimers();
    expect(onInputChange).toHaveBeenCalledWith('~');
  });

  it('puts a space first when the caret follows a word, so the "~" can open the picker', () => {
    const handle = fakeHandle('summarise', 9);
    const onInputChange = vi.fn();

    startSkillMention(handle, onInputChange);
    vi.runAllTimers();

    expect(handle.insertTextAtCursor).toHaveBeenCalledWith(' ~');
    expect(onInputChange).toHaveBeenCalledWith('summarise ~');
  });

  it('adds no space after whitespace', () => {
    const handle = fakeHandle('use ', 4);
    startSkillMention(handle, vi.fn());
    expect(handle.insertTextAtCursor).toHaveBeenCalledWith('~');
  });

  it('does nothing without a composer', () => {
    const onInputChange = vi.fn();
    startSkillMention(null, onInputChange)();
    vi.runAllTimers();
    expect(onInputChange).not.toHaveBeenCalled();
  });

  it('takes focus again after the menu that was clicked has closed', () => {
    const handle = fakeHandle('', 0);
    startSkillMention(handle, vi.fn());
    expect(handle.focus).toHaveBeenCalledTimes(1);
    vi.runAllTimers();
    expect(handle.focus).toHaveBeenCalledTimes(2);
  });

  it('announces nothing once cancelled (the composer unmounted)', () => {
    const handle = fakeHandle('', 0);
    const onInputChange = vi.fn();
    const cancel = startSkillMention(handle, onInputChange);
    cancel();
    vi.runAllTimers();
    expect(onInputChange).not.toHaveBeenCalled();
    expect(handle.focus).toHaveBeenCalledTimes(1);
  });

  it('reads the content when the timer fires, not when the insert was made', () => {
    const handle = fakeHandle('', 0);
    const onInputChange = vi.fn();
    startSkillMention(handle, onInputChange);
    handle.insertTextAtCursor('x');
    vi.runAllTimers();
    expect(onInputChange).toHaveBeenCalledWith('~x');
  });
});
