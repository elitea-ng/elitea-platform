/** The composer's send: the text once, also when Enter is pressed again before the input re-renders (#3077). */
import { useCallback, useEffect, useRef } from 'react';

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
  // The text just sent, until the input's next render commits (#3077): the state copy is a render behind, so Enter
  // pressed again in that window would send the same text twice. Released on every commit, and by a timer after the
  // current task (React skips the commit when a parent puts the same text back in the same batch), never on a change
  // of the text, so the same text can always be sent again.
  const sent = useRef<string | null>(null);
  useEffect(() => {
    sent.current = null;
  });
  return useCallback(() => {
    if (question.trim() && !disabledSend && sent.current !== question) {
      if (clearInputAfterSend) {
        sent.current = question;
        setTimeout(() => {
          if (sent.current === question) sent.current = null;
        }, 0);
        setInputContent('');
        setQuestion('');
        setShowExpandIcon(false);
      }
      onSend?.(question, inputContent);
    }
  }, [clearInputAfterSend, disabledSend, onSend, question, inputContent, setInputContent, setQuestion, setShowExpandIcon]);
}
