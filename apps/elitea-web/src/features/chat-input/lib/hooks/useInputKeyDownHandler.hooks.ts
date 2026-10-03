import type { KeyboardEvent, RefObject } from 'react';
import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/
 * useInputKeyDownHandler.hooks.js` (unit C3, "chat-input" cluster).
 *
 * Pure state-tracking hooks, NO popup-rendering — confirmed against the
 * baseline itself: neither `UserInput.jsx` nor `NewChatInput.jsx` calls
 * either hook; they are consumed one layer up, in `ChatBox.jsx`/
 * `NewConversationView.jsx`-equivalent future composition-root units,
 * which render the actual slash/mention suggestion popups keyed off the
 * `isProcessingSymbols`/`query`/`isProcessingAtSymbol`/`atQuery` state this
 * hook tracks. Exported from this slice's public barrel for that future
 * consumer, per this unit's own task brief.
 */
export interface UseNewInputKeyDownHandlerOptions {
  readonly disableHashtagDetection?: boolean;
}

export interface UseNewInputKeyDownHandlerResult {
  readonly onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
  readonly isProcessingSymbols: boolean;
  readonly query: string;
  readonly stopProcessingSymbols: () => void;
  /** Caret offset of the "#" that opened the agent/pipeline picker, or `null` when no picker is open. */
  readonly hashAnchorRef: RefObject<number | null>;
  readonly isProcessingAtSymbol: boolean;
  readonly atQuery: string;
  readonly stopProcessingAtSymbol: () => void;
  readonly atAnchorRef: RefObject<number | null>;
}

const NEW_SPECIAL_SYMBOLS = '#';
const AT_SYMBOL = '@';
const PRINTABLE_ASCII_REGEX = /^[\x20-\x7E]*$/;

interface InputTarget {
  readonly selectionStart: number | null;
  readonly selectionEnd: number | null;
  readonly value: string;
}

function eventTarget(event: KeyboardEvent<HTMLDivElement>): InputTarget {
  return event.target as unknown as InputTarget;
}

/**
 * `#`/`@`-mode backspace-or-delete resolution shared by both trigger modes
 * below. Baseline (`useNewInputKeyDownHandler`'s inline duplicate branches
 * for `#` and `@`): with an active selection, "will this deletion remove
 * the trigger" is `selectedText.includes(queryText)`; with a collapsed
 * caret, it is "the one character about to be removed is the trigger
 * character AND the query is down to just that character" — the
 * baseline's separate "`queryText.length === 0` -> true" fallback is
 * subsumed here (`''.includes('')` is always `true`, so it is redundant
 * for the selection branch and reproduced for the collapsed-caret branch
 * for parity).
 */
function resolveDeletion(
  target: InputTarget,
  queryText: string,
  isBackspace: boolean,
  triggerChar: string,
): boolean {
  const { selectionStart, selectionEnd, value } = target;
  const start = selectionStart ?? 0;
  const end = selectionEnd ?? 0;

  if (start !== end) {
    const selectedText = value.substring(start, end);
    return selectedText.includes(queryText);
  }
  if (isBackspace) {
    const charToDelete = start > 0 ? value[start - 1] : '';
    return (charToDelete === triggerChar && queryText.length === 1) || queryText.length === 0;
  }
  const charToDelete = start < value.length ? value[start] : '';
  return (charToDelete === triggerChar && queryText.length === 1) || queryText.length === 0;
}

export function useNewInputKeyDownHandler(
  options: UseNewInputKeyDownHandlerOptions = {},
): UseNewInputKeyDownHandlerResult {
  const { disableHashtagDetection = false } = options;

  const [isProcessingSymbols, setIsProcessingSymbols] = useState(false);
  const [query, setQuery] = useState('');
  const hashAnchorRef = useRef<number | null>(null);

  const [isProcessingAtSymbol, setIsProcessingAtSymbol] = useState(false);
  const [atQuery, setAtQuery] = useState('');
  const atAnchorRef = useRef<number | null>(null);

  const reset = useCallback(() => {
    setIsProcessingSymbols(false);
    setQuery('');
    hashAnchorRef.current = null;
  }, []);

  const resetAt = useCallback(() => {
    setIsProcessingAtSymbol(false);
    setAtQuery('');
    atAnchorRef.current = null;
  }, []);

  const handleAtSymbolMode = useCallback(
    (event: KeyboardEvent<HTMLDivElement>): void => {
      const target = eventTarget(event);
      if (event.key.length === 1 && PRINTABLE_ASCII_REGEX.test(event.key)) {
        if (event.key === ' ') resetAt();
        else setAtQuery((prev) => prev + event.key);
      } else if (event.key === 'Backspace' || event.key === 'Delete') {
        const willDeleteAt = resolveDeletion(target, atQuery, event.key === 'Backspace', AT_SYMBOL);
        if (willDeleteAt) {
          resetAt();
          return;
        }
        setAtQuery((prev) => (prev.length > 1 ? prev.slice(0, -1) : prev));
      } else if (event.key === 'Escape') {
        resetAt();
      }
    },
    [atQuery, resetAt],
  );

  /*
   * "#" opens the agent/pipeline picker. It must never trap the composer
   * (#6774): a space or Enter ends the picker query, the same way a space
   * ends an "@" query, so "issue #12 is fixed" stays an ordinary message.
   */
  const handleHashSymbolMode = useCallback(
    (event: KeyboardEvent<HTMLDivElement>): void => {
      const target = eventTarget(event);
      if (event.key === ' ' || event.key === 'Enter') {
        reset();
      } else if (event.key.length === 1 && PRINTABLE_ASCII_REGEX.test(event.key)) {
        setQuery((prev) => prev + event.key);
      } else if (event.key === 'Backspace' || event.key === 'Delete') {
        const willDeleteQuery = resolveDeletion(target, query, event.key === 'Backspace', '#');
        if (willDeleteQuery) {
          reset();
          return;
        }
        setQuery((prev) => (prev.length > 1 ? prev.slice(0, -1) : prev));
      } else if (event.key === 'Escape') {
        reset();
      }
    },
    [query, reset],
  );

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (disableHashtagDetection) return;

      if (isProcessingAtSymbol) {
        handleAtSymbolMode(event);
        return;
      }
      if (isProcessingSymbols) {
        handleHashSymbolMode(event);
        return;
      }

      if (event.key === AT_SYMBOL) {
        setIsProcessingAtSymbol(true);
        setAtQuery(AT_SYMBOL);
        atAnchorRef.current = eventTarget(event).selectionStart ?? null;
      } else if (event.key.length === 1 && NEW_SPECIAL_SYMBOLS.includes(event.key)) {
        setIsProcessingSymbols(true);
        setQuery(event.key);
        hashAnchorRef.current = eventTarget(event).selectionStart ?? null;
      }
    },
    [disableHashtagDetection, isProcessingAtSymbol, isProcessingSymbols, handleAtSymbolMode, handleHashSymbolMode],
  );

  return {
    onKeyDown,
    isProcessingSymbols,
    query,
    stopProcessingSymbols: reset,
    hashAnchorRef,
    isProcessingAtSymbol,
    atQuery,
    stopProcessingAtSymbol: resetAt,
    atAnchorRef,
  };
}

export interface UseNewStartConversationInputKeyDownHandlerResult {
  readonly onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
  readonly isProcessingSymbols: boolean;
  readonly query: string;
  readonly stopProcessingSymbols: () => void;
}

export function useNewStartConversationInputKeyDownHandler(
  options: UseNewInputKeyDownHandlerOptions = {},
): UseNewStartConversationInputKeyDownHandlerResult {
  const { disableHashtagDetection = false } = options;
  const [isProcessingSymbols, setIsProcessingSymbols] = useState(false);
  const [query, setQuery] = useState('');

  const reset = useCallback(() => {
    setIsProcessingSymbols(false);
    setQuery('');
  }, []);

  const onKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      if (disableHashtagDetection) return;

      if (!isProcessingSymbols && event.key.length === 1 && NEW_SPECIAL_SYMBOLS.includes(event.key)) {
        setIsProcessingSymbols(true);
        setQuery(event.key);
      } else if (isProcessingSymbols) {
        if (event.key.length === 1 && PRINTABLE_ASCII_REGEX.test(event.key)) {
          setQuery((prev) => prev + event.key);
        } else if (event.key === 'Backspace') {
          setQuery((prev) => prev.slice(0, -1));
        }
      }
    },
    [isProcessingSymbols, disableHashtagDetection],
  );

  useEffect(() => {
    if (!query) reset();
  }, [query, reset]);

  return { onKeyDown, isProcessingSymbols, query, stopProcessingSymbols: reset };
}
