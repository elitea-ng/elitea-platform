/**
 * One conversation per new chat (#3077). A second send before the page has the first one's uuid (Enter pressed again,
 * a message typed while the create runs) joins the create under way, or the conversation it made, instead of creating
 * another. Forgotten when the chat's uuid changes (the page has it, or a new chat starts) or when the create fails.
 */
import { useCallback, useEffect, useRef } from "react";

export function useOneConversationCreate<T>(
  create: (question: string) => Promise<T | undefined>,
  conversationUuid: string | undefined,
): (question: string) => Promise<T | undefined> {
  const creating = useRef<Promise<T | undefined> | null>(null);
  useEffect(() => {
    creating.current = null;
  }, [conversationUuid]);
  return useCallback(
    (question: string) => {
      const pending = creating.current ?? create(question);
      creating.current = pending;
      void pending.then(
        (created) => {
          if (created === undefined && creating.current === pending) creating.current = null;
        },
        () => {
          if (creating.current === pending) creating.current = null;
        },
      );
      return pending;
    },
    [create],
  );
}
