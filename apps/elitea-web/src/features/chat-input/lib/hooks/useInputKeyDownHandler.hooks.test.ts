import { act, renderHook } from '@testing-library/react';
import type { KeyboardEvent } from 'react';
import { describe, expect, it } from 'vitest';

import { useNewInputKeyDownHandler, useNewStartConversationInputKeyDownHandler } from './useInputKeyDownHandler.hooks';

function keyEvent(key: string, target: { value: string; selectionStart: number; selectionEnd: number }): KeyboardEvent<HTMLDivElement> {
  return { key, target } as unknown as KeyboardEvent<HTMLDivElement>;
}

describe('useNewInputKeyDownHandler', () => {
  it('starts hashtag processing on "#" and accumulates printable characters', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.isProcessingSymbols).toBe(true);
    expect(result.current.query).toBe('#');

    act(() => result.current.onKeyDown(keyEvent('a', { value: '#a', selectionStart: 2, selectionEnd: 2 })));
    expect(result.current.query).toBe('#a');
  });

  it('resets hashtag processing when Backspace deletes the "#" itself', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    act(() => result.current.onKeyDown(keyEvent('Backspace', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.isProcessingSymbols).toBe(false);
    expect(result.current.query).toBe('');
  });

  it('records the "#" anchor and ends the picker query on a space (#6774)', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: 'see #', selectionStart: 4, selectionEnd: 4 })));
    expect(result.current.hashAnchorRef.current).toBe(4);
    act(() => result.current.onKeyDown(keyEvent('1', { value: 'see #1', selectionStart: 6, selectionEnd: 6 })));
    act(() => result.current.onKeyDown(keyEvent(' ', { value: 'see #1 ', selectionStart: 7, selectionEnd: 7 })));
    expect(result.current.isProcessingSymbols).toBe(false);
    expect(result.current.query).toBe('');
    expect(result.current.hashAnchorRef.current).toBeNull();
  });

  it('ends the "#" picker query on Enter so the message can be sent (#6774)', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    act(() => result.current.onKeyDown(keyEvent('Enter', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.isProcessingSymbols).toBe(false);
  });

  it('starts @-mention processing on "@" and tracks the anchor position', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('@', { value: 'hi @', selectionStart: 4, selectionEnd: 4 })));
    expect(result.current.isProcessingAtSymbol).toBe(true);
    expect(result.current.atQuery).toBe('@');
    expect(result.current.atAnchorRef.current).toBe(4);
  });

  it('resets @-mention processing on a space', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('@', { value: '@', selectionStart: 1, selectionEnd: 1 })));
    act(() => result.current.onKeyDown(keyEvent(' ', { value: '@ ', selectionStart: 2, selectionEnd: 2 })));
    expect(result.current.isProcessingAtSymbol).toBe(false);
  });

  it('does nothing when disableHashtagDetection is set', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler({ disableHashtagDetection: true }));
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.isProcessingSymbols).toBe(false);
  });

  it('stopProcessingSymbols/stopProcessingAtSymbol reset state on demand', () => {
    const { result } = renderHook(() => useNewInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    act(() => result.current.stopProcessingSymbols());
    expect(result.current.isProcessingSymbols).toBe(false);

    act(() => result.current.onKeyDown(keyEvent('@', { value: '@', selectionStart: 1, selectionEnd: 1 })));
    act(() => result.current.stopProcessingAtSymbol());
    expect(result.current.isProcessingAtSymbol).toBe(false);
    expect(result.current.atAnchorRef.current).toBeNull();
  });
});

describe('useNewStartConversationInputKeyDownHandler', () => {
  it('starts and accumulates hashtag processing, resetting on empty query', () => {
    const { result } = renderHook(() => useNewStartConversationInputKeyDownHandler());
    act(() => result.current.onKeyDown(keyEvent('#', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.isProcessingSymbols).toBe(true);

    act(() => result.current.onKeyDown(keyEvent('Backspace', { value: '#', selectionStart: 1, selectionEnd: 1 })));
    expect(result.current.query).toBe('');
    expect(result.current.isProcessingSymbols).toBe(false);
  });
});
