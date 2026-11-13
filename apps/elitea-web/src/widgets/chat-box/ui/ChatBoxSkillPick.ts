/**
 * The "+" menu's "Skills" row (#2017): put a `~` into the composer, where the
 * skill mention picker (`useChatSkillMention`) already opens on one.
 *
 * `insertTextAtCursor` writes the text without telling the mention hooks, which
 * learn of an edit through the textarea's `onChange`. So the change is
 * announced here once the insert has committed (the caret and the value are
 * read back from the handle, not computed). A `~` only opens the picker at the
 * start of the text or after whitespace, so a space goes before it when the
 * caret follows a word.
 */
import type { ChatInputHandle } from '@/features/chat-input';

interface SkillPickHandle extends ChatInputHandle {
  focus(): void;
  insertTextAtCursor(text: string): void;
}

const SKILL_PICK_TRIGGER = '~';

export function startSkillMention(handle: SkillPickHandle | null | undefined, onInputChange: (value: string) => void): void {
  if (!handle) return;
  const content = handle.getInputContent();
  const caret = handle.getCursorPosition() ?? content.length;
  const previous = caret > 0 ? content.charAt(caret - 1) : '';
  handle.focus();
  handle.insertTextAtCursor(previous !== '' && !/\s/.test(previous) ? ` ${SKILL_PICK_TRIGGER}` : SKILL_PICK_TRIGGER);
  // After the commit: the layout effect that moves the caret has run by then.
  setTimeout(() => {
    onInputChange(handle.getInputContent());
  }, 0);
}
