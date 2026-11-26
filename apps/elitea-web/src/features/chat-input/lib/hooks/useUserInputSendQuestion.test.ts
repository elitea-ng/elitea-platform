/** #3077: Enter pressed again before the input re-renders sends the text once. */
import { act, renderHook } from '@testing-library/react';
import { useState } from 'react';
import { describe, expect, it, vi } from 'vitest';

import { useUserInputSendQuestion } from './useUserInputSendQuestion.hooks';

function useInput(onSend: (question: string, inputContent: string) => void, initial: string) {
  const [question, setQuestion] = useState(initial);
  const send = useUserInputSendQuestion({
    question,
    inputContent: question,
    disabledSend: false,
    clearInputAfterSend: true,
    onSend,
    setQuestion,
    setInputContent: () => undefined,
    setShowExpandIcon: () => undefined,
  });
  return { send, setQuestion };
}

describe('useUserInputSendQuestion', () => {
  it('sends once when it is called several times in the same tick', () => {
    const onSend = vi.fn();
    const { result } = renderHook(() => useInput(onSend, 'fix the build'));
    const { send } = result.current;
    act(() => {
      send();
      send();
      send();
    });
    expect(onSend).toHaveBeenCalledTimes(1);
    expect(onSend).toHaveBeenCalledWith('fix the build', 'fix the build');
  });

  it('sends the text again when the parent puts it back in the same batch as the clear', async () => {
    let restore: ((value: string) => void) | undefined;
    const onSend = vi.fn((question: string) => restore?.(question));
    const { result } = renderHook(() => {
      const input = useInput(onSend, 'retry me');
      restore = input.setQuestion;
      return input;
    });
    act(() => result.current.send());
    await act(() => new Promise((resolve) => { setTimeout(resolve, 0); }));
    act(() => result.current.send());
    expect(onSend).toHaveBeenCalledTimes(2);
  });

  it('sends the same text again once it was typed again', () => {
    const onSend = vi.fn();
    const { result } = renderHook(() => useInput(onSend, 'yes'));
    act(() => result.current.send());
    act(() => result.current.setQuestion('yes'));
    act(() => result.current.send());
    expect(onSend).toHaveBeenCalledTimes(2);
  });
});
