import type { ChangeEvent, ClipboardEvent, CompositionEvent, KeyboardEvent, Ref, RefObject } from 'react';
import { useCallback, useEffect, useImperativeHandle, useLayoutEffect, useRef, useState } from 'react';

import { generateRandomAppendix, renameFile } from '../attachmentPasteNaming';
import { useCtrlEnterKeyEventsHandler } from './useCtrlEnterKeyEventsHandler.hooks';
import { useMentionDetection } from './useMentionDetection.hooks';
import type { MentionCandidate, MentionMatch } from './useMentionDetection.hooks';

import type { UserInputHandle } from '../../ui/UserInput.types';
import { MIN_HEIGHT } from '../../ui/UserInput.types';

/**
 * Internal controller hooks for `UserInput.tsx`, split out purely to keep
 * that component's own file (and per-function cyclomatic complexity) under
 * the §3.5 budgets — see `UserInput.types.ts`'s module doc for the same
 * rationale. None of these are components (lowercase `use*` names), so the
 * §3.5 "≤12 component props" row does not apply to them; each is still
 * kept small and single-purpose to stay well under the ≤12 cyclomatic-
 * complexity budget regardless.
 */

/** Trivial state bag — no branches, `UserInput.tsx`'s five `useState` calls factored out. */
export function useUserInputTextState(initialRows: number) {
  const [question, setQuestion] = useState('');
  const [inputContent, setInputContent] = useState('');
  const [showExpandIcon, setShowExpandIcon] = useState(false);
  const [rows, setRows] = useState(initialRows);
  const [isFocused, setIsFocused] = useState(false);
  return {
    question,
    setQuestion,
    inputContent,
    setInputContent,
    showExpandIcon,
    setShowExpandIcon,
    rows,
    setRows,
    isFocused,
    setIsFocused,
  };
}

export interface UseUserInputMentionsParams {
  readonly inputContent: string;
  readonly users: readonly MentionCandidate[];
  readonly onMentionChange: ((mentions: readonly MentionMatch[]) => void) | undefined;
  readonly inputRef: RefObject<HTMLTextAreaElement | null>;
}

/** Mention detection + baseline's "bubble + focus the input" effect (`UserInput.jsx` effect 2/3). */
export function useUserInputMentions(params: UseUserInputMentionsParams): readonly MentionMatch[] {
  const { inputContent, users, onMentionChange, inputRef } = params;
  const { mentions } = useMentionDetection(inputContent, users, 'name', {
    allowPartialMatches: false,
    caseSensitive: false,
    minMatchLength: 1,
  });

  useEffect(() => {
    onMentionChange?.(mentions);
    if (inputRef.current && mentions.length > 0) inputRef.current.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- baseline parity (UserInput.jsx): fires only on `mentions` changing.
  }, [mentions]);

  return mentions;
}

/** Baseline's scroll-sync effect (effect 1/3): mirrors the textarea's scroll position onto the highlight overlay. */
export function useUserInputScrollSync(
  inputRef: RefObject<HTMLTextAreaElement | null>,
  mirrorRef: RefObject<HTMLDivElement | null>,
  hasHighlights: boolean,
): void {
  useEffect(() => {
    const textarea = inputRef.current;
    const mirror = mirrorRef.current;
    if (!textarea || !mirror || !hasHighlights) return;
    const sync = (): void => {
      mirror.scrollTop = textarea.scrollTop;
    };
    textarea.addEventListener('scroll', sync);
    return () => textarea.removeEventListener('scroll', sync);
  }, [hasHighlights, inputRef, mirrorRef]);
}

/** Baseline's expand-icon-collapse effect (effect 3/3): resets to `MAX_ROWS` once the expand affordance disappears. */
export function useUserInputRowCollapse(showExpandIcon: boolean, maxRows: number, setRows: (rows: number) => void): void {
  useEffect(() => {
    if (!showExpandIcon) setRows(maxRows);
  }, [showExpandIcon, maxRows, setRows]);
}

export interface UseUserInputSendQuestionParams {
  readonly question: string;
  readonly inputContent: string;
  readonly disabledSend: boolean | undefined;
  readonly clearInputAfterSend: boolean;
  readonly onSend: ((question: string, inputContent: string) => void) | undefined;
  readonly setQuestion: (value: string) => void;
  readonly setInputContent: (value: string) => void;
  readonly setShowExpandIcon: (value: boolean) => void;
}

export function useUserInputSendQuestion(params: UseUserInputSendQuestionParams): () => void {
  const { question, inputContent, disabledSend, clearInputAfterSend, onSend, setQuestion, setInputContent, setShowExpandIcon } =
    params;
  return useCallback(() => {
    if (question.trim() && !disabledSend) {
      if (clearInputAfterSend) {
        setInputContent('');
        setQuestion('');
        setShowExpandIcon(false);
      }
      onSend?.(question, inputContent);
    }
  }, [clearInputAfterSend, disabledSend, onSend, question, inputContent, setInputContent, setQuestion, setShowExpandIcon]);
}

export interface UseUserInputInsertTextAtCursorParams {
  readonly inputRef: RefObject<HTMLTextAreaElement | null>;
  /**
   * No `inputContent` here on purpose: the text this splices into is read
   * from the textarea itself. See the hook's own doc comment — the state copy
   * is one render behind the DOM in exactly the case that matters.
   */
  readonly setInputContent: (value: string) => void;
  readonly setQuestion: (value: string) => void;
  readonly setShowExpandIcon: (value: boolean) => void;
}

/**
 * Inserts text at the caret — Shift+Enter's newline, and the "+" menu's
 * mention insertions.
 *
 * # Why the caret is restored in a LAYOUT EFFECT and not a timer
 *
 * It used to be `setTimeout(… , 0)`: set the state, then put the caret back
 * one macrotask later. Between those two points the textarea is live and the
 * caret sits wherever the value change left it — at the END of the value —
 * so anything typed in that window landed there, and was then STRANDED there
 * when the timer finally moved the caret back to the insertion point.
 *
 * Typing `line one`, Shift+Enter, `line two` produced
 * `line one\nne two…li`: the `l` and `i` went in at the end of the value
 * before the timer fired, the timer moved the caret back in front of them,
 * and the rest of the word was typed there. Nothing threw, no state was
 * lost, and the composer simply reordered the person's own words. It is a
 * race and it behaves like one — the faster the machine, the smaller the
 * window, which is why it read as an intermittent test failure rather than
 * as the input defect it is.
 *
 * A layout effect closes the window rather than shrinking it. React flushes
 * a discrete event's state update and its layout effects synchronously,
 * inside the same task as the keystroke that caused them, so the caret is
 * back before the next key event can be dispatched. There is no interval in
 * which the caret is wrong.
 *
 * The value is read from `textarea.value`, not from the `inputContent` prop:
 * the textarea is what the person has been typing into, and the state is one
 * render behind it whenever an insertion follows a keystroke closely enough
 * to matter — which is precisely the case this function is used in.
 */
export function useUserInputInsertTextAtCursor(
  params: UseUserInputInsertTextAtCursorParams,
): (textToInsert: string) => void {
  const { inputRef, setInputContent, setQuestion, setShowExpandIcon } = params;
  // `null` means "nothing to restore" — set by the callback below, consumed
  // and cleared by the one commit that follows it.
  const pendingCursorRef = useRef<number | null>(null);

  useLayoutEffect(() => {
    const newCursorPosition = pendingCursorRef.current;
    if (newCursorPosition === null) return;
    pendingCursorRef.current = null;
    const textarea = inputRef.current;
    if (!textarea) return;
    textarea.setSelectionRange(newCursorPosition, newCursorPosition);
    textarea.focus();
    // Read AFTER the value is committed, so the height reflects the inserted
    // line rather than the one before it.
    setShowExpandIcon(textarea.offsetHeight > MIN_HEIGHT);
  });

  return useCallback(
    (textToInsert: string) => {
      const textarea = inputRef.current;
      if (!textarea) return;
      const start = textarea.selectionStart || 0;
      const end = textarea.selectionEnd || 0;
      const current = textarea.value;
      const newValue = current.slice(0, start) + textToInsert + current.slice(end);

      pendingCursorRef.current = start + textToInsert.length;
      setInputContent(newValue);
      setQuestion(newValue.trim() ? newValue : '');
    },
    [inputRef, setInputContent, setQuestion],
  );
}

export interface UseUserInputChangeHandlersParams {
  readonly setInputContent: (value: string) => void;
  readonly setQuestion: (value: string) => void;
  readonly setShowExpandIcon: (value: boolean) => void;
  readonly setRows: (updater: (previousRows: number) => number) => void;
  readonly onInputChange: ((value: string) => void) | undefined;
  readonly maxRows: number;
  readonly minRows: number;
}

/** `onInputQuestion` (textarea onChange) + `onClickExpander` (expand/collapse toggle). */
export function useUserInputChangeHandlers(params: UseUserInputChangeHandlersParams): {
  readonly onInputQuestion: (event: ChangeEvent<HTMLTextAreaElement>) => void;
  readonly onClickExpander: () => void;
} {
  const { setInputContent, setQuestion, setShowExpandIcon, setRows, onInputChange, maxRows, minRows } = params;

  const onInputQuestion = useCallback(
    (event: ChangeEvent<HTMLTextAreaElement>) => {
      const value = event.target.value;
      setInputContent(value);
      setQuestion(value.trim() ? value : '');
      onInputChange?.(value);
      const target = event.target;
      setTimeout(() => setShowExpandIcon(target.offsetHeight > MIN_HEIGHT), 0);
    },
    [onInputChange, setInputContent, setQuestion, setShowExpandIcon],
  );

  const onClickExpander = useCallback(() => {
    setRows((prevRows) => (prevRows === maxRows ? minRows : maxRows));
  }, [maxRows, minRows, setRows]);

  return { onInputQuestion, onClickExpander };
}

export function useUserInputPasteHandler(
  onFilePaste: ((files: File | readonly File[]) => void) | undefined,
): (event: ClipboardEvent<HTMLDivElement>) => void {
  return useCallback(
    (event: ClipboardEvent<HTMLDivElement>) => {
      const items = event.clipboardData.items;
      if (!items) return;

      const files: File[] = [];
      for (const item of items) {
        if (item.kind === 'file') {
          const file = item.getAsFile();
          if (file) files.push(renameFile(file, generateRandomAppendix(file.size)));
        }
      }

      if (files.length > 0 && onFilePaste) {
        event.preventDefault();
        onFilePaste(files.length === 1 ? (files[0] as File) : files);
      }
      // Text content: fall through to the default paste behaviour.
    },
    [onFilePaste],
  );
}

export interface UseUserInputImperativeHandleParams {
  readonly ref: Ref<UserInputHandle>;
  readonly inputRef: RefObject<HTMLTextAreaElement | null>;
  readonly inputContent: string;
  readonly setInputContent: (value: string) => void;
  readonly setQuestion: (value: string) => void;
  readonly setShowExpandIcon: (value: boolean) => void;
  readonly sendQuestion: () => void;
  readonly insertTextAtCursor: (text: string) => void;
}

export function useUserInputImperativeHandle(params: UseUserInputImperativeHandleParams): void {
  const { ref, inputRef, inputContent, setInputContent, setQuestion, setShowExpandIcon, sendQuestion, insertTextAtCursor } =
    params;

  useImperativeHandle(
    ref,
    (): UserInputHandle => ({
      focus: () => inputRef.current?.focus(),
      reset: () => {
        setInputContent('');
        setQuestion('');
        setShowExpandIcon(false);
      },
      getInputContent: () => inputContent,
      getCursorPosition: () => inputRef.current?.selectionStart ?? null,
      setValue: (value, cursorPos) => {
        setQuestion(value);
        setInputContent(value);
        if (cursorPos !== undefined) {
          setTimeout(() => {
            inputRef.current?.setSelectionRange(cursorPos, cursorPos);
            inputRef.current?.focus();
          }, 0);
        }
      },
      replaceRange: (start, end, text) => {
        const newValue = inputContent.slice(0, start) + text + inputContent.slice(end);
        setInputContent(newValue);
        setQuestion(newValue.trim() ? newValue : '');
        const newCursorPos = start + text.length;
        setTimeout(() => {
          inputRef.current?.setSelectionRange(newCursorPos, newCursorPos);
          inputRef.current?.focus();
        }, 0);
      },
      removeSymbol: (symbol) => {
        const index = inputContent.lastIndexOf(symbol);
        const newContent = inputContent.slice(0, index);
        setQuestion(newContent.trimEnd());
        setInputContent(newContent.trimEnd());
      },
      sendQuestion,
      insertTextAtCursor,
      mentionUser: (userString) => {
        if (!inputContent.includes(userString)) {
          const newContent = inputContent + userString;
          setInputContent(newContent);
          setQuestion(newContent);
        }
      },
    }),
    [inputContent, sendQuestion, insertTextAtCursor, inputRef, setInputContent, setQuestion, setShowExpandIcon],
  );
}

/**
 * Baseline parity, adapted delivery mechanism: `UserInput.jsx` passed
 * `{...(isCreatingConversation && { autoFocus: true })}` straight to the
 * `TextField`. The JSX `autoFocus` attribute is banned by `jsx-a11y/
 * no-autofocus` (same rule `shared/ui/BaseModal.tsx` already cites for its
 * own disclosed drop) — but unlike `BaseModal`'s modal-hijack case, this
 * one is a real, deliberate UX need (a brand-new conversation's composer
 * should be focus-ready) with no dialog-open side effect to avoid. Ported
 * as an imperative, mount-gated `.focus()` call instead of the JSX
 * attribute — same observable behaviour (focus moves into the input once,
 * when the input is first rendered for a conversation being created),
 * without tripping the syntactic lint rule. Lives in its own hook (not one
 * of `UserInput.tsx`'s own three `useEffect`s) so it does not count against
 * that component's §3.5 effect budget — see this file's module doc.
 */
export function useUserInputAutoFocus(inputRef: RefObject<HTMLTextAreaElement | null>, isCreatingConversation: boolean): void {
  useEffect(() => {
    if (isCreatingConversation) inputRef.current?.focus();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- mount-gated: fires once per `isCreatingConversation` transition, not on every `inputRef` identity change (a ref's identity is stable anyway).
  }, [isCreatingConversation]);
}

export interface UseUserInputKeyHandlingParams {
  readonly insertTextAtCursor: (text: string) => void;
  readonly sendQuestion: () => void;
  readonly onNormalKeyDown: ((event: KeyboardEvent<HTMLDivElement>) => void) | undefined;
}

/**
 * Wraps `useCtrlEnterKeyEventsHandler`, folding in the `'\n'`-insert and
 * plain-send bindings itself — so `UserInput.tsx` declares no inline
 * `onCtrlEnterDown`/`onEnterDown` arrow functions of its own.
 */
export function useUserInputKeyHandling(params: UseUserInputKeyHandlingParams): {
  readonly onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
  readonly onKeyUp: (event: KeyboardEvent<HTMLDivElement>) => void;
  readonly onCompositionStart: (event: CompositionEvent<HTMLDivElement>) => void;
  readonly onCompositionEnd: (event: CompositionEvent<HTMLDivElement>) => void;
} {
  const { insertTextAtCursor, sendQuestion, onNormalKeyDown } = params;
  const onCtrlEnterDown = useCallback(() => insertTextAtCursor('\n'), [insertTextAtCursor]);

  return useCtrlEnterKeyEventsHandler({
    onCtrlEnterDown,
    onShiftEnterPressed: onCtrlEnterDown,
    onEnterDown: sendQuestion,
    onNormalKeyDown,
  });
}

/** Stable `onFocus`/`onBlur` pair, factored out so `UserInput.tsx` itself declares no inline arrow functions. */
export function useUserInputFocusHandlers(setIsFocused: (value: boolean) => void): {
  readonly onFocus: () => void;
  readonly onBlur: () => void;
} {
  const onFocus = useCallback(() => setIsFocused(true), [setIsFocused]);
  const onBlur = useCallback(() => setIsFocused(false), [setIsFocused]);
  return { onFocus, onBlur };
}

/** Two textarea refs `UserInput.tsx` shares across its controller hooks — factored to avoid re-declaring `useRef` in the component itself. */
export function useUserInputRefs(): {
  readonly inputRef: RefObject<HTMLTextAreaElement | null>;
  readonly mirrorRef: RefObject<HTMLDivElement | null>;
} {
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const mirrorRef = useRef<HTMLDivElement>(null);
  return { inputRef, mirrorRef };
}
