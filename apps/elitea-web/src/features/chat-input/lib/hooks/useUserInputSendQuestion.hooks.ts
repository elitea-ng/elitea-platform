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
  // The text just sent, until the input re-renders without it (#3077): the state copy is a render behind, so Enter
  // pressed again in that window would send the same text twice.
  const sent = useRef<string | null>(null);
  useEffect(() => {
    if (question !== sent.current) sent.current = null;
  }, [question]);
  return useCallback(() => {
    if (question.trim() && !disabledSend && sent.current !== question) {
      if (clearInputAfterSend) {
        sent.current = question;
        setInputContent('');
        setQuestion('');
        setShowExpandIcon(false);
      }
      onSend?.(question, inputContent);
    }
  }, [clearInputAfterSend, disabledSend, onSend, question, inputContent, setInputContent, setQuestion, setShowExpandIcon]);
}
